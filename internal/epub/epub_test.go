package epub

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"library/internal/cover"
)

const containerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

// buildTestEPUB writes a minimal valid EPUB (mimetype + container.xml + the
// given OPF body) to a temp file and returns its path.
func buildTestEPUB(t *testing.T, opfXML string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "book.epub")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create epub file: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)

	files := map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      opfXML,
	}
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s in zip: %v", name, err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return path
}

// buildTestEPUBWithExtra is like buildTestEPUB but also writes extraFiles
// (zip path -> raw bytes) into the archive, for tests that need a cover
// image alongside the OPF.
func buildTestEPUBWithExtra(t *testing.T, opfXML string, extraFiles map[string][]byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "book.epub")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create epub file: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)

	files := map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      opfXML,
	}
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s in zip: %v", name, err)
		}
	}
	for name, content := range extraFiles {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write %s in zip: %v", name, err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return path
}

func TestReadMetadataCoverEPUB3(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Cover Test</dc:title>
  </metadata>
  <manifest>
    <item id="cover-img" href="images/cover.jpg" media-type="image/jpeg" properties="cover-image"/>
  </manifest>
</package>`
	want := []byte("epub3-cover-bytes")

	path := buildTestEPUBWithExtra(t, opfXML, map[string][]byte{
		"OEBPS/images/cover.jpg": want,
	})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !bytes.Equal(got.Cover, want) {
		t.Errorf("Cover = %q, want %q", got.Cover, want)
	}
}

func TestReadMetadataCoverEPUB2(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Cover Test EPUB2</dc:title>
    <meta name="cover" content="cover-img"/>
  </metadata>
  <manifest>
    <item id="cover-img" href="cover.jpg" media-type="image/jpeg"/>
  </manifest>
</package>`
	want := []byte("epub2-cover-bytes")

	path := buildTestEPUBWithExtra(t, opfXML, map[string][]byte{
		"OEBPS/cover.jpg": want,
	})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !bytes.Equal(got.Cover, want) {
		t.Errorf("Cover = %q, want %q", got.Cover, want)
	}
}

func TestReadMetadataCoverDanglingReference(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Dangling Cover</dc:title>
  </metadata>
  <manifest>
    <item id="cover-img" href="missing.jpg" media-type="image/jpeg" properties="cover-image"/>
  </manifest>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Cover != nil {
		t.Errorf("Cover = %q, want nil (declared entry doesn't exist in the zip)", got.Cover)
	}
}

func TestReadMetadata(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>  Example Book  </dc:title>
    <dc:creator>Jane Doe</dc:creator>
    <dc:creator>John Roe</dc:creator>
    <dc:language>en</dc:language>
    <dc:identifier opf:scheme="ISBN">978-3-16-148410-0</dc:identifier>
    <dc:identifier id="uuid_id">urn:uuid:1234</dc:identifier>
    <dc:description>A book about examples.</dc:description>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	if got.Title != "Example Book" {
		t.Errorf("Title = %q, want %q", got.Title, "Example Book")
	}
	if len(got.Authors) != 2 || got.Authors[0] != "Jane Doe" || got.Authors[1] != "John Roe" {
		t.Errorf("Authors = %v, want [Jane Doe John Roe]", got.Authors)
	}
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q", got.Language, "en")
	}
	if got.ISBN != "9783161484100" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "9783161484100")
	}
	if got.Description != "A book about examples." {
		t.Errorf("Description = %q, want %q", got.Description, "A book about examples.")
	}
	if got.Cover != nil {
		t.Errorf("Cover = %q, want nil (no cover declared)", got.Cover)
	}
}

func TestReadMetadataIgnoresNonISBNIdentifier(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>No ISBN Scheme</dc:title>
    <dc:identifier id="uuid_id">urn:uuid:1234</dc:identifier>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "" {
		t.Errorf("ISBN = %q, want empty (a urn:uuid: identifier matches none of the ISBN rules)", got.ISBN)
	}
}

func TestReadMetadataISBNFromURN(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>URN ISBN</dc:title>
    <dc:identifier id="pub-id">urn:isbn:9780306406157</dc:identifier>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "9780306406157" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "9780306406157")
	}
}

// A numeric identifier explicitly tagged with a non-ISBN scheme (an LCCN,
// say) must not be reclassified as a bare ISBN just because it happens to
// be 10 or 13 digits — the declared scheme is evidence it isn't one.
func TestReadMetadataIgnoresShapeMatchingNonISBNScheme(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>LCCN Only</dc:title>
    <dc:identifier opf:scheme="LCCN">2001000002</dc:identifier>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "" {
		t.Errorf("ISBN = %q, want empty (an LCCN-scheme'd identifier must not be read as a bare ISBN)", got.ISBN)
	}
}

// A non-ISBN-scheme'd identifier that happens to be ISBN-shaped must not
// shadow a genuine scheme-less ISBN listed elsewhere in the same file.
func TestReadMetadataNonISBNSchemeDoesNotHideALaterBareISBN(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>LCCN And Bare ISBN</dc:title>
    <dc:identifier opf:scheme="LCCN">2001000002</dc:identifier>
    <dc:identifier>9780306406157</dc:identifier>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "9780306406157" {
		t.Errorf("ISBN = %q, want %q (the scheme-less ISBN, not the LCCN)", got.ISBN, "9780306406157")
	}
}

func TestReadMetadataISBNFromBareHyphenated(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Bare Hyphenated ISBN</dc:title>
    <dc:identifier id="pub-id">978-0-306-40615-7</dc:identifier>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "9780306406157" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "9780306406157")
	}
}

// What a publisher actually writes under the ISBN scheme. Stored as
// written, "ISBN9780000000000(ebook)" is a lookup nobody answers and a
// field nothing re-derives, since enrichment only asks about empty ones.
func TestReadMetadataSchemeISBNWithSurroundingText(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>Annotated ISBN</dc:title>
    <dc:identifier opf:scheme="ISBN">ISBN 978-0-306-40615-7 (ebook)</dc:identifier>
  </metadata>
</package>`

	got, err := ReadMetadata(buildTestEPUB(t, opfXML))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "9780306406157" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "9780306406157")
	}
}

// An ISBN-scheme'd identifier holding no ISBN at all must not end the
// search: a publisher who writes "Not available" there and the real number
// under urn:isbn: still has a book with an ISBN.
func TestReadMetadataSchemeISBNNotAvailableFallsThrough(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>Unhelpful ISBN Scheme</dc:title>
    <dc:identifier opf:scheme="ISBN">Not available</dc:identifier>
    <dc:identifier id="pub-id">urn:isbn:978-0-306-40615-7</dc:identifier>
  </metadata>
</package>`

	got, err := ReadMetadata(buildTestEPUB(t, opfXML))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "9780306406157" {
		t.Errorf("ISBN = %q, want %q (the urn:isbn: identifier)", got.ISBN, "9780306406157")
	}
}

// The one thing the bare branch adds to storage.NormalizeISBN: a
// scheme-less identifier is evidence of nothing, so an ISBN-shaped run with
// text beside it must not be read as one on shape alone.
func TestReadMetadataBareIdentifierWithSurroundingTextIsNotAnISBN(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Prose Identifier</dc:title>
    <dc:identifier id="pub-id">Catalogue 978-0-306-40615-7 second printing</dc:identifier>
  </metadata>
</package>`

	got, err := ReadMetadata(buildTestEPUB(t, opfXML))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "" {
		t.Errorf("ISBN = %q, want empty (a scheme-less identifier that is not wholly an ISBN)", got.ISBN)
	}
}

func TestReadMetadataPublisherAndDate(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Publisher And Date</dc:title>
    <dc:publisher>Acme Books</dc:publisher>
    <dc:date>2011-05-01</dc:date>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Publisher != "Acme Books" {
		t.Errorf("Publisher = %q, want %q", got.Publisher, "Acme Books")
	}
	if got.PublishedDate != "2011-05-01" {
		t.Errorf("PublishedDate = %q, want %q", got.PublishedDate, "2011-05-01")
	}
}

func TestReadMetadataDatePublicationEventWins(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>Event Precedence</dc:title>
    <dc:date opf:event="modification">2020-01-01</dc:date>
    <dc:date opf:event="publication">2011-05-01</dc:date>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.PublishedDate != "2011-05-01" {
		t.Errorf("PublishedDate = %q, want %q (publication event beats modification)", got.PublishedDate, "2011-05-01")
	}
}

func TestReadMetadataDateModificationOnlyYieldsEmpty(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>Modification Only</dc:title>
    <dc:date opf:event="modification">2020-01-01</dc:date>
  </metadata>
</package>`

	path := buildTestEPUB(t, opfXML)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.PublishedDate != "" {
		t.Errorf("PublishedDate = %q, want empty (only a modification-event date is present)", got.PublishedDate)
	}
}

func TestReadMetadataCoverPercentEncodedHref(t *testing.T) {
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Percent Encoded Cover</dc:title>
  </metadata>
  <manifest>
    <item id="cover-img" href="cover%20art.jpg" media-type="image/jpeg" properties="cover-image"/>
  </manifest>
</package>`
	want := []byte("percent-encoded-cover-bytes")

	path := buildTestEPUBWithExtra(t, opfXML, map[string][]byte{
		"OEBPS/cover art.jpg": want,
	})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !bytes.Equal(got.Cover, want) {
		t.Errorf("Cover = %q, want %q", got.Cover, want)
	}
}

func TestReadMetadataCoverHrefWithEncodedHash(t *testing.T) {
	// "%23" is an escaped literal "#" in the filename, not a fragment
	// delimiter — it must survive decoding rather than being truncated
	// as if the raw href were "cover.jpg#1.jpg".
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Cover With Encoded Hash</dc:title>
  </metadata>
  <manifest>
    <item id="cover-img" href="cover%231.jpg" media-type="image/jpeg" properties="cover-image"/>
  </manifest>
</package>`
	want := []byte("encoded-hash-cover-bytes")

	path := buildTestEPUBWithExtra(t, opfXML, map[string][]byte{
		"OEBPS/cover#1.jpg": want,
	})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !bytes.Equal(got.Cover, want) {
		t.Errorf("Cover = %q, want %q", got.Cover, want)
	}
}

func TestReadMetadataCoverHrefWithLiteralFragment(t *testing.T) {
	// A malformed manifest href shouldn't carry a fragment, but if one
	// shows up as a literal "#" it must be stripped before the zip
	// lookup rather than failing for an unguessable reason.
	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Cover With Literal Fragment</dc:title>
  </metadata>
  <manifest>
    <item id="cover-img" href="cover.jpg#page1" media-type="image/jpeg" properties="cover-image"/>
  </manifest>
</package>`
	want := []byte("literal-fragment-cover-bytes")

	path := buildTestEPUBWithExtra(t, opfXML, map[string][]byte{
		"OEBPS/cover.jpg": want,
	})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !bytes.Equal(got.Cover, want) {
		t.Errorf("Cover = %q, want %q", got.Cover, want)
	}
}

func TestReadMetadataNotAZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-book.epub")
	if err := os.WriteFile(path, []byte("this is not a zip file"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := ReadMetadata(path); err == nil {
		t.Error("ReadMetadata on a non-zip file: want error, got nil")
	}
}

func TestReadMetadataMissingContainer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-container.epub")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	zw := zip.NewWriter(f)
	if _, err := zw.Create("mimetype"); err != nil {
		t.Fatalf("create mimetype entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	f.Close()

	if _, err := ReadMetadata(path); err == nil {
		t.Error("ReadMetadata on an epub missing container.xml: want error, got nil")
	}
}

const coverOPF = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Capped Cover</dc:title>
  </metadata>
  <manifest>
    <item id="cover-img" href="cover.png" media-type="image/png" properties="cover-image"/>
  </manifest>
</package>`

// A zip entry costs its compressed size on disk and its inflated size in
// memory, and deflate runs to about 1000:1 on flat data — so a cover entry
// a few kilobytes on disk can inflate to half a gigabyte. Over the cap it
// is treated exactly like a cover that cannot be read: dropped, with the
// text metadata intact
func TestReadMetadataDropsCoverOverTheByteCap(t *testing.T) {
	path := buildTestEPUBWithExtra(t, coverOPF, map[string][]byte{
		"OEBPS/cover.png": make([]byte, cover.MaxCoverBytes+1),
	})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Cover != nil {
		t.Errorf("Cover has %d bytes, want nil for an entry over the cap", len(got.Cover))
	}
	if got.Title != "Capped Cover" {
		t.Errorf("Title = %q, want the text metadata intact", got.Title)
	}
}

// Exactly at the cap is admitted: the limit is inclusive, so a cover the
// size Store accepts is never refused a byte early here
func TestReadMetadataKeepsCoverExactlyAtTheByteCap(t *testing.T) {
	path := buildTestEPUBWithExtra(t, coverOPF, map[string][]byte{
		"OEBPS/cover.png": make([]byte, cover.MaxCoverBytes),
	})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if len(got.Cover) != cover.MaxCoverBytes {
		t.Errorf("Cover has %d bytes, want %d", len(got.Cover), cover.MaxCoverBytes)
	}
}

// The declared uncompressed size is whatever the archive says it is, and a
// header that understates it passes the pre-check. What stops it is
// archive/zip itself, which fails the read with ErrFormat as soon as more
// bytes than declared come out — so a lying header cannot inflate past its
// own claim, and the cover is dropped as unreadable rather than as over the
// cap. This pins that the pre-check is not the only thing standing between
// the header and the heap
func TestReadMetadataDropsCoverWhoseHeaderLiesAboutItsSize(t *testing.T) {
	path := buildTestEPUBWithExtra(t, coverOPF, map[string][]byte{
		"OEBPS/cover.png": make([]byte, cover.MaxCoverBytes+1),
	})
	understateEntrySize(t, path, "OEBPS/cover.png", 100)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Cover != nil {
		t.Errorf("Cover has %d bytes, want nil for an entry that inflates past the cap", len(got.Cover))
	}
	if got.Title != "Capped Cover" {
		t.Errorf("Title = %q, want the text metadata intact", got.Title)
	}
}

// understateEntrySize rewrites the uncompressed size archive/zip reads for
// name — the central directory's, which is the only one it consults — to
// size, leaving the compressed data as it is
func understateEntrySize(t *testing.T, path, name string, size uint32) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Central directory file header: signature, then the file name at
	// offset 46 with its length at 28, and the uncompressed size at 24
	sig := []byte{0x50, 0x4b, 0x01, 0x02}
	for i := 0; i+46 <= len(data); i++ {
		if !bytes.Equal(data[i:i+4], sig) {
			continue
		}
		nameLen := int(binary.LittleEndian.Uint16(data[i+28:]))
		if string(data[i+46:i+46+nameLen]) != name {
			continue
		}
		binary.LittleEndian.PutUint32(data[i+24:], size)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no central directory entry for %s", name)
}

// The OPF is held whole to parse, so an entry inflating past its own cap is
// an error rather than a degraded read: with no package document there is
// no metadata to fall back on
func TestReadMetadataRefusesOPFOverTheByteCap(t *testing.T) {
	padded := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Padded</dc:title>
  </metadata>
  <!--` + strings.Repeat(" ", maxPackageDocBytes) + `-->
</package>`
	path := buildTestEPUB(t, padded)

	if _, err := ReadMetadata(path); err == nil {
		t.Fatal("ReadMetadata: want an error for an OPF over the byte cap")
	}
}
