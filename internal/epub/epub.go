// Package epub reads metadata embedded in an EPUB's OPF package —
// the title, authors, language, ISBN, and description most books already
// carry, per DESIGN.md's metadata source order.
package epub

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"strings"
)

// Metadata is what's extracted from an EPUB's embedded OPF package.
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

type container struct {
	Rootfiles struct {
		Rootfile []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfile"`
	} `xml:"rootfiles"`
}

type opfPackage struct {
	Metadata struct {
		Title       []string `xml:"title"`
		Creator     []string `xml:"creator"`
		Language    []string `xml:"language"`
		Description []string `xml:"description"`
		Publisher   []string `xml:"publisher"`
		Identifier  []struct {
			Scheme string `xml:"scheme,attr"`
			Value  string `xml:",chardata"`
		} `xml:"identifier"`
		// Date carries its EPUB2 opf:event attribute (publication,
		// modification, creation) because dc:date can repeat with
		// different meanings; EPUB3 drops the attribute entirely, which
		// findPublishedDate treats as the publication date.
		Date []struct {
			Event string `xml:"event,attr"`
			Value string `xml:",chardata"`
		} `xml:"date"`
		Meta []struct {
			Name    string `xml:"name,attr"`
			Content string `xml:"content,attr"`
		} `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Item []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
}

// ReadMetadata opens path as an EPUB (zip) container and parses the OPF
// package it points to.
func ReadMetadata(path string) (Metadata, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Metadata{}, fmt.Errorf("open epub: %w", err)
	}
	defer zr.Close()

	opfPath, err := findOPFPath(&zr.Reader)
	if err != nil {
		return Metadata{}, err
	}

	pkg, err := readOPFPackage(&zr.Reader, opfPath)
	if err != nil {
		return Metadata{}, err
	}

	return Metadata{
		Title:         first(pkg.Metadata.Title),
		Authors:       trimAll(pkg.Metadata.Creator),
		Language:      first(pkg.Metadata.Language),
		ISBN:          findISBN(pkg),
		Description:   first(pkg.Metadata.Description),
		Publisher:     first(pkg.Metadata.Publisher),
		PublishedDate: findPublishedDate(pkg),
		Cover:         readCover(&zr.Reader, opfPath, pkg),
	}, nil
}

// readCover locates the manifest item the OPF declares as the cover (EPUB3's
// properties="cover-image", falling back to EPUB2's <meta name="cover">) and
// returns its raw bytes. Returns nil whenever no cover is declared or the
// declared entry can't be read — a missing/corrupt cover doesn't invalidate
// the rest of the metadata.
func readCover(zr *zip.Reader, opfPath string, pkg opfPackage) []byte {
	href := findCoverHref(pkg)
	if href == "" {
		return nil
	}

	// Strip any fragment before decoding, not after: a literal "#" in the
	// raw href is a fragment delimiter, but a percent-encoded one ("%23")
	// is an escaped filename character and must survive decoding intact.
	raw := href
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}

	// href is a URI reference, so a name like "cover art.jpg" is declared
	// as "cover%20art.jpg"; decode it before treating it as a zip path.
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		// Not a valid escape sequence — a literal "%" in a filename is
		// legal in a zip, so fall back to the raw href rather than
		// giving up on the cover entirely.
		decoded = raw
	}

	coverPath := path.Join(path.Dir(opfPath), decoded)
	f, err := zr.Open(coverPath)
	if err != nil {
		slog.Debug("cover declared but unreadable", "href", href, "path", coverPath, "error", err)
		return nil
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		slog.Debug("cover declared but unreadable", "href", href, "path", coverPath, "error", err)
		return nil
	}
	return data
}

func findCoverHref(pkg opfPackage) string {
	for _, item := range pkg.Manifest.Item {
		if hasToken(item.Properties, "cover-image") {
			return item.Href
		}
	}

	var coverID string
	for _, m := range pkg.Metadata.Meta {
		if strings.EqualFold(m.Name, "cover") {
			coverID = m.Content
			break
		}
	}
	if coverID == "" {
		return ""
	}

	for _, item := range pkg.Manifest.Item {
		if item.ID == coverID {
			return item.Href
		}
	}
	return ""
}

func hasToken(tokens, target string) bool {
	for _, t := range strings.Fields(tokens) {
		if t == target {
			return true
		}
	}
	return false
}

func findOPFPath(zr *zip.Reader) (string, error) {
	f, err := zr.Open("META-INF/container.xml")
	if err != nil {
		return "", fmt.Errorf("open container.xml: %w", err)
	}
	defer f.Close()

	var c container
	if err := xml.NewDecoder(f).Decode(&c); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	if len(c.Rootfiles.Rootfile) == 0 || c.Rootfiles.Rootfile[0].FullPath == "" {
		return "", fmt.Errorf("container.xml: no rootfile")
	}
	return c.Rootfiles.Rootfile[0].FullPath, nil
}

func readOPFPackage(zr *zip.Reader, opfPath string) (opfPackage, error) {
	f, err := zr.Open(opfPath)
	if err != nil {
		return opfPackage{}, fmt.Errorf("open %s: %w", opfPath, err)
	}
	defer f.Close()

	var pkg opfPackage
	if err := xml.NewDecoder(f).Decode(&pkg); err != nil {
		return opfPackage{}, fmt.Errorf("parse %s: %w", opfPath, err)
	}
	return pkg, nil
}

// findISBN tries, in order, an EPUB2 opf:scheme="ISBN" identifier, an EPUB3
// urn:isbn: identifier, and a bare ISBN-shaped identifier — a publisher who
// marks an identifier as an ISBN at all almost always uses one of the first
// two forms, so the refines-based EPUB3 form isn't worth the extra
// resolution logic. The check digit is never validated: a malformed ISBN in
// the file is still the best identifier it offers.
func findISBN(pkg opfPackage) string {
	for _, id := range pkg.Metadata.Identifier {
		if strings.EqualFold(id.Scheme, "ISBN") {
			if isbn := normalizeISBN(id.Value); isbn != "" {
				return isbn
			}
		}
	}
	for _, id := range pkg.Metadata.Identifier {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(id.Value)), "urn:isbn:") {
			if isbn := normalizeISBN(id.Value); isbn != "" {
				return isbn
			}
		}
	}
	for _, id := range pkg.Metadata.Identifier {
		if isBareISBN(id.Value) {
			return normalizeISBN(id.Value)
		}
	}
	return ""
}

// isBareISBN reports whether raw, with no scheme or urn:isbn: marker, is
// still shaped like an ISBN-10 or ISBN-13: digits, hyphens and spaces, with
// a trailing X permitted only where an ISBN-10 check digit allows one. This
// is what keeps an unrelated identifier (a UUID, say) from being mistaken
// for an unmarked ISBN.
func isBareISBN(raw string) bool {
	v := strings.TrimSpace(raw)
	if v == "" {
		return false
	}
	runes := []rune(v)
	for i, r := range runes {
		switch {
		case r >= '0' && r <= '9':
		case r == '-' || r == ' ':
		case (r == 'x' || r == 'X') && i == len(runes)-1:
		default:
			return false
		}
	}
	switch normalized := normalizeISBN(v); len(normalized) {
	case 10:
		return true
	case 13:
		return !strings.HasSuffix(normalized, "X")
	default:
		return false
	}
}

// normalizeISBN strips a urn:isbn: prefix and any hyphens or spaces, and
// upper-cases a trailing check-digit X.
func normalizeISBN(raw string) string {
	v := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(v), "urn:isbn:") {
		v = v[len("urn:isbn:"):]
	}
	v = strings.NewReplacer("-", "", " ", "").Replace(v)
	if v == "" {
		return ""
	}
	if last := v[len(v)-1]; last == 'x' {
		v = v[:len(v)-1] + "X"
	}
	return v
}

// findPublishedDate picks the publication date among possibly-repeated
// dc:date elements: EPUB2 tags each with an opf:event attribute, so the one
// marked "publication" wins; EPUB3 drops the attribute and just gives the
// publication date directly, so the first event-less element is the
// fallback. creation and modification are never used — a file's last-edited
// date is worse in a column called published_date than leaving it empty.
func findPublishedDate(pkg opfPackage) string {
	for _, d := range pkg.Metadata.Date {
		if strings.EqualFold(d.Event, "publication") {
			return strings.TrimSpace(d.Value)
		}
	}
	for _, d := range pkg.Metadata.Date {
		if d.Event == "" {
			return strings.TrimSpace(d.Value)
		}
	}
	return ""
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func trimAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
