// Package epub reads metadata embedded in an EPUB's OPF package —
// the title, authors, language, ISBN, and description most books already
// carry, per DESIGN.md's metadata source order.
package epub

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"
)

// Metadata is what's extracted from an EPUB's embedded OPF package.
type Metadata struct {
	Title       string
	Authors     []string
	Language    string
	ISBN        string
	Description string
	Cover       []byte
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
		Identifier  []struct {
			Scheme string `xml:"scheme,attr"`
			Value  string `xml:",chardata"`
		} `xml:"identifier"`
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
		Title:       first(pkg.Metadata.Title),
		Authors:     trimAll(pkg.Metadata.Creator),
		Language:    first(pkg.Metadata.Language),
		ISBN:        findISBN(pkg),
		Description: first(pkg.Metadata.Description),
		Cover:       readCover(&zr.Reader, opfPath, pkg),
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

	coverPath := path.Join(path.Dir(opfPath), href)
	f, err := zr.Open(coverPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
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

func findISBN(pkg opfPackage) string {
	for _, id := range pkg.Metadata.Identifier {
		if strings.EqualFold(id.Scheme, "ISBN") {
			return strings.TrimSpace(id.Value)
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
