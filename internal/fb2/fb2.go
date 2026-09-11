// Package fb2 reads metadata embedded in an FB2 document — the title,
// authors, language, description, and cover most books already carry,
// mirroring internal/epub's surface so the scanner stays ignorant of
// format detail.
package fb2

import (
	"archive/zip"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"

	"library/internal/cover"
	"library/internal/storage"
)

// maxCoverBase64Bytes bounds the copy readCoverBinary accumulates of the
// cover's <binary> character data: the base64 of a cover cover.Store would
// accept, padded to a whole quantum so a cover of exactly MaxCoverBytes is
// admitted here as it is there. One quantum encodes three byte lengths, so
// the exact boundary is drawn on the decoded length afterwards; this only
// bounds the copy. Past it the cover is dropped mid-token and nothing more
// of the element is looked at. It bounds that copy and nothing
// else: encoding/xml buffers a whole text node inside Decoder.text before
// Token returns it, and nothing in the package caps that, so the node itself
// still costs its own size once — maxZipDocumentBytes is what bounds it for
// an archive, and a plain file's size on disk bounds it for a plain .fb2
const maxCoverBase64Bytes = (cover.MaxCoverBytes + 2) / 3 * 4

// maxZipDocumentBytes bounds the .fb2 inside a .fb2.zip, and is in effect the
// largest single text node an archive may inflate to: the walk keeps a
// document of any length from accumulating, but the tokeniser holds one text
// node whole, and a zip entry can inflate one node to whatever size it likes
// for a few kilobytes on disk. The tokeniser's buffer doubles as it grows,
// so the worst case is about twice this transiently — 128 MiB keeps that in
// line with the ~300 MB internal/cover's maxPixels already accepts from a
// progressive JPEG header. A real document whose cover binary sits past this
// much of other illustrations loses its cover and keeps its text. A plain
// .fb2 has no such cap, since it already costs its own size on disk.
//
// It is applied on both sides of the charset decoder (readCappedDocument),
// which is what keeps the figure honest: a decoder only grows a byte count,
// so bounding the archive's bytes alone would let a cp1251 document hold
// twice this and a legacy CJK one three times
const maxZipDocumentBytes = 128 << 20

// errDocumentTooLarge is what a cappedReader returns once its cap is
// reached; readMetadata tells it apart from a parse failure so that a
// document whose text metadata was already read keeps it
var errDocumentTooLarge = errors.New("fb2 document exceeds the size limit")

// Metadata is what's extracted from an FB2 document. Same field set as
// epub.Metadata, deliberately: the scanner's bookMeta already has these
// eight fields and fills them from either source with the same code shape.
type Metadata struct {
	Title         string
	Authors       []string
	Language      string
	ISBN          string
	Description   string
	Publisher     string
	PublishedDate string
	Cover         []byte
}

// description is the <description> element, the one part of an FB2 document
// decoded through a struct. The <binary> elements that follow it are walked
// token by token instead (see readMetadata), so a struct field for them
// would hold every illustration in the book in memory to find one cover
type description struct {
	TitleInfo   titleInfo   `xml:"title-info"`
	PublishInfo publishInfo `xml:"publish-info"`
}

type titleInfo struct {
	BookTitle string      `xml:"book-title"`
	Author    []fb2Author `xml:"author"`
	Lang      string      `xml:"lang"`
	// Annotation holds <p> elements, sometimes with nested markup
	// (<emphasis>, <strong>, ...). paragraph's UnmarshalXML walks the raw
	// token stream to collect character data wherever it occurs, including
	// inside such child elements — a plain string field would not: Go's
	// struct-based XML unmarshaling skips a child element entirely (tag
	// and text both) when no field claims it, so "<p>A <emphasis>great</
	// emphasis> book</p>" would come back "A  book" and a paragraph that's
	// entirely markup would vanish outright. Confirmed by hand before
	// fixing.
	Annotation struct {
		P []paragraph `xml:"p"`
	} `xml:"annotation"`
	Date      fb2Date   `xml:"date"`
	Coverpage coverpage `xml:"coverpage"`
}

// paragraph is a <p>'s text with any inline markup tags stripped but their
// enclosed text kept.
type paragraph string

func (p *paragraph) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var sb strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				*p = paragraph(sb.String())
				return nil
			}
		}
	}
}

// fb2Author carries the structured name FB2 gives — dc:creator in EPUB
// gives one display string, but FB2 separates first/middle/last. The
// authors table stores one name, so authorName below joins the non-empty
// parts with a single space; this is the one place FB2 offers more
// structure than the schema keeps, and a future browse-by-author feature
// might want it back. Nickname is a separate, valid author form the FB2
// schema permits on its own (a pen name, with no real name given at all)
// — authorName falls back to it when first/middle/last assemble to
// nothing, rather than silently dropping the author.
type fb2Author struct {
	FirstName  string `xml:"first-name"`
	MiddleName string `xml:"middle-name"`
	LastName   string `xml:"last-name"`
	Nickname   string `xml:"nickname"`
}

// fb2Date carries title-info/date's value attribute and text content —
// per the FB2 spec this is when the work was *written*, so it's only used
// as a fallback for PublishedDate when publish-info/year (when this
// *edition* was published, what books.published_date means) is absent.
type fb2Date struct {
	Value string `xml:"value,attr"`
	Text  string `xml:",chardata"`
}

// coverpage's image href is namespaced (l:href, xlink). Tagging it
// href,attr matches on local name regardless of namespace — the same
// trick epub's scheme,attr already relies on.
type coverpage struct {
	Image struct {
		Href string `xml:"href,attr"`
	} `xml:"image"`
}

type publishInfo struct {
	Publisher string `xml:"publisher"`
	Year      string `xml:"year"`
	ISBN      string `xml:"isbn"`
}

// ReadMetadata parses path as either a plain FB2 document or a .fb2.zip
// archive containing exactly one, dispatching on the filename suffix
// case-insensitively.
func ReadMetadata(path string) (Metadata, error) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".fb2.zip"):
		return readMetadataFromZip(path)
	case strings.HasSuffix(lower, ".fb2"):
		f, err := os.Open(path)
		if err != nil {
			return Metadata{}, fmt.Errorf("open fb2: %w", err)
		}
		defer f.Close()
		return readMetadata(f, 0)
	default:
		return Metadata{}, fmt.Errorf("unsupported fb2 path %q", path)
	}
}

// readMetadataFromZip opens path as a zip archive and parses the single
// .fb2 entry inside it — mirroring epub.ReadMetadata's pattern of opening
// the zip and locating the one document that matters. An archive holding
// zero or more than one .fb2 entry is an error rather than a guess at
// which book it contains.
func readMetadataFromZip(path string) (Metadata, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Metadata{}, fmt.Errorf("open fb2.zip: %w", err)
	}
	defer zr.Close()

	var entry *zip.File
	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".fb2") {
			continue
		}
		if entry != nil {
			return Metadata{}, fmt.Errorf("fb2.zip contains more than one .fb2 entry")
		}
		entry = f
	}
	if entry == nil {
		return Metadata{}, fmt.Errorf("fb2.zip contains no .fb2 entry")
	}

	rc, err := entry.Open()
	if err != nil {
		return Metadata{}, fmt.Errorf("open %s in fb2.zip: %w", entry.Name, err)
	}
	defer rc.Close()

	return readCappedDocument(rc, maxZipDocumentBytes)
}

// readCappedDocument parses a document that must not be allowed to inflate,
// bounding it at both ends of the charset decoder.
//
// One cap is not enough once the declared encoding is honoured. The cap on
// the archive's bytes bounds what is read; what encoding/xml holds is a
// whole decoded text node, and a decoder only ever grows the byte count —
// every cp1251 Cyrillic byte becomes two of UTF-8, and a legacy CJK
// encoding's byte can become three. Capping the read alone would hand an
// untrusted archive two to three times the budget the constant names
func readCappedDocument(r io.Reader, max int64) (Metadata, error) {
	return readMetadata(&cappedReader{r: r, remaining: max}, max)
}

// cappedReader is io.LimitReader with a distinct error at the cap instead of
// io.EOF, so the XML decoder reports a truncated document as too large
// rather than as malformed
type cappedReader struct {
	r         io.Reader
	remaining int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, fmt.Errorf("%w (%d bytes)", errDocumentTooLarge, maxZipDocumentBytes)
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// budgetReader stops a stream once it has produced more than budget bytes,
// with the same error cappedReader gives.
//
// It counts whole reads instead of trimming the caller's buffer to what is
// left, which is what makes it right for the far side of a charset decoder:
// a trimmed read cuts the decoder's output mid-rune, and encoding/xml then
// reports invalid UTF-8 rather than the size refusal that actually
// happened. The overshoot is one read buffer against a budget in megabytes
type budgetReader struct {
	r      io.Reader
	budget int64
	read   int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if b.read+int64(n) > b.budget {
		// The crossing read is refused whole rather than trimmed to what
		// is left. Trimming would cut the decoder's output mid-rune, and
		// encoding/xml reports invalid UTF-8 for that before it ever asks
		// for the byte that would carry this error. Refusing the whole
		// read keeps every rune intact and makes the refusal the reason
		// the parse stops
		return 0, fmt.Errorf("%w (%d bytes decoded)", errDocumentTooLarge, b.budget)
	}
	b.read += int64(n)
	return n, err
}

// readMetadata parses an FB2 document from r: the <description> through a
// struct, then a walk of the remaining top-level elements that decodes only
// the <binary> the coverpage names and skips every other one token by token.
// FB2 places <description> first and <binary> last, so the cover's id is
// known before any binary is reached; a document ordered the other way round
// is invalid FB2 and gets no cover, the same as one whose coverpage points
// at an id that does not exist. Reading stops as soon as nothing more is
// wanted — after the description when it names no cover, after the cover's
// binary otherwise — so the rest of an illustrated book is never read
//
// maxDecoded bounds what the charset decoder may produce, and is zero for a
// document read straight off disk, which costs its own size and needs no
// bound. See readCappedDocument for why the two caps are separate
func readMetadata(r io.Reader, maxDecoded int64) (Metadata, error) {
	decoder := xml.NewDecoder(r)
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		decoded, err := decodeCharset(label, input)
		if err != nil || maxDecoded == 0 {
			return decoded, err
		}
		return &budgetReader{r: decoded, budget: maxDecoded}, nil
	}

	if _, err := nextStartElement(decoder); err != nil {
		return Metadata{}, fmt.Errorf("parse fb2: %w", err)
	}

	var desc description
	var haveDescription bool
	var coverID string
	var coverData []byte
	// The cap running out while the walk is still looking for the cover is
	// not a parse failure: the text metadata is already in hand and is not
	// made wrong by a cover that could not be reached. Before the
	// description it is one, since there is nothing to keep
	coverNotReached := func(err error) bool {
		if !haveDescription || !errors.Is(err, errDocumentTooLarge) {
			return false
		}
		slog.Debug("fb2 cover not reached", "error", err)
		return true
	}
walk:
	for {
		tok, err := decoder.Token()
		if err != nil {
			if err == io.EOF || coverNotReached(err) {
				break
			}
			return Metadata{}, fmt.Errorf("parse fb2: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Local == "description" && !haveDescription:
				if err := decoder.DecodeElement(&desc, &t); err != nil {
					return Metadata{}, fmt.Errorf("parse fb2: %w", err)
				}
				haveDescription = true
				coverID = strings.TrimPrefix(strings.TrimSpace(desc.TitleInfo.Coverpage.Image.Href), "#")
				if coverID == "" {
					break walk
				}
			case t.Name.Local == "binary" && haveDescription && attrValue(t, "id") == coverID:
				coverData = readCoverBinary(decoder)
				break walk
			default:
				if err := decoder.Skip(); err != nil {
					if coverNotReached(err) {
						break walk
					}
					return Metadata{}, fmt.Errorf("parse fb2: %w", err)
				}
			}
		case xml.EndElement:
			break walk
		}
	}

	ti := desc.TitleInfo
	return Metadata{
		Title:         strings.TrimSpace(ti.BookTitle),
		Authors:       authorNames(ti.Author),
		Language:      strings.TrimSpace(ti.Lang),
		ISBN:          storage.NormalizeISBN(desc.PublishInfo.ISBN),
		Description:   annotationText(ti.Annotation.P),
		Publisher:     strings.TrimSpace(desc.PublishInfo.Publisher),
		PublishedDate: findPublishedDate(desc),
		Cover:         coverData,
	}, nil
}

// decodeCharset is the xml.Decoder.CharsetReader every FB2 parse installs:
// it wraps the document in the decoder for the charset it declares, so an
// honestly labelled windows-1251 file — which is most of a legacy Russian
// collection — parses instead of failing on "invalid UTF-8" and landing
// under its filename. encoding/xml never calls this for a UTF-8 label, and
// rejects non-UTF-8 bytes whatever Strict says, so the declaration is the
// only thing that can make those bytes readable.
//
// A label htmlindex does not know is passed through unchanged, which is
// also what happens to a UTF-8 file that lies about its label: its title
// becomes mojibake rather than the parse failing. That is the trade — the
// cost falls on a file that misdescribes itself, and mojibake is one edit
// away from right where a parse failure is a book nobody finds.
//
// The .fb2.zip cap sits beneath this untouched: the cap bounds the bytes
// read out of the archive, and a decoder over a capped reader stops where
// the reader does
func decodeCharset(label string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(strings.TrimSpace(label))
	if err != nil {
		slog.Debug("fb2 declares an unknown charset", "charset", label)
		return input, nil
	}
	return transform.NewReader(input, enc.NewDecoder()), nil
}

// nextStartElement consumes tokens up to and including the first start
// element — the document's root, past the XML declaration and any comments
func nextStartElement(decoder *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				return xml.StartElement{}, io.ErrUnexpectedEOF
			}
			return xml.StartElement{}, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start, nil
		}
	}
}

func attrValue(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// readCoverBinary accumulates the character data of the <binary> just
// opened, whitespace stripped, and base64-decodes it. It returns nil on a
// cover past maxCoverBase64Bytes, on malformed base64 and on a truncated
// element — a missing or corrupt cover must never invalidate the rest of
// the metadata, matching epub.readCover. On the first two nothing more of
// the element is read: the caller stops at the cover either way
func readCoverBinary(decoder *xml.Decoder) []byte {
	var encoded []byte
	for {
		tok, err := decoder.Token()
		if err != nil {
			slog.Debug("fb2 cover binary unreadable", "error", err)
			return nil
		}
		switch t := tok.(type) {
		case xml.CharData:
			// Byte by byte into the one buffer, with the cap checked as it
			// fills, rather than a copy, a whitespace strip and an append of
			// the whole token first: the token is the entire node, which for
			// a hostile archive is hundreds of megabytes, and three copies of
			// it made before the cap was consulted is what the cap was meant
			// to prevent. The strip is byte-wise since base64 is ASCII; a
			// non-ASCII byte is kept and fails the decode below, as it should.
			// Real files wrap base64 at a fixed line length, so the newlines
			// have to go before StdEncoding sees them.
			//
			// Room for the token is reserved once, bounded by what the cap
			// still allows: append's own growth past a few hundred bytes is
			// 1.25x a step, and a byte-at-a-time fill of eleven megabytes
			// through it measured five times the final size in allocation
			room := min(len(t), maxCoverBase64Bytes+1-len(encoded))
			encoded = slices.Grow(encoded, room)
			for _, b := range t {
				if b == ' ' || b == '\n' || b == '\r' || b == '\t' {
					continue
				}
				if len(encoded) >= maxCoverBase64Bytes {
					slog.Debug("fb2 cover binary dropped", "reason", "over the byte limit", "limit", maxCoverBase64Bytes)
					return nil
				}
				encoded = append(encoded, b)
			}
		case xml.StartElement:
			if err := decoder.Skip(); err != nil {
				slog.Debug("fb2 cover binary unreadable", "error", err)
				return nil
			}
		case xml.EndElement:
			// Decoded straight from the buffer: a string conversion first
			// would be a second copy of the one copy the cap bounds
			data, err := base64.StdEncoding.AppendDecode(nil, encoded)
			if err != nil {
				return nil
			}
			// The character cap admits up to two bytes past the limit, since
			// a final quantum encodes one, two or three bytes alike; the
			// decoded length is where the limit is exact
			if len(data) > cover.MaxCoverBytes {
				slog.Debug("fb2 cover binary dropped", "reason", "over the byte limit", "limit", cover.MaxCoverBytes)
				return nil
			}
			return data
		}
	}
}

func authorName(a fb2Author) string {
	var parts []string
	for _, p := range []string{a.FirstName, a.MiddleName, a.LastName} {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if name := strings.Join(parts, " "); name != "" {
		return name
	}
	return strings.TrimSpace(a.Nickname)
}

func authorNames(authors []fb2Author) []string {
	var names []string
	for _, a := range authors {
		if name := authorName(a); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// annotationText joins annotation paragraphs with a blank line, dropping
// any that are empty after trimming.
//
// The join is not the whole shape. A single <p> is chardata, so a paragraph
// whose text is wrapped across source lines keeps every one of its interior
// breaks, and a description reaches books.description with a run this
// package never chose. storage.CapBlankLines is where that run is capped —
// the same call internal/epub reaches through storage.PlainDescription, so
// the blank lines in a description are the parser's business in both
// formats rather than something the scanner has to know a format by.
func annotationText(paragraphs []paragraph) string {
	var trimmed []string
	for _, p := range paragraphs {
		if t := strings.TrimSpace(string(p)); t != "" {
			trimmed = append(trimmed, t)
		}
	}
	return storage.CapBlankLines(strings.Join(trimmed, "\n\n"))
}

// findPublishedDate prefers publish-info/year — what books.published_date
// means — falling back to title-info/date's value attribute, then its
// text, when publish-info is absent. Not normalised, same rule
// 2026083114-epub-metadata-completeness established for dc:date and for
// the same reason: the column is TEXT so display formatting stays the
// template's job.
func findPublishedDate(desc description) string {
	if year := strings.TrimSpace(desc.PublishInfo.Year); year != "" {
		return year
	}
	date := desc.TitleInfo.Date
	if v := strings.TrimSpace(date.Value); v != "" {
		return v
	}
	return strings.TrimSpace(date.Text)
}
