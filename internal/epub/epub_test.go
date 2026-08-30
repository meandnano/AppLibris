package epub

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
	if got.ISBN != "978-3-16-148410-0" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "978-3-16-148410-0")
	}
	if got.Description != "A book about examples." {
		t.Errorf("Description = %q, want %q", got.Description, "A book about examples.")
	}
	if got.Cover != nil {
		t.Errorf("Cover = %q, want nil (no cover declared)", got.Cover)
	}
}

func TestReadMetadataNoScemeISBN(t *testing.T) {
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
		t.Errorf("ISBN = %q, want empty (no scheme-tagged identifier)", got.ISBN)
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
