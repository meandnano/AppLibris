package importer

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrUnsupportedFormat is what a staged file that is neither an EPUB nor an
// FB2 is refused with. Wrapped rather than returned bare wherever the
// refusal has a detail worth naming, so the transport still recognises it
// with errors.Is.
var ErrUnsupportedFormat = errors.New("importer: not an EPUB or FB2 file")

// sniffBytes is how much of a plain file's head is read to decide what it
// is. Wide enough that a byte-order mark, an XML declaration, a DOCTYPE and
// a licence comment can all sit in front of the root element and it is
// still found; a file that has not named its root by then has not named it.
const sniffBytes = 4 << 10

// fb2Prefixes are the two openings an FB2 document legitimately has: the
// XML declaration nearly all of them carry, and a bare root element for the
// ones written without it.
var fb2Prefixes = []string{"<?xml", "<FictionBook"}

// fb2Root is the element that makes an XML document an FB2 one.
var fb2Root = []byte("<FictionBook")

// zipMagic is a local file header, which is what a zip whose first entry is
// stored normally begins with. An empty archive or one written
// back-to-front begins differently and takes the plain-file branch below,
// where it is refused — neither shape is a book.
var zipMagic = []byte("PK\x03\x04")

// detectSuffix decides which supported suffix the file at path is, from its
// content alone.
//
// The client's filename and Content-Type are hints and nothing more: a
// browser sends application/octet-stream for an FB2, and people rename
// files. What the suffix then decides is real — internal/epub and
// internal/fb2 are picked by it, and it is the extension the library file
// is written with — so it is read out of the bytes.
//
// The zip branch reads the central directory through archive/zip, which
// costs one open and inflates nothing: an EPUB is a zip declaring
// META-INF/container.xml, and an .fb2.zip is the archive shape the scanner
// already indexes, one FB2 document and no container.
func detectSuffix(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	head = head[:n]

	if bytes.HasPrefix(head, zipMagic) {
		return zipSuffix(path)
	}
	// Both halves. The opening is what says the file is XML at all, and the
	// root element is what says which XML it is: an SVG, a bare OPF or any
	// other document beginning `<?xml` would otherwise be written into the
	// library as a .fb2 and indexed as a book with a filename for a title.
	// Searching the window rather than testing a prefix, because a real FB2
	// carries its declaration — and sometimes a DOCTYPE and a comment —
	// ahead of the root.
	if hasXMLOpening(head) && bytes.Contains(head, fb2Root) {
		return ".fb2", nil
	}
	return "", ErrUnsupportedFormat
}

// hasXMLOpening reports whether head begins, after an optional UTF-8
// byte-order mark and any leading whitespace, with one of fb2Prefixes.
//
// Only the UTF-8 mark is skipped. A UTF-16 document matches neither this
// nor the root-element search as bytes, and internal/fb2 decodes a declared
// charset from a document it can already read the declaration of — so a
// file this refuses is one that package could not have parsed either.
func hasXMLOpening(head []byte) bool {
	trimmed := strings.TrimLeft(string(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))), " \t\r\n")
	for _, prefix := range fb2Prefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// zipSuffix classifies an archive by what its central directory names.
//
// Exactly one .fb2 entry is required for the .fb2.zip verdict, not merely
// one among several: internal/fb2 reads the single document such an archive
// holds, and an archive of five books is not a book.
func zipSuffix(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnsupportedFormat, err)
	}
	defer zr.Close()

	container, fb2Entries := false, 0
	for _, entry := range zr.File {
		name := strings.ToLower(entry.Name)
		if name == "meta-inf/container.xml" {
			container = true
		}
		if strings.HasSuffix(name, ".fb2") {
			fb2Entries++
		}
	}
	// The whole directory is read before either verdict, so an EPUB that
	// also carries an .fb2 entry is an EPUB whichever order the entries
	// happen to be listed in.
	switch {
	case container:
		return ".epub", nil
	case fb2Entries == 1:
		return ".fb2.zip", nil
	default:
		return "", ErrUnsupportedFormat
	}
}
