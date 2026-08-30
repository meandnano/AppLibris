package epub

import (
	"archive/zip"
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
