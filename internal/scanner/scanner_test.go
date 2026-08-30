package scanner

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"library/internal/storage"
)

const testContainerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

const testOPFTemplate = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
  </metadata>
  <manifest>%s</manifest>
</package>`

const testCoverManifestItem = `<item id="cover-image" href="cover.jpg" media-type="image/jpeg" properties="cover-image"/>`

// writeTestEPUB writes a minimal EPUB to path. When coverImage is non-nil,
// it's declared as the EPUB3 cover-image manifest item and embedded at
// OEBPS/cover.jpg.
func writeTestEPUB(t *testing.T, path, title, author string, coverImage []byte) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)

	var manifestItems string
	if coverImage != nil {
		manifestItems = testCoverManifestItem
	}

	files := map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": testContainerXML,
		"OEBPS/content.opf":      fmt.Sprintf(testOPFTemplate, title, author, manifestItems),
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

	if coverImage != nil {
		w, err := zw.Create("OEBPS/cover.jpg")
		if err != nil {
			t.Fatalf("create cover.jpg in zip: %v", err)
		}
		if _, err := w.Write(coverImage); err != nil {
			t.Fatalf("write cover.jpg in zip: %v", err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
}

// testCoverImage returns a small valid PNG, suitable as a fixture cover.
func testCoverImage(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 20, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 20; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test cover PNG: %v", err)
	}
	return buf.Bytes()
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

func TestScanBasic(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book1.epub"), "Book One", "Author A", nil)
	writeTestEPUB(t, filepath.Join(libDir, "book2.epub"), "Book Two", "Author B", nil)
	if err := os.WriteFile(filepath.Join(libDir, "book3.fb2"), []byte("fake fb2 content"), 0o644); err != nil {
		t.Fatalf("write fb2 stub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3 (notes.txt is not a supported format)", result.Scanned)
	}
	if result.New != 3 {
		t.Errorf("New = %d, want 3", result.New)
	}
	if result.Errors != 0 {
		t.Errorf("Errors = %d, want 0", result.Errors)
	}

	book1, err := db.FindBookByPath(ctx, filepath.Join(libDir, "book1.epub"))
	if err != nil || book1 == nil {
		t.Fatalf("FindBookByPath book1: %v", err)
	}
	if book1.Title != "Book One" {
		t.Errorf("book1 Title = %q, want %q", book1.Title, "Book One")
	}
	if book1.CoverPath != "" {
		t.Errorf("book1 CoverPath = %q, want empty (fixture declares no cover)", book1.CoverPath)
	}

	fb2Book, err := db.FindBookByPath(ctx, filepath.Join(libDir, "book3.fb2"))
	if err != nil || fb2Book == nil {
		t.Fatalf("FindBookByPath fb2: %v", err)
	}
	if fb2Book.Title != "book3" {
		t.Errorf("fb2 Title = %q, want filename-derived %q", fb2Book.Title, "book3")
	}
	if fb2Book.Format != "fb2" {
		t.Errorf("fb2 Format = %q, want %q", fb2Book.Format, "fb2")
	}
}

func TestScanExtractsCover(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book1.epub"), "Book One", "Author A", testCoverImage(t))

	result, err := Scan(ctx, db, libDir, coversDir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 || result.Errors != 0 {
		t.Fatalf("scan = %+v, want New=1 Errors=0", result)
	}

	book, err := db.FindBookByPath(ctx, filepath.Join(libDir, "book1.epub"))
	if err != nil || book == nil {
		t.Fatalf("FindBookByPath: %v", err)
	}
	if book.CoverPath == "" {
		t.Fatal("CoverPath is empty, want a stored cover path")
	}
	if filepath.Dir(book.CoverPath) != coversDir {
		t.Errorf("CoverPath = %q, want it under %q", book.CoverPath, coversDir)
	}

	f, err := os.Open(book.CoverPath)
	if err != nil {
		t.Fatalf("open stored cover: %v", err)
	}
	defer f.Close()
	if _, err := jpeg.Decode(f); err != nil {
		t.Errorf("stored cover does not decode as JPEG: %v", err)
	}
}

func TestScanIsIdempotent(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book1.epub"), "Book One", "Author A", nil)

	first, err := Scan(ctx, db, libDir, coversDir)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 {
		t.Fatalf("first scan New = %d, want 1", first.New)
	}

	second, err := Scan(ctx, db, libDir, coversDir)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.New != 0 || second.Unchanged != 1 {
		t.Errorf("second scan = %+v, want New=0 Unchanged=1", second)
	}
}

func TestScanDetectsMove(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	oldPath := filepath.Join(libDir, "book1.epub")
	writeTestEPUB(t, oldPath, "Book One", "Author A", nil)

	first, err := Scan(ctx, db, libDir, coversDir)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 {
		t.Fatalf("first scan New = %d, want 1", first.New)
	}

	original, err := db.FindBookByPath(ctx, oldPath)
	if err != nil || original == nil {
		t.Fatalf("FindBookByPath oldPath: %v", err)
	}

	newPath := filepath.Join(libDir, "renamed.epub")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename: %v", err)
	}

	second, err := Scan(ctx, db, libDir, coversDir)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Moved != 1 || second.New != 0 {
		t.Errorf("second scan = %+v, want Moved=1 New=0", second)
	}

	moved, err := db.FindBookByPath(ctx, newPath)
	if err != nil || moved == nil {
		t.Fatalf("FindBookByPath newPath: %v", err)
	}
	if moved.ID != original.ID {
		t.Errorf("moved book id = %d, want %d (should be the same row)", moved.ID, original.ID)
	}

	stillAtOldPath, err := db.FindBookByPath(ctx, oldPath)
	if err != nil {
		t.Fatalf("FindBookByPath oldPath after move: %v", err)
	}
	if stillAtOldPath != nil {
		t.Errorf("old path still resolves to a book: %+v", stillAtOldPath)
	}
}
