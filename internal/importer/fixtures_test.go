package importer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"library/internal/storage"
)

const containerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

const opfTemplate = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
  </metadata>
  <manifest></manifest>
</package>`

const fb2Template = `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description>
    <title-info>
      <author><first-name>%s</first-name></author>
      <book-title>%s</book-title>
    </title-info>
  </description>
  <body><section><p>text</p></section></body>
</FictionBook>`

// epubBytes builds a minimal EPUB in memory. padding is appended as an
// extra stored entry, which is how a test reaches an exact byte count
// without the zip writer's own framing having to be predicted.
func epubBytes(t *testing.T, title, author string, padding int) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      fmt.Sprintf(opfTemplate, title, author),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s in zip: %v", name, err)
		}
	}
	if padding > 0 {
		w, err := zw.Create("OEBPS/pad.bin")
		if err != nil {
			t.Fatalf("create pad.bin in zip: %v", err)
		}
		if _, err := w.Write(bytes.Repeat([]byte{'x'}, padding)); err != nil {
			t.Fatalf("write pad.bin in zip: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

func fb2Bytes(title, author string) []byte {
	return fmt.Appendf(nil, fb2Template, author, title)
}

// fb2ZipBytes wraps an FB2 document in the archive shape the scanner
// indexes: one .fb2 entry and no container.
func fb2ZipBytes(t *testing.T, title, author string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("book.fb2")
	if err != nil {
		t.Fatalf("create book.fb2 in zip: %v", err)
	}
	if _, err := w.Write(fb2Bytes(title, author)); err != nil {
		t.Fatalf("write book.fb2 in zip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// testStager builds a Stager over a fresh library, covers and staging
// directory, with the given cap. The clock is a pointer the caller moves,
// so expiry is asserted without waiting for it.
func testStager(t *testing.T, db *storage.DB, maxSize int64) (*Stager, string) {
	t.Helper()
	return testStagerAt(t, db, maxSize, nil)
}

func testStagerAt(t *testing.T, db *storage.DB, maxSize int64, now func() time.Time) (*Stager, string) {
	t.Helper()

	root := t.TempDir()
	libraryDir := filepath.Join(root, "library")
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}
	coversDir := filepath.Join(root, "covers")
	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		t.Fatalf("mkdir covers: %v", err)
	}

	stager, err := New(db, Options{
		LibraryDir: libraryDir,
		CoversDir:  coversDir,
		TempDir:    filepath.Join(root, "staging"),
		MaxSize:    maxSize,
		Writable:   true,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return stager, libraryDir
}

// libraryNames lists what the library directory holds, so a test can assert
// both what landed and that no .part survived it.
func libraryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// stagingNames lists what the staging directory holds.
func stagingNames(t *testing.T, s *Stager) []string {
	t.Helper()
	entries, err := os.ReadDir(s.tempDir)
	if err != nil {
		t.Fatalf("read %s: %v", s.tempDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
