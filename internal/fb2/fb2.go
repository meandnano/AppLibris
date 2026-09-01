// Package fb2 reads metadata embedded in an FB2 document — the title,
// authors, language, description, and cover most books already carry,
// mirroring internal/epub's surface so the scanner stays ignorant of
// format detail.
package fb2

import (
	"archive/zip"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

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

type fictionBook struct {
	Description struct {
		TitleInfo   titleInfo   `xml:"title-info"`
		PublishInfo publishInfo `xml:"publish-info"`
	} `xml:"description"`
	Binary []binaryElement `xml:"binary"`
}

type titleInfo struct {
	BookTitle string      `xml:"book-title"`
	Author    []fb2Author `xml:"author"`
	Lang      string      `xml:"lang"`
	// Annotation holds <p> elements, sometimes with nested emphasis. A
	// plain string field collects all character data within an element,
	// including inside child elements no struct field otherwise claims, so
	// this drops inline markup by construction — correct here, since
	// Metadata.Description is plain text and the template renders it as
	// such.
	Annotation struct {
		P []string `xml:"p"`
	} `xml:"annotation"`
	Date      fb2Date   `xml:"date"`
	Coverpage coverpage `xml:"coverpage"`
}

// fb2Author carries the structured name FB2 gives — dc:creator in EPUB
// gives one display string, but FB2 separates first/middle/last. The
// authors table stores one name, so authorName below joins the non-empty
// parts with a single space; this is the one place FB2 offers more
// structure than the schema keeps, and a future browse-by-author feature
// might want it back.
type fb2Author struct {
	FirstName  string `xml:"first-name"`
	MiddleName string `xml:"middle-name"`
	LastName   string `xml:"last-name"`
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

type binaryElement struct {
	ID      string `xml:"id,attr"`
	Content string `xml:",chardata"`
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
		return readMetadata(f)
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

	return readMetadata(rc)
}

// readMetadata parses an FB2 document from r.
func readMetadata(r io.Reader) (Metadata, error) {
	decoder := xml.NewDecoder(r)
	// A declared non-UTF-8 encoding would otherwise fail the whole parse
	// outright ("encoding ... declared but Decoder.CharsetReader is nil"),
	// not degrade to mojibake. The library's FB2 files are UTF-8
	// regardless of what they declare, so trust the byte content over the
	// label: pass the stream through unchanged for any charset name,
	// known or not. A best-effort parse beats a filename title.
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	var fb fictionBook
	if err := decoder.Decode(&fb); err != nil {
		return Metadata{}, fmt.Errorf("parse fb2: %w", err)
	}

	ti := fb.Description.TitleInfo
	return Metadata{
		Title:         strings.TrimSpace(ti.BookTitle),
		Authors:       authorNames(ti.Author),
		Language:      strings.TrimSpace(ti.Lang),
		ISBN:          strings.TrimSpace(fb.Description.PublishInfo.ISBN),
		Description:   annotationText(ti.Annotation.P),
		Publisher:     strings.TrimSpace(fb.Description.PublishInfo.Publisher),
		PublishedDate: findPublishedDate(fb),
		Cover:         readCover(fb),
	}, nil
}

func authorName(a fb2Author) string {
	var parts []string
	for _, p := range []string{a.FirstName, a.MiddleName, a.LastName} {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
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
func annotationText(paragraphs []string) string {
	var trimmed []string
	for _, p := range paragraphs {
		if t := strings.TrimSpace(p); t != "" {
			trimmed = append(trimmed, t)
		}
	}
	return strings.Join(trimmed, "\n\n")
}

// findPublishedDate prefers publish-info/year — what books.published_date
// means — falling back to title-info/date's value attribute, then its
// text, when publish-info is absent. Not normalised, same rule
// 2026083114-epub-metadata-completeness established for dc:date and for
// the same reason: the column is TEXT so display formatting stays the
// template's job.
func findPublishedDate(fb fictionBook) string {
	if year := strings.TrimSpace(fb.Description.PublishInfo.Year); year != "" {
		return year
	}
	date := fb.Description.TitleInfo.Date
	if v := strings.TrimSpace(date.Value); v != "" {
		return v
	}
	return strings.TrimSpace(date.Text)
}

// readCover finds the <binary> the coverpage's namespaced href points at
// (stripping the leading "#" reference marker) and base64-decodes it.
// Returns nil on any mismatch — a missing or corrupt cover must never
// invalidate the rest of the metadata, matching epub.readCover.
func readCover(fb fictionBook) []byte {
	id := strings.TrimPrefix(strings.TrimSpace(fb.Description.TitleInfo.Coverpage.Image.Href), "#")
	if id == "" {
		return nil
	}

	for _, b := range fb.Binary {
		if b.ID != id {
			continue
		}
		// Real files wrap base64 content at a fixed line length, so the
		// chardata carries embedded newlines StdEncoding won't tolerate.
		data, err := base64.StdEncoding.DecodeString(stripWhitespace(b.Content))
		if err != nil {
			return nil
		}
		return data
	}
	return nil
}

func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
