package scanner

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"library/internal/storage"
)

// testMissingGrace is used by every test that doesn't itself exercise
// missing-file grace-period timing — long enough that nothing marked
// missing during a test's short lifetime is ever eligible for pruning.
const testMissingGrace = 24 * time.Hour

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

// writeTestEPUBWithOPF is like writeTestEPUB but takes the OPF package body
// verbatim, for tests exercising metadata fields the fixed template doesn't
// carry.
func writeTestEPUBWithOPF(t *testing.T, path, opfXML string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)

	files := map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": testContainerXML,
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
}

const testFB2Template = `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
  <description>
    <title-info>
      <book-title>%s</book-title>
      <author>
        <first-name>%s</first-name>
      </author>%s
    </title-info>
  </description>
  <body></body>%s
</FictionBook>`

const testFB2CoverpageTemplate = `
      <coverpage>
        <image l:href="#cover.jpg"/>
      </coverpage>`

// writeTestFB2 writes a minimal FB2 document to path. When coverImage is
// non-nil, it's declared via title-info/coverpage and embedded as a
// base64-encoded <binary id="cover.jpg">.
func writeTestFB2(t *testing.T, path, title, author string, coverImage []byte) {
	t.Helper()

	var coverpage, binary string
	if coverImage != nil {
		coverpage = testFB2CoverpageTemplate
		binary = fmt.Sprintf("\n  <binary id=\"cover.jpg\" content-type=\"image/jpeg\">%s</binary>",
			base64.StdEncoding.EncodeToString(coverImage))
	}

	body := fmt.Sprintf(testFB2Template, title, author, coverpage, binary)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeTestFB2Zip writes a .fb2.zip archive containing exactly one entry,
// "book.fb2", built the same way writeTestFB2 builds a plain file.
func writeTestFB2Zip(t *testing.T, path, title, author string, coverImage []byte) {
	t.Helper()

	var coverpage, binary string
	if coverImage != nil {
		coverpage = testFB2CoverpageTemplate
		binary = fmt.Sprintf("\n  <binary id=\"cover.jpg\" content-type=\"image/jpeg\">%s</binary>",
			base64.StdEncoding.EncodeToString(coverImage))
	}
	body := fmt.Sprintf(testFB2Template, title, author, coverpage, binary)

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	w, err := zw.Create("book.fb2")
	if err != nil {
		t.Fatalf("create book.fb2 in zip: %v", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write book.fb2 in zip: %v", err)
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

// bookByPath resolves a scanned file's path — relative to the library
// root, the form it's stored under — all the way to its book row, via the
// book_files -> books join the storage package doesn't expose as a single
// call.
func bookByPath(t *testing.T, ctx context.Context, db *storage.DB, relPath string) *storage.Book {
	t.Helper()

	f, err := db.FindFileByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("FindFileByPath %s: %v", relPath, err)
	}
	if f == nil {
		t.Fatalf("FindFileByPath %s: no such file", relPath)
	}

	var b storage.Book
	err = db.Read().QueryRowContext(ctx, `SELECT title, format, cover_path, cover_retry, publisher, published_date FROM books WHERE id = ?`, f.BookID).
		Scan(&b.Title, &b.Format, &b.CoverPath, &b.CoverRetry, &b.Publisher, &b.PublishedDate)
	if err != nil {
		t.Fatalf("look up book %d: %v", f.BookID, err)
	}
	b.ID = f.BookID
	return &b
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

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
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

	book1 := bookByPath(t, ctx, db, "book1.epub")
	if book1.Title != "Book One" {
		t.Errorf("book1 Title = %q, want %q", book1.Title, "Book One")
	}
	if book1.CoverPath != "" {
		t.Errorf("book1 CoverPath = %q, want empty (fixture declares no cover)", book1.CoverPath)
	}

	fb2Book := bookByPath(t, ctx, db, "book3.fb2")
	if fb2Book.Title != "book3" {
		t.Errorf("fb2 Title = %q, want filename-derived %q", fb2Book.Title, "book3")
	}
	if fb2Book.Format != "fb2" {
		t.Errorf("fb2 Format = %q, want %q", fb2Book.Format, "fb2")
	}
}

const testOPFWithPublisherAndDate = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Book With Publisher</dc:title>
    <dc:creator>Author A</dc:creator>
    <dc:publisher>Acme Books</dc:publisher>
    <dc:date>2011-05-01</dc:date>
  </metadata>
</package>`

func TestScanExtractsPublisherAndPublishedDate(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUBWithOPF(t, filepath.Join(libDir, "book.epub"), testOPFWithPublisherAndDate)

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	book := bookByPath(t, ctx, db, "book.epub")
	if book.Publisher != "Acme Books" {
		t.Errorf("Publisher = %q, want %q", book.Publisher, "Acme Books")
	}
	if book.PublishedDate != "2011-05-01" {
		t.Errorf("PublishedDate = %q, want %q", book.PublishedDate, "2011-05-01")
	}
}

const testOPFWithTwoCreators = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Two Authors</dc:title>
    <dc:creator>Neil Gaiman</dc:creator>
    <dc:creator>Terry Pratchett</dc:creator>
  </metadata>
</package>`

// The end-to-end path the author-order bug actually travels: internal/epub
// preserves OPF document order into Metadata.Authors, and this proves that
// order survives all the way through the scanner into ListBookAuthors,
// rather than being reshuffled at the storage boundary.
func TestScanPreservesAuthorOrderFromOPF(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUBWithOPF(t, filepath.Join(libDir, "book.epub"), testOPFWithTwoCreators)

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	book := bookByPath(t, ctx, db, "book.epub")
	authors, err := db.ListBookAuthors(ctx)
	if err != nil {
		t.Fatalf("ListBookAuthors: %v", err)
	}
	got := authors[book.ID]
	if len(got) != 2 || got[0] != "Neil Gaiman" || got[1] != "Terry Pratchett" {
		t.Errorf("authors = %v, want [Neil Gaiman Terry Pratchett] — the OPF's own document order", got)
	}
}

func TestScanExtractsCover(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book1.epub"), "Book One", "Author A", testCoverImage(t))

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 || result.Errors != 0 {
		t.Fatalf("scan = %+v, want New=1 Errors=0", result)
	}

	book := bookByPath(t, ctx, db, "book1.epub")
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

func TestScanRegeneratesMissingCoverDirectory(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", testCoverImage(t))
	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 {
		t.Fatalf("first scan = %+v, want New=1", first)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	if err := os.RemoveAll(coversDir); err != nil {
		t.Fatalf("remove covers directory: %v", err)
	}

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Unchanged != 1 || second.CoversRegenerated != 1 || second.Errors != 0 {
		t.Errorf("second scan = %+v, want Unchanged=1 CoversRegenerated=1 Errors=0", second)
	}
	if _, err := os.Stat(book.CoverPath); err != nil {
		t.Fatalf("stat regenerated cover: %v", err)
	}
	if width, height := decodedJPEGSize(t, book.CoverPath); width != 20 || height != 30 {
		t.Errorf("regenerated cover size = %dx%d, want 20x30", width, height)
	}
}

func TestScanRegeneratesZeroByteCover(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", testCoverImage(t))
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	if err := os.WriteFile(book.CoverPath, nil, 0o644); err != nil {
		t.Fatalf("truncate cover: %v", err)
	}

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.CoversRegenerated != 1 {
		t.Errorf("second scan = %+v, want CoversRegenerated=1", second)
	}
	if width, height := decodedJPEGSize(t, book.CoverPath); width != 20 || height != 30 {
		t.Errorf("regenerated cover size = %dx%d, want 20x30", width, height)
	}
}

func TestScanDoesNotRetryCoverlessBook(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if first.CoversRegenerated != 0 || second.CoversRegenerated != 0 {
		t.Errorf("cover regeneration counts = first %d, second %d, want both 0", first.CoversRegenerated, second.CoversRegenerated)
	}
	if book := bookByPath(t, ctx, db, "book.epub"); book.CoverPath != "" {
		t.Errorf("CoverPath = %q, want empty", book.CoverPath)
	}
	if strings.Contains(logs.String(), "regenerate cover failed") {
		t.Errorf("coverless rescan attempted regeneration: %s", logs.String())
	}
}

func TestScanRetriesInitialCoverStoreFailure(t *testing.T) {
	libDir := t.TempDir()
	coversDir := filepath.Join(t.TempDir(), "covers")
	db := openTestDB(t)
	ctx := context.Background()

	if err := os.WriteFile(coversDir, []byte("blocks directory creation"), 0o644); err != nil {
		t.Fatalf("write covers path blocker: %v", err)
	}
	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", testCoverImage(t))
	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 || first.Errors != 0 {
		t.Fatalf("first scan = %+v, want New=1 Errors=0", first)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	if book.CoverPath != "" || !book.CoverRetry {
		t.Fatalf("book after failed store = %+v, want empty path and retry marker", book)
	}

	if err := os.Remove(coversDir); err != nil {
		t.Fatalf("remove covers path blocker: %v", err)
	}
	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Unchanged != 1 || second.CoversRegenerated != 1 || second.Errors != 0 {
		t.Errorf("second scan = %+v, want Unchanged=1 CoversRegenerated=1 Errors=0", second)
	}
	book = bookByPath(t, ctx, db, "book.epub")
	if book.CoverPath == "" || book.CoverRetry {
		t.Fatalf("book after retry = %+v, want stored path and cleared retry marker", book)
	}
	decodedJPEGSize(t, book.CoverPath)
}

func TestScanDoesNotRegenerateCoverOnNonNotExistStatError(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", testCoverImage(t))
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	if err := os.Remove(book.CoverPath); err != nil {
		t.Fatalf("remove cover: %v", err)
	}
	if err := os.Symlink(filepath.Base(book.CoverPath), book.CoverPath); err != nil {
		t.Fatalf("create cover symlink loop: %v", err)
	}

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.CoversRegenerated != 0 || second.Errors != 0 {
		t.Errorf("second scan = %+v, want CoversRegenerated=0 Errors=0", second)
	}
	if !strings.Contains(logs.String(), "inspect cover failed") {
		t.Errorf("scan log = %q, want inspect-cover warning", logs.String())
	}
	info, err := os.Lstat(book.CoverPath)
	if err != nil {
		t.Fatalf("lstat cover after scan: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("cover path mode after scan = %v, want original symlink untouched", info.Mode())
	}
}

func decodedJPEGSize(t *testing.T, path string) (width, height int) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return img.Bounds().Dx(), img.Bounds().Dy()
}

func TestScanIsIdempotent(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book1.epub"), "Book One", "Author A", nil)

	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 {
		t.Fatalf("first scan New = %d, want 1", first.New)
	}

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
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

	oldRel := "book1.epub"
	oldPath := filepath.Join(libDir, oldRel)
	writeTestEPUB(t, oldPath, "Book One", "Author A", nil)

	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 {
		t.Fatalf("first scan New = %d, want 1", first.New)
	}

	originalFile, err := db.FindFileByPath(ctx, oldRel)
	if err != nil || originalFile == nil {
		t.Fatalf("FindFileByPath oldRel: %v", err)
	}

	newRel := "renamed.epub"
	newPath := filepath.Join(libDir, newRel)
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename: %v", err)
	}

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Moved != 1 || second.New != 0 {
		t.Errorf("second scan = %+v, want Moved=1 New=0", second)
	}

	movedFile, err := db.FindFileByPath(ctx, newRel)
	if err != nil || movedFile == nil {
		t.Fatalf("FindFileByPath newRel: %v", err)
	}
	if movedFile.BookID != originalFile.BookID {
		t.Errorf("moved file's book id = %d, want %d (should be the same book)", movedFile.BookID, originalFile.BookID)
	}

	// the old path's book_files row is deliberately left stale, not
	// deleted or repointed — missing-file handling is separately deferred,
	// and WalkDir never revisits a path once the file there is gone
	staleFile, err := db.FindFileByPath(ctx, oldRel)
	if err != nil {
		t.Fatalf("FindFileByPath oldRel after move: %v", err)
	}
	if staleFile == nil {
		t.Error("old path's book_files row was removed, want it left stale")
	} else if staleFile.BookID != originalFile.BookID {
		t.Errorf("stale row's book id = %d, want unchanged %d", staleFile.BookID, originalFile.BookID)
	}
}

func TestScanTracksMultipleLocations(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	sourcePath := filepath.Join(t.TempDir(), "source.epub")
	writeTestEPUB(t, sourcePath, "Duplicated Book", "Author A", nil)
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	relA := "copy-a.epub"
	relB := "copy-b.epub"
	pathA := filepath.Join(libDir, relA)
	pathB := filepath.Join(libDir, relB)
	if err := os.WriteFile(pathA, content, 0o644); err != nil {
		t.Fatalf("write copy-a: %v", err)
	}
	if err := os.WriteFile(pathB, content, 0o644); err != nil {
		t.Fatalf("write copy-b: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 || result.Moved != 1 {
		t.Fatalf("scan = %+v, want New=1 Moved=1 (one book, one extra location)", result)
	}

	fileA, err := db.FindFileByPath(ctx, relA)
	if err != nil || fileA == nil {
		t.Fatalf("FindFileByPath copy-a: %v", err)
	}
	fileB, err := db.FindFileByPath(ctx, relB)
	if err != nil || fileB == nil {
		t.Fatalf("FindFileByPath copy-b: %v", err)
	}
	if fileA.BookID != fileB.BookID {
		t.Errorf("copy-a book id %d != copy-b book id %d, want the same book", fileA.BookID, fileB.BookID)
	}

	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if bookCount != 1 {
		t.Errorf("books count = %d, want 1 (byte-identical content is one book)", bookCount)
	}

	// rescan without touching the filesystem: both locations are cheap-path
	// unchanged, and neither path's row is ever dropped or reassigned
	rescan, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if rescan.Unchanged != 2 || rescan.New != 0 || rescan.Moved != 0 {
		t.Errorf("rescan = %+v, want Unchanged=2 New=0 Moved=0", rescan)
	}

	fileA2, err := db.FindFileByPath(ctx, relA)
	if err != nil || fileA2 == nil || fileA2.ID != fileA.ID {
		t.Errorf("copy-a row changed across rescans: before=%+v after=%+v (%v)", fileA, fileA2, err)
	}
	fileB2, err := db.FindFileByPath(ctx, relB)
	if err != nil || fileB2 == nil || fileB2.ID != fileB.ID {
		t.Errorf("copy-b row changed across rescans: before=%+v after=%+v (%v)", fileB, fileB2, err)
	}
}

// Regression for the orphan bug this step fixes: overwriting a path with
// different, never-before-seen content used to reassign the path (new
// content means the scanner takes the "create a book" branch, and
// upsertBookFileTx's ON CONFLICT reassigns the path unconditionally) and
// leave the previous book's row behind with zero locations, forever.
// Before this step, this assertion failed with 2 books.
func TestScanPrunesOrphanWhenPathContentReplaced(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	rel := "book.epub"
	path := filepath.Join(libDir, rel)
	writeTestEPUB(t, path, "Book A", "Author A", nil)

	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 || first.Orphaned != 0 {
		t.Fatalf("first scan = %+v, want New=1 Orphaned=0", first)
	}

	// Overwrite the same path with genuinely different, new content — same
	// filename, a different book, the way a re-download over an existing
	// file or an in-place metadata rewrite would. New content means this
	// scan takes the create-a-book branch, not the known-content one.
	writeTestEPUB(t, path, "Book B", "Author B", nil)

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.New != 1 || second.Orphaned != 1 || second.Moved != 0 {
		t.Errorf("second scan = %+v, want New=1 Orphaned=1 Moved=0", second)
	}

	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if bookCount != 1 {
		t.Errorf("books count = %d, want 1 (Book A's orphaned row must be gone)", bookCount)
	}

	book := bookByPath(t, ctx, db, rel)
	if book.Title != "Book B" {
		t.Errorf("book at path = %q, want %q", book.Title, "Book B")
	}
}

// The other branch that can orphan a book: the path is overwritten with
// content that matches an *existing* book elsewhere in the library, not
// new content. This takes the known-content branch (ReassignFileAndPrune-
// Orphan) rather than create-a-book (CreateBookWithFile) — both call
// upsertBookFileTx, which reassigns a path unconditionally, so both need
// the prune.
func TestScanPrunesOrphanWhenPathReassignedToKnownContent(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	relA := "a.epub"
	pathA := filepath.Join(libDir, relA)
	writeTestEPUB(t, pathA, "Book A", "Author A", nil)
	relB := "b.epub"
	pathB := filepath.Join(libDir, relB)
	writeTestEPUB(t, pathB, "Book B", "Author B", nil)

	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 2 || first.Orphaned != 0 {
		t.Fatalf("first scan = %+v, want New=2 Orphaned=0", first)
	}

	contentB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("read Book B content: %v", err)
	}
	// Overwrite A's path with B's exact bytes — known content, reassigned
	// to a different, already-existing book.
	if err := os.WriteFile(pathA, contentB, 0o644); err != nil {
		t.Fatalf("overwrite pathA with Book B's content: %v", err)
	}

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Moved != 1 || second.Orphaned != 1 || second.New != 0 {
		t.Errorf("second scan = %+v, want Moved=1 Orphaned=1 New=0", second)
	}

	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if bookCount != 1 {
		t.Errorf("books count = %d, want 1 (Book A's orphaned row must be gone)", bookCount)
	}

	bookAtA := bookByPath(t, ctx, db, relA)
	if bookAtA.Title != "Book B" {
		t.Errorf("book at pathA = %q, want %q", bookAtA.Title, "Book B")
	}
	bookAtB := bookByPath(t, ctx, db, relB)
	if bookAtB.ID != bookAtA.ID {
		t.Errorf("pathA and pathB resolve to different books (%d, %d); want the same Book B", bookAtA.ID, bookAtB.ID)
	}
}

// A directory WalkDir can't read must cost its own subtree, not the sweep:
// files in sibling directories still get indexed, and the unreadable
// directory counts as one error rather than aborting everything after it
// in walk order.
func TestScanSkipsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory mode bits aren't enforced")
	}

	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author A", nil)

	restrictedDir := filepath.Join(libDir, "restricted")
	if err := os.Mkdir(restrictedDir, 0o755); err != nil {
		t.Fatalf("mkdir restricted: %v", err)
	}
	writeTestEPUB(t, filepath.Join(restrictedDir, "hidden.epub"), "Hidden Book", "Author B", nil)
	if err := os.Chmod(restrictedDir, 0o000); err != nil {
		t.Fatalf("chmod restricted: %v", err)
	}
	// restore before TempDir's own cleanup tries to remove it
	t.Cleanup(func() { os.Chmod(restrictedDir, 0o755) })

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 {
		t.Errorf("New = %d, want 1 (the sibling file)", result.New)
	}
	if result.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the unreadable directory)", result.Errors)
	}

	sibling := bookByPath(t, ctx, db, "sibling.epub")
	if sibling.Title != "Sibling Book" {
		t.Errorf("sibling Title = %q, want %q", sibling.Title, "Sibling Book")
	}
}

// Unlike an unreadable subdirectory, a missing library root is a
// configuration error (the volume didn't mount), not a partial result —
// it must not look like an empty, successful sweep.
func TestScanMissingLibraryDirReturnsError(t *testing.T) {
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	missingDir := filepath.Join(t.TempDir(), "does-not-exist")

	result, err := Scan(ctx, db, missingDir, coversDir, testMissingGrace)
	if err == nil {
		t.Fatal("Scan with a missing library dir: want an error, got nil")
	}
	if result.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", result.Scanned)
	}
}

// An existing-but-unreadable root exercises a different branch than a
// missing one: WalkDir hands the callback a non-nil DirEntry for it (see
// TestScanMissingLibraryDirReturnsError for the nil-DirEntry case), so
// Scan's directory-skip guard has to check path != libraryDir to keep
// treating the root itself as fatal rather than a skippable subtree.
func TestScanUnreadableLibraryRootReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory mode bits aren't enforced")
	}

	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)

	if err := os.Chmod(libDir, 0o000); err != nil {
		t.Fatalf("chmod libDir: %v", err)
	}
	// restore before TempDir's own cleanup tries to remove it
	t.Cleanup(func() { os.Chmod(libDir, 0o755) })

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err == nil {
		t.Fatal("Scan with an unreadable library root: want an error, got nil")
	}
	if result.Errors != 0 {
		t.Errorf("Errors = %d, want 0 (this is the fatal-root path, not a counted skipped subtree)", result.Errors)
	}
}

// Stored paths must be relative to the library root even for a file several
// directories deep — a flat library can't distinguish that from storing the
// absolute path, since the two forms only diverge once a file is nested.
func TestScanStoresRelativePaths(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	nestedDir := filepath.Join(libDir, "sub", "dir")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	writeTestEPUB(t, filepath.Join(nestedDir, "book.epub"), "Nested Book", "Author A", nil)

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	f, err := db.FindFileByPath(ctx, "sub/dir/book.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath sub/dir/book.epub: %+v, %v; want it found", f, err)
	}

	rows, err := db.Read().QueryContext(ctx, `SELECT file_path FROM book_files`)
	if err != nil {
		t.Fatalf("query file_path: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan file_path: %v", err)
		}
		if strings.Contains(p, libDir) {
			t.Errorf("stored file_path %q contains the temp-directory prefix %q, want a relative path", p, libDir)
		}
	}
}

// The same content, at the same relative path, must be recognised as one
// location regardless of the absolute root it's scanned through — dev's
// ./library and the container's /library, say — rather than a false second
// location on the book's first day in production.
func TestScanSameLibraryThroughDifferentRootsYieldsOneLocation(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(rootA, "book.epub"), "Book One", "Author A", nil)

	first, err := Scan(ctx, db, rootA, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan (rootA): %v", err)
	}
	if first.New != 1 {
		t.Fatalf("first scan New = %d, want 1", first.New)
	}

	// the exact same content, at the same relative path, mounted under a
	// different absolute root
	content, err := os.ReadFile(filepath.Join(rootA, "book.epub"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "book.epub"), content, 0o644); err != nil {
		t.Fatalf("write copy under rootB: %v", err)
	}

	second, err := Scan(ctx, db, rootB, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan (rootB): %v", err)
	}
	// Scanned/Unchanged pin that the file was actually visited and matched
	// the existing row, not just that New/Moved happen to be zero — which a
	// scan that silently skipped the file would also satisfy
	if second.Scanned != 1 || second.Unchanged != 1 || second.New != 0 || second.Moved != 0 || second.Errors != 0 {
		t.Errorf("second scan through a different root = %+v, want Scanned=1 Unchanged=1 New=0 Moved=0 Errors=0 (recognised as the same location)", second)
	}

	var fileCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM book_files`).Scan(&fileCount); err != nil {
		t.Fatalf("count book_files: %v", err)
	}
	if fileCount != 1 {
		t.Errorf("book_files rows = %d, want 1 (same relative path both times, not a second location)", fileCount)
	}
}

func TestFilenameTitleStripsWholeMatchedSuffix(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"epub", "/library/book.epub", "book"},
		{"plain fb2", "/library/book.fb2", "book"},
		{"fb2.zip strips the whole suffix, not just .zip", "/library/book.fb2.zip", "book"},
		{"mixed case suffix", "/library/Book.FB2.ZIP", "Book"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix := matchedSuffix(tt.path)
			if got := filenameTitle(tt.path, suffix); got != tt.want {
				t.Errorf("filenameTitle(%q, %q) = %q, want %q", tt.path, suffix, got, tt.want)
			}
		})
	}
}

func TestSortTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"leading The", "The Hobbit", "hobbit"},
		{"leading A", "A Wizard of Earthsea", "wizard of earthsea"},
		{"leading An", "An Ideal Husband", "ideal husband"},
		{"article case is ignored", "THE GREAT GATSBY", "great gatsby"},
		{"word merely starting with an article", "Theory of Everything", "theory of everything"},
		{"another near-miss", "Android Dreams", "android dreams"},
		{"already lowercase", "apple book", "apple book"},
		{"mixed case folds", "Zebra Book", "zebra book"},
		{"surrounding whitespace", "  Spaced Out  ", "spaced out"},
		{"only an article", "A", "a"},
		{"article with nothing after it", "The ", "the"},
		{"only whitespace", "   ", ""},
		{"leading digits are kept", "1984", "1984"},
		{"leading punctuation is kept", "'Salem's Lot", "'salem's lot"},
		{"one article only", "The A Team", "a team"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sortTitle(tt.title); got != tt.want {
				t.Errorf("sortTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestScanDerivesSortTitle(t *testing.T) {
	libraryDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libraryDir, "hobbit.epub"), "The Hobbit", "J.R.R. Tolkien", nil)

	if _, err := Scan(ctx, db, libraryDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	books, err := db.ListBooks(ctx, storage.BookPage{})
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("ListBooks returned %d books, want 1", len(books))
	}
	if books[0].Title != "The Hobbit" {
		t.Errorf("Title = %q, want %q", books[0].Title, "The Hobbit")
	}
	if books[0].SortTitle != "hobbit" {
		t.Errorf("SortTitle = %q, want %q — the display title must be kept intact "+
			"while the sort form drops the article", books[0].SortTitle, "hobbit")
	}
}

// A file gone from disk gets its book_files row marked, not deleted — the
// grace period exists precisely so a transient disappearance (an unmounted
// volume, a mid-copy rename) doesn't lose data on the next sweep.
func TestScanMarksMissingFileWithoutDeleting(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	rel := "book.epub"
	path := filepath.Join(libDir, rel)
	writeTestEPUB(t, path, "Book One", "Author A", nil)
	// A surviving sibling keeps Scanned > 0 on the second scan — removing a
	// library's only file is indistinguishable from an unmounted volume, so
	// the empty-sweep guard would otherwise (correctly) skip reconciliation.
	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author B", nil)

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove book: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if result.Missing != 1 || result.Pruned != 0 {
		t.Errorf("second scan = %+v, want Missing=1 Pruned=0", result)
	}

	f, err := db.FindFileByPath(ctx, rel)
	if err != nil {
		t.Fatalf("FindFileByPath: %v", err)
	}
	if f == nil {
		t.Fatal("book_files row was deleted, want it left in place but marked")
	}
	if !f.MissingSince.Valid {
		t.Error("MissingSince is not set, want it set")
	}

	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if bookCount != 2 {
		t.Errorf("books count = %d, want 2 (marking missing must not delete the book)", bookCount)
	}
}

// A file that reappears (the volume remounts, the rename completes) must
// have its missing mark cleared rather than carry a stale timer toward
// eventual deletion.
func TestScanClearsMissingWhenFileReappears(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	rel := "book.epub"
	path := filepath.Join(libDir, rel)
	writeTestEPUB(t, path, "Book One", "Author A", nil)
	// A surviving sibling keeps Scanned > 0 while book.epub is gone —
	// otherwise the empty-sweep guard would (correctly) skip reconciliation.
	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author B", nil)

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture before removal: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove book: %v", err)
	}

	if second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	} else if second.Missing != 1 {
		t.Fatalf("second scan = %+v, want Missing=1", second)
	}

	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("restore book: %v", err)
	}

	third, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if third.Missing != 0 || third.Pruned != 0 {
		t.Errorf("third scan = %+v, want Missing=0 Pruned=0", third)
	}

	f, err := db.FindFileByPath(ctx, rel)
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	if f.MissingSince.Valid {
		t.Error("MissingSince is still set, want it cleared once the file reappeared")
	}
}

// Past the grace period, a still-missing file's row — and its book, since
// this is its only location — is actually deleted.
func TestScanPrunesMissingFileAfterGracePeriod(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	rel := "book.epub"
	path := filepath.Join(libDir, rel)
	writeTestEPUB(t, path, "Book One", "Author A", nil)
	// A surviving sibling keeps Scanned > 0 while book.epub is gone —
	// otherwise the empty-sweep guard would (correctly) skip reconciliation.
	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author B", nil)

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove book: %v", err)
	}

	if second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	} else if second.Missing != 1 {
		t.Fatalf("second scan = %+v, want Missing=1", second)
	}

	// Backdate missing_since well past any grace period, rather than
	// sleeping, so the third scan finds it already eligible for pruning.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = ? WHERE file_path = ?`,
			"2020-01-01T00:00:00.000000000Z", rel)
		return err
	}); err != nil {
		t.Fatalf("backdate missing_since: %v", err)
	}

	third, err := Scan(ctx, db, libDir, coversDir, time.Hour)
	if err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if third.Pruned != 1 {
		t.Errorf("third scan = %+v, want Pruned=1", third)
	}

	f, err := db.FindFileByPath(ctx, rel)
	if err != nil {
		t.Fatalf("FindFileByPath: %v", err)
	}
	if f != nil {
		t.Error("book_files row still exists, want it deleted past the grace period")
	}

	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if bookCount != 1 {
		t.Errorf("books count = %d, want 1 (book.epub's book is gone; the sibling's survives)", bookCount)
	}
}

// The single most important test here: a sweep that walked nothing at all —
// an unmounted volume presenting as an empty directory, say — must not be
// read as "every known file just vanished." Nothing gets marked or pruned.
func TestScanOfEmptyDirectoryPrunesNothing(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book1.epub"), "Book One", "Author A", nil)
	writeTestEPUB(t, filepath.Join(libDir, "book2.epub"), "Book Two", "Author B", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 2 {
		t.Fatalf("first scan = %+v, want New=2", first)
	}

	// Back-date book1's row well past any grace period, so it's already
	// eligible for deletion by age alone — proving the empty-sweep guard
	// itself blocks pruning, not just that nothing new happened to have
	// aged out yet.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = ? WHERE file_path = ?`,
			"2020-01-01T00:00:00.000000000Z", "book1.epub")
		return err
	}); err != nil {
		t.Fatalf("backdate missing_since: %v", err)
	}

	emptyDir := t.TempDir()
	result, err := Scan(ctx, db, emptyDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan of empty dir: %v", err)
	}
	if result.Scanned != 0 || result.Missing != 0 || result.Pruned != 0 {
		t.Errorf("scan of empty dir = %+v, want Scanned=0 Missing=0 Pruned=0", result)
	}

	if f, err := db.FindFileByPath(ctx, "book1.epub"); err != nil || f == nil {
		t.Fatalf("FindFileByPath book1.epub: %+v, %v (want it to survive despite being overdue)", f, err)
	}

	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if bookCount != 2 {
		t.Errorf("books count = %d, want 2 (an empty-looking sweep must not touch anything, even an overdue row)", bookCount)
	}
}

// A file under a directory the sweep couldn't read this time must not be
// marked missing — the sweep has no evidence either way for it, so it's
// left exactly as it was.
func TestScanUnreadableSubdirectoryPrunesNothingUnderIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory mode bits aren't enforced")
	}

	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	// A readable sibling keeps Scanned > 0 on the second scan, so this test
	// exercises the skippedDirs guard specifically — not the separate
	// Scanned==0 "empty library" guard that TestScanOfEmptyDirectoryPrunes-
	// Nothing already covers.
	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author B", nil)

	restrictedDir := filepath.Join(libDir, "restricted")
	if err := os.Mkdir(restrictedDir, 0o755); err != nil {
		t.Fatalf("mkdir restricted: %v", err)
	}
	writeTestEPUB(t, filepath.Join(restrictedDir, "hidden.epub"), "Hidden Book", "Author A", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 2 {
		t.Fatalf("first scan = %+v, want New=2", first)
	}

	// Back-date hidden.epub's row well past any grace period — as if it had
	// already been marked missing once before and were now sitting overdue —
	// so this test proves the skippedDirs guard itself blocks pruning, not
	// just that nothing new happened to have aged out yet.
	hidden, err := db.FindFileByPath(ctx, "restricted/hidden.epub")
	if err != nil || hidden == nil {
		t.Fatalf("FindFileByPath restricted/hidden.epub: %+v, %v", hidden, err)
	}
	if err := db.SetFilesMissing(ctx, []int64{hidden.ID}, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("backdate hidden.epub missing_since: %v", err)
	}

	if err := os.Chmod(restrictedDir, 0o000); err != nil {
		t.Fatalf("chmod restricted: %v", err)
	}
	t.Cleanup(func() { os.Chmod(restrictedDir, 0o755) })

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if result.Scanned == 0 {
		t.Fatal("Scanned = 0, want the sibling file still visited (this test targets the skippedDirs guard, not the empty-sweep guard)")
	}
	if result.Missing != 0 || result.Pruned != 0 {
		t.Errorf("second scan = %+v, want Missing=0 Pruned=0", result)
	}

	f, err := db.FindFileByPath(ctx, "restricted/hidden.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v (want it to survive despite being overdue)", f, err)
	}
	if !f.MissingSince.Valid {
		t.Error("MissingSince was cleared, want the pre-existing backdated mark left exactly as it was")
	}
}

// An offline sub-mount — a second bind mount, an NFS share in a subfolder,
// an Unraid disk mid-rebuild — presents as a directory the walk reads
// cleanly and finds empty, so nothing lands in skippedDirs and every row
// under it fails Lstat with ErrNotExist, the very signal reconciliation
// otherwise trusts. Its rows are marked like any other absent row, so the
// detail page annotates them and the sender skips them, but must never be
// pruned however long the directory stays empty: manual edits, provenance
// and enrichment results are what pruning would destroy, and none of them
// can be rebuilt from the files when the disk returns. Going offline is
// simulated by moving the content out of the library and back, so the
// remount lands byte-identical files at their old paths
func TestScanSubdirectoryWithNoFilesPrunesNothingUnderIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// offline moves the library's b/ content into stash; online moves
		// it back
		offline, online func(t *testing.T, libDir, stash string)
	}{
		{"directory left empty",
			func(t *testing.T, libDir, stash string) {
				if err := os.Rename(filepath.Join(libDir, "b", "gone.epub"), filepath.Join(stash, "gone.epub")); err != nil {
					t.Fatalf("move gone.epub out: %v", err)
				}
			},
			func(t *testing.T, libDir, stash string) {
				if err := os.Rename(filepath.Join(stash, "gone.epub"), filepath.Join(libDir, "b", "gone.epub")); err != nil {
					t.Fatalf("move gone.epub back: %v", err)
				}
			}},
		{"directory removed",
			func(t *testing.T, libDir, stash string) {
				if err := os.Rename(filepath.Join(libDir, "b"), filepath.Join(stash, "b")); err != nil {
					t.Fatalf("move b out: %v", err)
				}
			},
			func(t *testing.T, libDir, stash string) {
				if err := os.Rename(filepath.Join(stash, "b"), filepath.Join(libDir, "b")); err != nil {
					t.Fatalf("move b back: %v", err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			libDir := t.TempDir()
			coversDir := t.TempDir()
			stash := t.TempDir()
			db := openTestDB(t)
			ctx := context.Background()

			for _, dir := range []string{"a", "b"} {
				if err := os.Mkdir(filepath.Join(libDir, dir), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			writeTestEPUB(t, filepath.Join(libDir, "a", "kept.epub"), "Kept Book", "Author A", nil)
			writeTestEPUB(t, filepath.Join(libDir, "b", "gone.epub"), "Gone Book", "Author B", nil)

			if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
				t.Fatalf("first Scan: %v", err)
			} else if first.New != 2 {
				t.Fatalf("first scan = %+v, want New=2", first)
			}

			tc.offline(t, libDir, stash)

			second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
			if err != nil {
				t.Fatalf("second Scan: %v", err)
			}
			if second.Scanned != 1 || second.Missing != 1 || second.Pruned != 0 || second.Unconfirmed != 1 {
				t.Errorf("second scan = %+v, want Scanned=1 Missing=1 Pruned=0 Unconfirmed=1", second)
			}
			gone, err := db.FindFileByPath(ctx, "b/gone.epub")
			if err != nil || gone == nil {
				t.Fatalf("FindFileByPath b/gone.epub: %+v, %v (want the row left in place)", gone, err)
			}
			if !gone.MissingSince.Valid {
				t.Error("b/gone.epub is not marked missing, want it marked: the mark is what keeps the sender off a path that is not there")
			}

			// Long overdue, the row must still survive — the guard is about
			// evidence, not age
			if err := db.SetFilesMissing(ctx, []int64{gone.ID}, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("backdate missing_since: %v", err)
			}
			third, err := Scan(ctx, db, libDir, coversDir, time.Hour)
			if err != nil {
				t.Fatalf("third Scan: %v", err)
			}
			if third.Missing != 0 || third.Pruned != 0 || third.Unconfirmed != 1 {
				t.Errorf("third scan = %+v, want Missing=0 Pruned=0 Unconfirmed=1", third)
			}
			if f, err := db.FindFileByPath(ctx, "b/gone.epub"); err != nil || f == nil {
				t.Fatalf("FindFileByPath b/gone.epub past grace: %+v, %v (want it to survive despite being overdue)", f, err)
			} else if !f.MissingSince.Valid {
				t.Error("MissingSince was cleared, want the pre-existing mark left exactly as it was")
			}
			if kept, err := db.FindFileByPath(ctx, "a/kept.epub"); err != nil || kept == nil {
				t.Fatalf("FindFileByPath a/kept.epub: %+v, %v", kept, err)
			} else if kept.MissingSince.Valid {
				t.Error("a/kept.epub was marked missing, want the populated directory's row untouched")
			}
			var bookCount int
			if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
				t.Fatalf("count books: %v", err)
			}
			if bookCount != 2 {
				t.Errorf("books count = %d, want 2 (an offline directory must not cost its books)", bookCount)
			}

			// The disk returns: the same bytes at the same path clear the
			// mark on the next sweep, with nothing re-indexed
			tc.online(t, libDir, stash)
			fourth, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
			if err != nil {
				t.Fatalf("fourth Scan: %v", err)
			}
			if fourth.Scanned != 2 || fourth.New != 0 || fourth.Missing != 0 || fourth.Unconfirmed != 0 {
				t.Errorf("fourth scan = %+v, want Scanned=2 New=0 Missing=0 Unconfirmed=0", fourth)
			}
			if f, err := db.FindFileByPath(ctx, "b/gone.epub"); err != nil || f == nil {
				t.Fatalf("FindFileByPath b/gone.epub after remount: %+v, %v", f, err)
			} else if f.MissingSince.Valid {
				t.Error("b/gone.epub is still marked missing after it came back, want the mark cleared")
			}
		})
	}
}

// The guard is about a directory that yielded nothing, not about any
// missing row: a file removed from a directory that keeps another is a
// deletion the sweep has every reason to trust, so it still reconciles in
// the ordinary two phases
func TestScanPrunesFileRemovedFromDirectoryThatKeepsAnother(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	for _, dir := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(libDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestEPUB(t, filepath.Join(libDir, "a", "kept.epub"), "Kept Book", "Author A", nil)
	writeTestEPUB(t, filepath.Join(libDir, "b", "stays.epub"), "Staying Book", "Author B", nil)
	writeTestEPUB(t, filepath.Join(libDir, "b", "gone.epub"), "Gone Book", "Author C", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 3 {
		t.Fatalf("first scan = %+v, want New=3", first)
	}

	if err := os.Remove(filepath.Join(libDir, "b", "gone.epub")); err != nil {
		t.Fatalf("remove gone.epub: %v", err)
	}

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Missing != 1 || second.Pruned != 0 || second.Unconfirmed != 0 {
		t.Errorf("second scan = %+v, want Missing=1 Pruned=0 Unconfirmed=0", second)
	}

	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = ? WHERE file_path = ?`,
			"2020-01-01T00:00:00.000000000Z", "b/gone.epub")
		return err
	}); err != nil {
		t.Fatalf("backdate missing_since: %v", err)
	}

	third, err := Scan(ctx, db, libDir, coversDir, time.Hour)
	if err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if third.Pruned != 1 || third.Unconfirmed != 0 {
		t.Errorf("third scan = %+v, want Pruned=1 Unconfirmed=0", third)
	}
	if f, err := db.FindFileByPath(ctx, "b/gone.epub"); err != nil {
		t.Fatalf("FindFileByPath b/gone.epub: %v", err)
	} else if f != nil {
		t.Error("b/gone.epub still exists, want it pruned: its directory kept a file, so its absence is confirmed")
	}
}

// A book moved up from a/novels/ to a/ leaves a/novels/ empty, and that is
// a real reorganisation, not an offline mount: a/ still has files, so the
// old row is confirmed and marked. Narrowing the guard from the top-level
// directory to the row's own directory is what this test refuses
func TestScanMarksMissingWhenFileMovedOutOfNowEmptyNestedDirectory(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	if err := os.MkdirAll(filepath.Join(libDir, "a", "novels"), 0o755); err != nil {
		t.Fatalf("mkdir a/novels: %v", err)
	}
	oldPath := filepath.Join(libDir, "a", "novels", "book.epub")
	writeTestEPUB(t, oldPath, "Book One", "Author A", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 1 {
		t.Fatalf("first scan = %+v, want New=1", first)
	}

	if err := os.Rename(oldPath, filepath.Join(libDir, "a", "book.epub")); err != nil {
		t.Fatalf("move book: %v", err)
	}

	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Moved != 1 || second.Missing != 1 || second.Unconfirmed != 0 {
		t.Errorf("second scan = %+v, want Moved=1 Missing=1 Unconfirmed=0", second)
	}
	if f, err := db.FindFileByPath(ctx, "a/novels/book.epub"); err != nil || f == nil {
		t.Fatalf("FindFileByPath a/novels/book.epub: %+v, %v (want it marked, not deleted yet)", f, err)
	} else if !f.MissingSince.Valid {
		t.Error("a/novels/book.epub is not marked missing, want it marked: a/ still has files, so the move is confirmed")
	}
	if f, err := db.FindFileByPath(ctx, "a/book.epub"); err != nil || f == nil {
		t.Fatalf("FindFileByPath a/book.epub: %+v, %v", f, err)
	} else if f.MissingSince.Valid {
		t.Error("a/book.epub is marked missing, want the new location clean")
	}
}

// Deleting a single-directory library's only file makes the root itself
// the directory that yielded nothing, and the Scanned == 0 guard answers
// first: the row is left alone and not counted as unconfirmed, the root
// case of the same rule rather than a second one
func TestScanOfLibraryWhoseOnlyFileWasDeletedPrunesNothing(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	path := filepath.Join(libDir, "book.epub")
	writeTestEPUB(t, path, "Book One", "Author A", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 1 {
		t.Fatalf("first scan = %+v, want New=1", first)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove book: %v", err)
	}
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = ? WHERE file_path = ?`,
			"2020-01-01T00:00:00.000000000Z", "book.epub")
		return err
	}); err != nil {
		t.Fatalf("backdate missing_since: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir, time.Hour)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if result.Scanned != 0 || result.Missing != 0 || result.Pruned != 0 || result.Unconfirmed != 0 {
		t.Errorf("second scan = %+v, want Scanned=0 Missing=0 Pruned=0 Unconfirmed=0", result)
	}
	if f, err := db.FindFileByPath(ctx, "book.epub"); err != nil || f == nil {
		t.Fatalf("FindFileByPath book.epub: %+v, %v (want it to survive despite being overdue)", f, err)
	}
}

// A non-ErrNotExist failure to stat a path (e.g. a path component that's no
// longer a directory) must not be read as "the file is missing" — only a
// specific ErrNotExist is trusted as proof of absence.
func TestScanDoesNotMarkMissingOnNonNotExistLstatError(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	subDir := filepath.Join(libDir, "top", "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir top/sub: %v", err)
	}
	writeTestEPUB(t, filepath.Join(subDir, "book.epub"), "Book One", "Author A", nil)
	// A sibling inside top/ keeps that directory populated on the second
	// scan — otherwise replacing the only subtree under it trips the
	// unconfirmed-directory guard (and, at the root, the empty-sweep guard)
	// before reconcileMissing ever reaches the Lstat call this test means
	// to exercise, and the test would pass vacuously.
	writeTestEPUB(t, filepath.Join(libDir, "top", "sibling.epub"), "Sibling Book", "Author B", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 2 {
		t.Fatalf("first scan = %+v, want New=2", first)
	}

	// Replace the directory itself with a plain file of the same name, so a
	// later Lstat on the stored nested path ("top/sub/book.epub") fails with
	// ENOTDIR rather than ErrNotExist — deterministic and root-independent,
	// unlike a permissions-based approach.
	if err := os.RemoveAll(subDir); err != nil {
		t.Fatalf("remove sub: %v", err)
	}
	if err := os.WriteFile(subDir, []byte("not a directory anymore"), 0o644); err != nil {
		t.Fatalf("replace sub with a file: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if result.Scanned == 0 {
		t.Fatal("Scanned = 0, want the sibling file still visited (this test targets the ENOTDIR guard, not the empty-sweep guard)")
	}
	if result.Missing != 0 {
		t.Errorf("second scan = %+v, want Missing=0 (ENOTDIR is not proof of absence)", result)
	}
	if result.Unconfirmed != 0 {
		t.Errorf("second scan = %+v, want Unconfirmed=0 (top/ kept a file, so the row must reach Lstat)", result)
	}

	f, err := db.FindFileByPath(ctx, "top/sub/book.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	if f.MissingSince.Valid {
		t.Error("MissingSince is set, want it untouched on a non-ErrNotExist stat failure")
	}
}

// A row marked missing on the strength of a confirmed ErrNotExist must not
// be pruned on the strength of that same confirmation forever — if its
// path's failure mode changes while it waits out the grace period (here,
// the file's parent directory is later replaced by a plain file, so Lstat
// starts failing with ENOTDIR instead), pruning must re-confirm ErrNotExist
// this exact sweep, not just trust an aging, no-longer-checked mark.
func TestScanDoesNotPruneWhenCurrentSweepCannotReconfirmAbsence(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	subDir := filepath.Join(libDir, "top", "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir top/sub: %v", err)
	}
	writeTestEPUB(t, filepath.Join(subDir, "book.epub"), "Book One", "Author A", nil)
	// A sibling inside top/ keeps that directory populated throughout, so
	// the row reaches Lstat rather than the unconfirmed-directory guard
	writeTestEPUB(t, filepath.Join(libDir, "top", "sibling.epub"), "Sibling Book", "Author B", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 2 {
		t.Fatalf("first scan = %+v, want New=2", first)
	}

	// Remove just the nested book — a genuine ErrNotExist — so the second
	// scan marks it missing legitimately.
	if err := os.Remove(filepath.Join(subDir, "book.epub")); err != nil {
		t.Fatalf("remove book: %v", err)
	}
	if second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	} else if second.Missing != 1 {
		t.Fatalf("second scan = %+v, want Missing=1", second)
	}

	// Now replace "sub" itself with a plain file, so a later Lstat on
	// "top/sub/book.epub" fails with ENOTDIR — a different failure mode than
	// the ErrNotExist that earned the mark in the first place.
	if err := os.RemoveAll(subDir); err != nil {
		t.Fatalf("remove sub: %v", err)
	}
	if err := os.WriteFile(subDir, []byte("not a directory anymore"), 0o644); err != nil {
		t.Fatalf("replace sub with a file: %v", err)
	}

	// Back-date the existing mark well past grace, as if it had been
	// sitting missing for a long time before this happened.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = ? WHERE file_path = ?`,
			"2020-01-01T00:00:00.000000000Z", "top/sub/book.epub")
		return err
	}); err != nil {
		t.Fatalf("backdate missing_since: %v", err)
	}

	third, err := Scan(ctx, db, libDir, coversDir, time.Hour)
	if err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if third.Scanned == 0 {
		t.Fatal("Scanned = 0, want the sibling file still visited")
	}
	if third.Pruned != 0 {
		t.Errorf("third scan = %+v, want Pruned=0 (ENOTDIR this sweep must not honor an old ErrNotExist confirmation)", third)
	}
	if third.Unconfirmed != 0 {
		t.Errorf("third scan = %+v, want Unconfirmed=0 (top/ kept a file, so the row must reach Lstat)", third)
	}

	f, err := db.FindFileByPath(ctx, "top/sub/book.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v (want it to survive — this sweep could not reconfirm absence)", f, err)
	}
}

// underAny's skippedDirs check must protect a book_files row whose path is
// exactly a skipped directory's own path, not just its descendants — a
// prefix-only match ("dir/%") would miss this case, since the row's
// file_path is identical to the directory's relative path, no trailing
// segment at all. This is the exact-equality half of the guard that
// TestScanUnreadableSubdirectoryPrunesNothingUnderIt (a descendant,
// restricted/hidden.epub) doesn't exercise.
func TestScanDoesNotPruneOverdueRowAtExactlyAnUnreadableDirectorysPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory mode bits aren't enforced")
	}

	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	targetPath := filepath.Join(libDir, "target.epub")
	writeTestEPUB(t, targetPath, "Target Book", "Author A", nil)
	// A readable sibling keeps Scanned > 0 throughout.
	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author B", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 2 {
		t.Fatalf("first scan = %+v, want New=2", first)
	}

	// Remove target.epub — a genuine ErrNotExist — so it gets marked missing
	// legitimately.
	if err := os.Remove(targetPath); err != nil {
		t.Fatalf("remove target.epub: %v", err)
	}
	if second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	} else if second.Missing != 1 {
		t.Fatalf("second scan = %+v, want Missing=1", second)
	}

	// Back-date it well past grace.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = ? WHERE file_path = ?`,
			"2020-01-01T00:00:00.000000000Z", "target.epub")
		return err
	}); err != nil {
		t.Fatalf("backdate missing_since: %v", err)
	}

	// Now something occupies exactly that path: an unreadable directory
	// named "target.epub" — not a subdirectory containing a file of that
	// name, the literal path itself.
	if err := os.Mkdir(targetPath, 0o755); err != nil {
		t.Fatalf("mkdir target.epub: %v", err)
	}
	if err := os.Chmod(targetPath, 0o000); err != nil {
		t.Fatalf("chmod target.epub: %v", err)
	}
	t.Cleanup(func() { os.Chmod(targetPath, 0o755) })

	third, err := Scan(ctx, db, libDir, coversDir, time.Hour)
	if err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if third.Scanned == 0 {
		t.Fatal("Scanned = 0, want the sibling file still visited")
	}
	if third.Pruned != 0 {
		t.Errorf("third scan = %+v, want Pruned=0 (the row's path exactly matches an unreadable directory, not just a descendant)", third)
	}

	f, err := db.FindFileByPath(ctx, "target.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v (want it to survive — exact-path match with a skipped directory)", f, err)
	}
}

// A pre-cancelled context must not walk anything at all — not even the
// cheap-path check on a single file — since a caller cancelling before
// calling Scan is indistinguishable from one cancelling an instant later,
// and the latter must stop the walk (see the next test).
func TestScanReturnsErrorOnPreCancelledContext(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan error = %v, want context.Canceled", err)
	}
	if result.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0 (a pre-cancelled context must not walk anything)", result.Scanned)
	}
}

// Cancelling mid-sweep must stop the walk from visiting further entries,
// not just make each remaining file's own DB calls fail one at a time —
// the latter would still cost a full directory traversal, a stat, and a
// warning per remaining file on every shutdown of a large library.
func TestScanStopsOnCancellationMidSweep(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	const total = 20
	for i := 0; i < total; i++ {
		writeTestEPUB(t, filepath.Join(libDir, fmt.Sprintf("book-%d.epub", i)), fmt.Sprintf("Book %d", i), "Author", nil)
	}

	scanCtx, cancel := context.WithCancel(ctx)
	go func() {
		// Cancel as soon as the walk has demonstrably started (the first
		// book has been committed), leaving most of the 20 files
		// unprocessed — deterministic, unlike a fixed sleep.
		for {
			var count int
			if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&count); err == nil && count > 0 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	result, err := Scan(scanCtx, db, libDir, coversDir, testMissingGrace)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan error = %v, want context.Canceled", err)
	}
	if result.Scanned >= total {
		t.Errorf("Scanned = %d, want fewer than %d — cancellation should have stopped the walk early", result.Scanned, total)
	}
}

// The end-to-end regression for the whole step: on master an FB2 book is a
// card with the filename as its title, no author, and no cover — this
// proves real embedded metadata reaches the book row instead.
func TestScanExtractsFB2Metadata(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestFB2(t, filepath.Join(libDir, "kniga.fb2"), "Real Title", "Real Author", testCoverImage(t))

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 || result.Errors != 0 {
		t.Fatalf("scan = %+v, want New=1 Errors=0", result)
	}

	book := bookByPath(t, ctx, db, "kniga.fb2")
	if book.Title != "Real Title" {
		t.Errorf("Title = %q, want %q (not the filename)", book.Title, "Real Title")
	}
	if book.Format != "fb2" {
		t.Errorf("Format = %q, want %q", book.Format, "fb2")
	}
	if book.CoverPath == "" {
		t.Error("CoverPath is empty, want a stored cover path")
	}

	authors, err := db.ListBookAuthors(ctx)
	if err != nil {
		t.Fatalf("ListBookAuthors: %v", err)
	}
	if got := authors[book.ID]; len(got) != 1 || got[0] != "Real Author" {
		t.Errorf("authors = %v, want [Real Author]", got)
	}
}

// On master a .fb2.zip file is invisible to the scanner entirely — the walk
// filter's filepath.Ext only ever sees ".zip", which isn't a supported
// extension, so the file is never even reported as skipped. This asserts
// the book count, not just its fields, since "not indexed at all" wouldn't
// otherwise show up as a field-level assertion failure.
func TestScanIndexesFB2Zip(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestFB2Zip(t, filepath.Join(libDir, "kniga.fb2.zip"), "Zipped Title", "Zipped Author", nil)

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 || result.Errors != 0 {
		t.Fatalf("scan = %+v, want New=1 Errors=0", result)
	}

	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if bookCount != 1 {
		t.Fatalf("books count = %d, want 1 (a .fb2.zip file must be indexed)", bookCount)
	}

	book := bookByPath(t, ctx, db, "kniga.fb2.zip")
	if book.Title != "Zipped Title" {
		t.Errorf("Title = %q, want %q", book.Title, "Zipped Title")
	}
	if book.Format != "fb2" {
		t.Errorf("Format = %q, want %q (how the book is packaged on disk isn't the format badge)", book.Format, "fb2")
	}
}

// An unparseable FB2 file degrades to the filename title, the same way a
// bad EPUB does — it must not count as a scan error, and it must not stop
// the book from appearing in the library at all.
func TestScanFallsBackToFilenameTitleOnUnparseableFB2(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	path := filepath.Join(libDir, "broken.fb2")
	if err := os.WriteFile(path, []byte("this is not xml at all"), 0o644); err != nil {
		t.Fatalf("write broken fb2: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 || result.Errors != 0 {
		t.Fatalf("scan = %+v, want New=1 Errors=0 (an unparseable file degrades, it doesn't fail the sweep)", result)
	}

	book := bookByPath(t, ctx, db, "broken.fb2")
	if book.Title != "broken" {
		t.Errorf("Title = %q, want %q (the filename fallback)", book.Title, "broken")
	}
}

// giveProviderCover puts the book into the state a provider-fetched cover
// leaves: a cover_path, and the field_sources row that is the only way to
// tell a provider's cover from one the scanner extracted.
//
// Through ApplyEnrichedFields, which is what the enrichment worker calls and
// the only writer that *creates* a cover row (ClearProviderCover and
// UpdateBookCoverPath both remove one) — and it must run while cover_path is
// still empty, since its own re-check skips a field that is no longer
// missing and would then write the path without the provenance.
func giveProviderCover(t *testing.T, ctx context.Context, db *storage.DB, bookID int64, path string) {
	t.Helper()
	written, _, err := db.ApplyEnrichedFields(ctx, bookID,
		map[storage.MetadataField]string{storage.FieldCover: path},
		map[storage.MetadataField]string{storage.FieldCover: "openlibrary"}, time.Now())
	if err != nil {
		t.Fatalf("ApplyEnrichedFields: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("ApplyEnrichedFields wrote %v, want the cover — the book must have no cover_path yet", written)
	}
	sources, err := db.FieldSourcesForBook(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if sources[storage.FieldCover] != "openlibrary" {
		t.Fatalf("cover source = %q, want openlibrary", sources[storage.FieldCover])
	}
}

// A provider-supplied cover has no embedded original, so a sweep that finds
// its file gone must forget it rather than try to re-extract it forever.
// The second sweep is what actually pins this: the first would also pass if
// the fix were merely to quieten the log line, leaving cover_path dangling
// and the grid rendering a broken image.
func TestScanForgetsAProviderCoverWhoseFileIsGone(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	// No embedded cover: this book's only cover ever came from a provider.
	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	coverPath := filepath.Join(coversDir, "provider.jpg")
	if err := os.WriteFile(coverPath, testCoverImage(t), 0o644); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, coverPath)
	if err := os.Remove(coverPath); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.CoversRegenerated != 0 || second.Errors != 0 {
		t.Errorf("second scan = %+v, want CoversRegenerated=0 Errors=0", second)
	}
	// The counters cannot see a fall-through into readEmbeddedCover — it
	// bumps neither — so the warning itself is the assertion. Reinstating
	// that line is exactly the loop this step exists to end.
	if got := logs.String(); strings.Contains(got, "regenerate cover failed") {
		t.Errorf("sweep logged a regeneration failure for a provider cover:\n%s", got)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != "" {
		t.Errorf("CoverPath = %q, want empty — a path naming a file that is gone renders a broken image", after.CoverPath)
	}
	if after.CoverRetry {
		t.Error("CoverRetry set; there is no embedded original to retry")
	}
	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src, ok := sources[storage.FieldCover]; ok {
		t.Errorf("cover source = %q, want the row gone with the value", src)
	}

	// The assertion that distinguishes forgetting from quietening: with the
	// path cleared, the third sweep short-circuits before any cover work at
	// all, so the book has stopped costing anything on every sweep forever.
	third, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if third.CoversRegenerated != 0 || third.Errors != 0 {
		t.Errorf("third scan = %+v, want CoversRegenerated=0 Errors=0", third)
	}
	stillAfter, err := db.FindBookByID(ctx, book.ID)
	if err != nil || stillAfter == nil {
		t.Fatalf("FindBookByID: %+v, %v", stillAfter, err)
	}
	if stillAfter.CoverPath != "" {
		t.Errorf("CoverPath = %q after a third sweep, want empty", stillAfter.CoverPath)
	}
}

// A zero-byte file takes the same path as a missing one — both mean the
// stored thumbnail is unusable, and neither can be re-extracted for a
// provider's cover.
func TestScanForgetsAZeroByteProviderCover(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	coverPath := filepath.Join(coversDir, "provider.jpg")
	if err := os.WriteFile(coverPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, coverPath)

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != "" {
		t.Errorf("CoverPath = %q, want empty", after.CoverPath)
	}
}

// cover_retry makes the scanner skip its stat check entirely, so a
// provider's cover carrying that marker must still be forgotten rather than
// sent into an extraction that has nothing to read.
func TestScanForgetsAProviderCoverMarkedForRetry(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	giveProviderCover(t, ctx, db, book.ID, filepath.Join(coversDir, "gone.jpg"))
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_retry = 1 WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != "" || after.CoverRetry {
		t.Errorf("CoverPath = %q, CoverRetry = %v, want empty and false", after.CoverPath, after.CoverRetry)
	}
}

// A storage error says nothing about where the cover came from. Guessing
// "provider" would silently discard a scanner-extracted cover; guessing
// "embedded" would re-enter the extraction loop this exists to end. So an
// unreadable provenance leaves the book exactly as it is and lets the next
// sweep try again — the same posture missing-file reconciliation takes
// toward an ambiguous Lstat.
//
// The book has no embedded cover, which is what routes it to the provenance
// read at all: a book that can simply be re-extracted never gets there.
func TestScanLeavesTheCoverAloneWhenProvenanceIsUnreadable(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	coverPath := filepath.Join(coversDir, "provider.jpg")
	if err := os.WriteFile(coverPath, testCoverImage(t), 0o644); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, coverPath)
	if err := os.Remove(coverPath); err != nil {
		t.Fatal(err)
	}

	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE field_sources`)
		return err
	}); err != nil {
		t.Fatalf("drop field_sources: %v", err)
	}

	logs := captureLogs(t)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	// Asserted on the log rather than on the row, because dropping the table
	// also breaks ClearProviderCover's own DELETE: a "clear anyway" bug
	// would roll its transaction back and leave the row looking untouched,
	// so the fixture would be doing the assertion's job.
	//
	// And asserted on the *attempt*, not the outcome. "provider cover
	// forgotten" only fires on the success path, which is exactly the path a
	// rolled-back clear skips — so asserting its absence would pass under
	// the bug. "clear provider cover" matches the failure line the rollback
	// does emit, which is what catches the attempt however a future clear is
	// written. (It does not also match "provider cover forgotten"; that
	// outcome is covered by the CoverPath assertion below.)
	if got := logs.String(); !strings.Contains(got, "read cover provenance failed") {
		t.Errorf("sweep did not log the provenance failure:\n%s", got)
	}
	if got := logs.String(); strings.Contains(got, "clear provider cover") {
		t.Errorf("sweep tried to clear a cover on an unreadable provenance:\n%s", got)
	}
	// And says only the true thing. Falling through would add "regenerate
	// cover failed: embedded cover is missing" on top, which is misleading:
	// what failed was the provenance read, and whether this book has an
	// embedded cover was never the question.
	if got := logs.String(); strings.Contains(got, "regenerate cover failed") {
		t.Errorf("sweep also blamed the embedded cover:\n%s", got)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != coverPath {
		t.Errorf("CoverPath = %q, want it left at %q — provenance was unreadable, so nothing is known", after.CoverPath, coverPath)
	}
}

// A provider cover whose file is present must survive a sweep untouched.
//
// The ordinary case, and the one every other test here skips: they all start
// from a cover that is missing, zero-byte or retry-marked. Without this,
// hoisting the provenance read above the os.Stat check — the natural
// refactor, since the two reads look independent — wipes every provider
// cover on every sweep with the whole suite green, and the user re-fetches
// it every fifteen minutes forever. That is this step's own fight, running
// in reverse.
func TestScanKeepsAHealthyProviderCover(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	coverPath := filepath.Join(coversDir, "provider.jpg")
	if err := os.WriteFile(coverPath, testCoverImage(t), 0o644); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, coverPath)

	logs := captureLogs(t)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != coverPath {
		t.Errorf("CoverPath = %q, want %q — a healthy provider cover was wiped", after.CoverPath, coverPath)
	}
	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sources[storage.FieldCover] != "openlibrary" {
		t.Errorf("cover source = %q, want openlibrary — provenance was discarded", sources[storage.FieldCover])
	}
	if got := logs.String(); strings.Contains(got, "provider cover forgotten") {
		t.Errorf("sweep forgot a cover whose file is present:\n%s", got)
	}
	// Nor may it complain: reaching the re-extraction path at all for a
	// healthy cover means the stat check was bypassed, which is the shape of
	// the hoist that would otherwise wipe every provider cover.
	if got := logs.String(); strings.Contains(got, "regenerate cover failed") {
		t.Errorf("sweep tried to re-extract a cover that is present:\n%s", got)
	}
	if _, err := os.Stat(coverPath); err != nil {
		t.Errorf("stat cover: %v — the file itself must be left alone", err)
	}
}

// The guard against an invariant this package cannot enforce: cover_retry
// set beside a provider row, for a book whose file holds no embedded cover.
// maybeRegenerateCover skips its stat entirely when the marker is set, so
// without confirming the file is actually unusable, a present and perfectly
// good provider cover would be thrown away on the strength of the marker
// alone.
//
// Only that half. A book in the same state whose file *does* hold an
// embedded cover never reaches the guard — it re-extracts and overwrites
// the path instead. See forgetUnregenerableCover for why that is left
// alone.
//
// The state should not arise — updateBookColumnTx clears the marker in the
// same statement that writes a path — but that invariant lives in another
// package, so it is constructed here deliberately rather than trusted.
func TestScanKeepsAPresentProviderCoverMarkedForRetry(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	coverPath := filepath.Join(coversDir, "provider.jpg")
	if err := os.WriteFile(coverPath, testCoverImage(t), 0o644); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, coverPath)
	// The violation, set directly: a marker beside a path that is fine.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_retry = 1 WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != coverPath {
		t.Errorf("CoverPath = %q, want %q — the file was present, so there was no evidence to clear on", after.CoverPath, coverPath)
	}
	if got := logs.String(); strings.Contains(got, "provider cover forgotten") {
		t.Errorf("a present cover was forgotten on the strength of cover_retry alone:\n%s", got)
	}
}

// A book can hold an embedded cover *and* a provider provenance row, so
// "there is a provider row" does not imply "there is nothing to
// re-extract". The route: cover.Store fails when the book is first seen,
// leaving cover_retry set, cover_path empty and — since
// setEmbeddedFieldSourcesTx never writes a cover row — no provenance
// either; enrichment then supplies a cover of its own, creating the row.
// The embedded original was there the whole time.
//
// Such a book must be regenerated from its own file, not forgotten. Testing
// it rather than reasoning about it, because the earlier shape of this code
// inferred the one from the other and would have discarded the cover.
func TestScanRegeneratesAnEmbeddedCoverDespiteProviderProvenance(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	// The fixture carries a real embedded cover, so the first sweep stores
	// one and records no provenance for it.
	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", testCoverImage(t))
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	if book.CoverPath == "" {
		t.Fatal("no cover extracted; the fixture must carry one")
	}

	// Put a provider row on it, as an enrichment run that had supplied the
	// cover would. Clearing the column first is what lets
	// ApplyEnrichedFields' own missing-check write the row at all.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_path = '' WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, book.CoverPath)
	if err := os.Remove(book.CoverPath); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.CoversRegenerated != 1 {
		t.Errorf("second scan = %+v, want CoversRegenerated=1 — the book's own cover was recoverable", second)
	}
	if got := logs.String(); strings.Contains(got, "provider cover forgotten") {
		t.Errorf("a regenerable cover was forgotten:\n%s", got)
	}
	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath == "" {
		t.Error("CoverPath is empty; the embedded cover should have been re-extracted")
	}
	if _, err := os.Stat(after.CoverPath); err != nil {
		t.Errorf("stat regenerated cover: %v", err)
	}
	// The image on disk is now the scanner's own re-extraction, so the row
	// claiming a provider supplied it has to go with it. Leaving it is the
	// state Decision 1's table forbids — "row present means a provider
	// supplied it" — and it has a bite: a book in that state that later hits
	// a read failure would be treated as a provider cover.
	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src, ok := sources[storage.FieldCover]; ok {
		t.Errorf("cover source = %q after re-extraction, want no row — this cover is the scanner's", src)
	}
}

// A read *error* is not evidence that the book holds no cover, so it must
// never reach the forget branch.
//
// The loss it would cause is silent and permanent: once cover_path is
// empty, maybeRegenerateCover returns at its first guard on every later
// sweep, so the embedded original is never recovered even after the read
// starts working. The only repair left is Fetch, which brings back the
// provider's image rather than the book's own — and COVERS_DIR stops being
// disposable for that book, by a different door than the one this step
// closed.
//
// The book is corrupted in place at identical size and mtime, so the cheap
// path+size+mtime check still routes it here rather than treating it as
// changed content.
func TestScanKeepsAProviderCoverWhenTheBookCannotBeRead(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	bookPath := filepath.Join(libDir, "book.epub")
	writeTestEPUB(t, bookPath, "Book One", "Author A", testCoverImage(t))
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_path = '' WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, book.CoverPath)
	if err := os.Remove(book.CoverPath); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(bookPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := make([]byte, info.Size())
	for i := range corrupt {
		corrupt[i] = 'x'
	}
	if err := os.WriteFile(bookPath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bookPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != book.CoverPath {
		t.Errorf("CoverPath = %q, want it left at %q — the read failed, so nothing is known about the book's own cover", after.CoverPath, book.CoverPath)
	}
	if got := logs.String(); strings.Contains(got, "clear provider cover") {
		t.Errorf("sweep tried to clear a cover after a failed read:\n%s", got)
	}
	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sources[storage.FieldCover] != "openlibrary" {
		t.Errorf("cover source = %q, want openlibrary — provenance was discarded on an unknown", sources[storage.FieldCover])
	}
}

// An ambiguous stat on the covers directory is not evidence the file is
// gone either. Reached through the cover_retry route, where
// maybeRegenerateCover's own stat is skipped, so coverFileDefinitelyGone is
// the only thing asking.
func TestScanKeepsAProviderCoverWhenTheCoverStatIsAmbiguous(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; an unreadable directory is still readable")
	}
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	// A subdirectory whose traverse bit is removed makes os.Stat fail with
	// EACCES rather than ErrNotExist — the file may well be there.
	sub := filepath.Join(coversDir, "locked")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	coverPath := filepath.Join(sub, "provider.jpg")
	if err := os.WriteFile(coverPath, testCoverImage(t), 0o644); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, coverPath)
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_retry = 1 WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	logs := captureLogs(t)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != coverPath {
		t.Errorf("CoverPath = %q, want it left at %q — an EACCES says nothing about whether the file is there", after.CoverPath, coverPath)
	}
	if got := logs.String(); strings.Contains(got, "provider cover forgotten") {
		t.Errorf("sweep forgot a cover on an ambiguous stat:\n%s", got)
	}
}

// Orphaned provenance: a cover row with no path stored at all. Nothing
// points anywhere, so there is nothing to protect and the row is a claim
// about a value that does not exist — the sweep drops it.
//
// Reached only through cover_retry, which is what gets a pathless book past
// maybeRegenerateCover's first guard, and constructed directly because the
// combination should not arise on its own.
func TestScanForgetsOrphanedProviderProvenance(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	giveProviderCover(t, ctx, db, book.ID, filepath.Join(coversDir, "gone.jpg"))
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_path = '', cover_retry = 1 WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src, ok := sources[storage.FieldCover]; ok {
		t.Errorf("cover source = %q, want the orphaned row gone — no path was ever stored", src)
	}
	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverRetry {
		t.Error("CoverRetry still set; the next sweep would come straight back")
	}
}

// Decision 1's discriminator, from the other side: a book with **no**
// provenance row must be warned about, never forgotten. Forgetting is
// reserved for a provider's cover, and this one is the scanner's own.
//
// The existing regeneration tests do not cover this. Their books have an
// embedded cover, so they re-extract and return long before reaching the
// branch — satisfying the plan's "the existing re-extract path still runs"
// sentence without guarding the line it is about. Dropping `fromProvider`
// left all fourteen packages green until this existed.
func TestScanNeverForgetsAScannerCover(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	bookPath := filepath.Join(libDir, "book.epub")
	writeTestEPUB(t, bookPath, "Book One", "Author A", testCoverImage(t))
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sources[storage.FieldCover]; ok {
		t.Fatal("fixture is wrong: a scanner-extracted cover must carry no provenance row")
	}

	// The thumbnail goes, and the book is replaced — at identical size and
	// mtime, so the cheap check still skips re-indexing — with one holding
	// no embedded cover. The read then succeeds and returns nothing, which
	// is what routes this book to the branch at all.
	if err := os.Remove(book.CoverPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bookPath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestEPUB(t, bookPath, "Book One", "Author A", nil)
	if err := os.Truncate(bookPath, info.Size()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bookPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath == "" {
		t.Error("cover_path was cleared for a book with no provenance row")
	}
	if got := logs.String(); strings.Contains(got, "provider cover forgotten") {
		t.Errorf("forgot a scanner cover:\n%s", got)
	}
	if got := logs.String(); !strings.Contains(got, "regenerate cover failed") {
		t.Errorf("the sweep should have warned instead:\n%s", got)
	}
}

// bmpCover is an image in a format nothing registers a decoder for: the
// shape of a cover that can never be stored, however many times it is tried
func bmpCover() []byte {
	return append([]byte("BM"), make([]byte, 60)...)
}

// A cover that fails to decode fails the same way on every sweep, so the
// retry marker is the wrong record for it: set, it makes every sweep
// re-open and re-parse the book to reach the same refusal. The book is
// recorded as having no usable cover instead, and later sweeps say nothing
// about it at all
func TestScanRecordsUndecodableCoverWithoutRetry(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", bmpCover())
	logs := captureLogs(t)
	first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if first.New != 1 || first.Errors != 0 {
		t.Fatalf("first scan = %+v, want New=1 Errors=0", first)
	}
	if !strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), "embedded cover unusable") {
		t.Errorf("first scan log = %q, want an Info line recording the unusable cover", logs.String())
	}
	book := bookByPath(t, ctx, db, "book.epub")
	if book.CoverPath != "" || book.CoverRetry {
		t.Fatalf("book after undecodable cover = %+v, want empty path and no retry marker", book)
	}

	logs.Reset()
	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.Unchanged != 1 || second.CoversRegenerated != 0 || second.Errors != 0 {
		t.Errorf("second scan = %+v, want Unchanged=1 CoversRegenerated=0 Errors=0", second)
	}
	if strings.Contains(logs.String(), "cover") {
		t.Errorf("second scan log = %q, want nothing about the cover", logs.String())
	}
}

// A book indexed before decode failures were told apart from I/O ones
// carries the retry marker from that first store. The next sweep's retry
// reaches the same refusal, and that is what finally clears the marker —
// after which the book is left alone
func TestScanClearsLegacyRetryMarkerForUndecodableCover(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", bmpCover())
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_retry = 1 WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	second, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if second.CoversRegenerated != 0 || second.Errors != 0 {
		t.Errorf("second scan = %+v, want CoversRegenerated=0 Errors=0", second)
	}
	if !strings.Contains(logs.String(), "embedded cover unusable") {
		t.Errorf("second scan log = %q, want the unusable cover recorded", logs.String())
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("second scan log = %q, want no warning for a permanent failure", logs.String())
	}
	book = bookByPath(t, ctx, db, "book.epub")
	if book.CoverPath != "" || book.CoverRetry {
		t.Fatalf("book after retry = %+v, want empty path and the marker cleared", book)
	}

	logs.Reset()
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if strings.Contains(logs.String(), "cover") {
		t.Errorf("third scan log = %q, want nothing about the cover", logs.String())
	}
}

// A provider cover whose file has gone is normally rebuilt from the book's
// embedded original. When that original cannot decode, there is nothing to
// rebuild from, and the provider cover is forgotten the same way it would be
// for a book with no embedded cover at all: path and provenance row both,
// so enrichment offers to fetch it again
func TestScanForgetsProviderCoverWhenEmbeddedCoverIsUndecodable(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", bmpCover())
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")
	gone := filepath.Join(coversDir, "provider.jpg")
	if _, _, err := db.ApplyEnrichedFields(ctx, book.ID,
		map[storage.MetadataField]string{storage.FieldCover: gone},
		map[storage.MetadataField]string{storage.FieldCover: "openlibrary"}, time.Now()); err != nil {
		t.Fatalf("ApplyEnrichedFields: %v", err)
	}

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	book = bookByPath(t, ctx, db, "book.epub")
	if book.CoverPath != "" || book.CoverRetry {
		t.Errorf("book after sweep = %+v, want the provider cover forgotten", book)
	}
	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src, ok := sources[storage.FieldCover]; ok {
		t.Errorf("cover source = %q, want the provider row gone", src)
	}
}

// The undecodable-cover branch reaches its write with cover_retry set and the
// stat skipped, so it has no evidence about a stored cover that happens to be
// beside the marker. A present provider cover must survive it: the embedded
// image being undecodable says nothing about the file on disk. Same violated
// invariant TestScanKeepsAPresentProviderCoverMarkedForRetry constructs, for
// the same reason — it lives in another package
func TestScanKeepsAPresentProviderCoverWhenEmbeddedCoverIsUndecodable(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "book.epub"), "Book One", "Author A", bmpCover())
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	book := bookByPath(t, ctx, db, "book.epub")

	coverPath := filepath.Join(coversDir, "provider.jpg")
	if err := os.WriteFile(coverPath, testCoverImage(t), 0o644); err != nil {
		t.Fatal(err)
	}
	giveProviderCover(t, ctx, db, book.ID, coverPath)
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_retry = 1 WHERE id = ?`, book.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != coverPath {
		t.Errorf("CoverPath = %q, want %q — the file was present, so there was no evidence to clear on", after.CoverPath, coverPath)
	}
	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src := sources[storage.FieldCover]; src == "" {
		t.Error("the provider's provenance row was removed on the strength of cover_retry alone")
	}
	if got := logs.String(); !strings.Contains(got, "embedded cover unusable but stored cover present") {
		t.Errorf("scan log = %q, want the refusal warned about", got)
	}
}

// The motivation for cmd/server resolving LIBRARY_DIR before anything sees
// it: filepath.WalkDir Lstats its root and never follows a link, so a
// symlinked root is visited once as a non-directory entry and the walk
// ends. The sweep reports the link, but a report is all it can do — the
// library is empty either way, and only the resolution one layer up gives
// it any books. A change that made Scan resolve its own root should delete
// this test rather than satisfy it
func TestScanOfAnUnresolvedSymlinkedRootIndexesNothing(t *testing.T) {
	target := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(target, "book.epub"), "Book One", "Author A", nil)

	link := filepath.Join(t.TempDir(), "library")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink library root: %v", err)
	}

	logs := captureLogs(t)
	result, err := Scan(ctx, db, link, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan through an unresolved symlinked root: %v", err)
	}
	if result.Scanned != 0 || result.New != 0 {
		t.Errorf("scan = %+v, want Scanned=0 New=0 — WalkDir does not follow the root link", result)
	}
	// The root arrives at the callback as a symlink entry like any other,
	// so the unfollowed-directory branch catches it too: no books, and the
	// link named rather than passed over
	if result.Errors != 1 {
		t.Errorf("Errors = %d, want 1 — the root link itself is reported", result.Errors)
	}
	if got := logs.String(); !strings.Contains(got, "symlinked directory is not followed") {
		t.Errorf("scan log = %q, want the unfollowed root named", got)
	}

	// and the same root, resolved as cmd/server resolves it, is an
	// ordinary library
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	result, err = Scan(ctx, db, resolved, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan of the resolved root: %v", err)
	}
	if result.Scanned != 1 || result.New != 1 || result.Errors != 0 {
		t.Errorf("scan of the resolved root = %+v, want Scanned=1 New=1 Errors=0", result)
	}
	if book := bookByPath(t, ctx, db, "book.epub"); book.Title != "Book One" {
		t.Errorf("Title = %q, want %q", book.Title, "Book One")
	}
}

// A symlinked subdirectory is not followed — following it needs a cycle
// guard and would index files whose relative file_path cannot say where
// they are — but it is named rather than passed over: the entry arrives as
// a non-directory whose name has no supported suffix, so the ordinary
// filter alone would drop it without a word
func TestScanDoesNotFollowASymlinkedSubdirectory(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author A", nil)

	outside := t.TempDir()
	writeTestEPUB(t, filepath.Join(outside, "hidden.epub"), "Hidden Book", "Author B", nil)
	if err := os.Symlink(outside, filepath.Join(libDir, "more")); err != nil {
		t.Fatalf("symlink subdirectory: %v", err)
	}

	logs := captureLogs(t)
	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Scanned != 1 || result.New != 1 {
		t.Errorf("scan = %+v, want Scanned=1 New=1 (the sibling alone)", result)
	}
	if result.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the unfollowed symlinked directory)", result.Errors)
	}
	if got := logs.String(); !strings.Contains(got, "symlinked directory is not followed") {
		t.Errorf("scan log = %q, want the unfollowed directory named", got)
	}
	if f, err := db.FindFileByPath(ctx, "more/hidden.epub"); err != nil || f != nil {
		t.Errorf("FindFileByPath more/hidden.epub = %+v, %v; want it absent", f, err)
	}
}

// A symlinked *file* is indexed as any other: every read of it — the stat,
// the open, the hash — goes through the link, so there is nothing about it
// the index cannot express
func TestScanIndexesASymlinkedFile(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	outside := filepath.Join(t.TempDir(), "elsewhere.epub")
	writeTestEPUB(t, outside, "Linked Book", "Author A", nil)
	if err := os.Symlink(outside, filepath.Join(libDir, "book.epub")); err != nil {
		t.Fatalf("symlink book: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Scanned != 1 || result.New != 1 || result.Errors != 0 {
		t.Errorf("scan = %+v, want Scanned=1 New=1 Errors=0", result)
	}
	if book := bookByPath(t, ctx, db, "book.epub"); book.Title != "Linked Book" {
		t.Errorf("Title = %q, want %q", book.Title, "Linked Book")
	}
}

// A link that resolves to nothing is not a directory, and unlike one that
// cannot be resolved at all it is not an unknown either: ErrNotExist is a
// definite answer, so it takes the ordinary route. With a supported suffix
// that is a per-file error exactly as a deleted file would be, and without
// one it is ignored. Getting this wrong the other way — treating any
// symlink as a directory — would report a stray dangling link as a library
// problem
func TestScanTreatsADanglingSymlinkAsAFile(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Symlink(gone, filepath.Join(libDir, "book.epub")); err != nil {
		t.Fatalf("symlink dangling book: %v", err)
	}
	if err := os.Symlink(gone, filepath.Join(libDir, "notes")); err != nil {
		t.Fatalf("symlink dangling other: %v", err)
	}

	logs := captureLogs(t)
	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Scanned != 1 || result.New != 0 || result.Errors != 1 {
		t.Errorf("scan = %+v, want Scanned=1 New=0 Errors=1 (the dangling .epub alone)", result)
	}
	if got := logs.String(); strings.Contains(got, "symlinked directory is not followed") {
		t.Errorf("a dangling link was reported as a directory:\n%s", got)
	}
}

// A link that cannot be resolved at all — a cycle here, a target whose
// directory denies a stat on a real library — is the third case, and the
// one a bare "does it resolve to a directory" test drops on the floor: it
// is neither a directory to refuse nor a file to index, and its name says
// nothing about which it would have been. An unknown is not evidence, so
// it is reported rather than passed over, the same posture missing-file
// reconciliation takes toward a non-ErrNotExist Lstat
func TestScanReportsASymlinkItCannotResolve(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "sibling.epub"), "Sibling Book", "Author A", nil)

	// a link to itself: Stat fails ELOOP, which is neither "gone" nor
	// "here", and no filename suffix would tell us either
	loop := filepath.Join(libDir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("symlink loop: %v", err)
	}

	logs := captureLogs(t)
	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Scanned != 1 || result.New != 1 {
		t.Errorf("scan = %+v, want Scanned=1 New=1 (the sibling alone)", result)
	}
	if result.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the unresolvable link)", result.Errors)
	}
	if got := logs.String(); !strings.Contains(got, "could not resolve symlink") {
		t.Errorf("scan log = %q, want the unresolvable link reported", got)
	}
	if got := logs.String(); strings.Contains(got, "symlinked directory is not followed") {
		t.Errorf("a link that does not resolve was reported as a directory:\n%s", got)
	}
}

func TestTopLevelDirHasBooks(t *testing.T) {
	libDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(libDir, "populated", "nested"), 0o755); err != nil {
		t.Fatalf("mkdir populated/nested: %v", err)
	}
	writeTestEPUB(t, filepath.Join(libDir, "populated", "nested", "book.epub"), "Book", "Author", nil)
	if err := os.MkdirAll(filepath.Join(libDir, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	sidecars := filepath.Join(libDir, "sidecars")
	if err := os.MkdirAll(sidecars, 0o755); err != nil {
		t.Fatalf("mkdir sidecars: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sidecars, "cover.jpg"), []byte("not a book"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	writeTestEPUB(t, filepath.Join(libDir, "root.epub"), "Root Book", "Author", nil)

	cases := []struct {
		name    string
		relPath string
		want    bool
	}{
		// Deliberately a path that is not there itself: the question is
		// about the directory, and the caller only ever asks it about a
		// file that has just gone missing.
		{"populated, at depth", "populated/nested/gone.epub", true},
		{"empty", "empty/gone.epub", false},
		{"absent", "never-existed/gone.epub", false},
		{"only sidecars", "sidecars/gone.epub", false},
		{"root-level path has no top-level directory", "root.epub", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TopLevelDirHasBooks(libDir, tc.relPath)
			if err != nil {
				t.Fatalf("TopLevelDirHasBooks: %v", err)
			}
			if got != tc.want {
				t.Errorf("TopLevelDirHasBooks(%q) = %v, want %v", tc.relPath, got, tc.want)
			}
		})
	}

	t.Run("unreadable directory returns the error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: directory mode bits aren't enforced")
		}
		restricted := filepath.Join(libDir, "restricted")
		if err := os.MkdirAll(restricted, 0o755); err != nil {
			t.Fatalf("mkdir restricted: %v", err)
		}
		writeTestEPUB(t, filepath.Join(restricted, "book.epub"), "Hidden", "Author", nil)
		if err := os.Chmod(restricted, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { os.Chmod(restricted, 0o755) })

		// An unknown is not evidence: the caller has a third answer for
		// this and must not be handed "no books" for "could not look".
		got, err := TopLevelDirHasBooks(libDir, "restricted/book.epub")
		if err == nil {
			t.Fatalf("TopLevelDirHasBooks = %v, nil; want the read failure returned", got)
		}
		if got {
			t.Error("TopLevelDirHasBooks = true beside an error, want false")
		}
	})
}

// The per-directory counts Scan reports are what cmd/server logs, at a
// level it picks from whether the directory was unconfirmed last sweep. The
// count itself is the scanner's, so it is pinned here.
func TestReconcileMissingReportsUnconfirmedDirs(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	for _, dir := range []string{"fiction", "keep"} {
		if err := os.MkdirAll(filepath.Join(libDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestEPUB(t, filepath.Join(libDir, "fiction", "one.epub"), "One", "Author A", nil)
	writeTestEPUB(t, filepath.Join(libDir, "fiction", "two.epub"), "Two", "Author B", nil)
	// A sibling that stays put, so the sweep is not the empty-library case
	// and "fiction" is the only directory in question.
	writeTestEPUB(t, filepath.Join(libDir, "keep", "three.epub"), "Three", "Author C", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 3 {
		t.Fatalf("first scan = %+v, want New=3", first)
	}
	if len(mustScanResult(t, ctx, db, libDir, coversDir).UnconfirmedDirs) != 0 {
		t.Error("UnconfirmedDirs is non-empty for a library nothing has left")
	}

	if err := os.Rename(filepath.Join(libDir, "fiction"), filepath.Join(libDir, "novels")); err != nil {
		t.Fatalf("rename fiction: %v", err)
	}

	result := mustScanResult(t, ctx, db, libDir, coversDir)
	if result.Unconfirmed != 2 {
		t.Errorf("Unconfirmed = %d, want 2", result.Unconfirmed)
	}
	if got := result.UnconfirmedDirs["fiction"]; got != 2 {
		t.Errorf("UnconfirmedDirs = %v, want fiction:2", result.UnconfirmedDirs)
	}
	if len(result.UnconfirmedDirs) != 1 {
		t.Errorf("UnconfirmedDirs = %v, want only fiction", result.UnconfirmedDirs)
	}
}

func mustScanResult(t *testing.T, ctx context.Context, db *storage.DB, libDir, coversDir string) Result {
	t.Helper()
	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return result
}

// A file rewritten in place is a new content hash at a known path, which
// creates a book and orphans the old one. The edit has to survive that,
// end to end: it is the whole reason Calibre's write-back is not a data
// loss event.
func TestScanInPlaceRewriteKeepsManualEdits(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	path := filepath.Join(libDir, "book.epub")
	writeTestEPUB(t, path, "Embedded Title", "Embedded Author", nil)
	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	book := bookByPath(t, ctx, db, "book.epub")
	if _, err := db.UpdateBookField(ctx, book.ID, storage.FieldTitle, "The Title I Typed", time.Now()); err != nil {
		t.Fatalf("UpdateBookField: %v", err)
	}

	// Different bytes at the same path: a metadata write-back, a re-zip, a
	// re-download.
	writeTestEPUB(t, path, "Rewritten Embedded Title", "Embedded Author", testCoverImage(t))

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if result.New != 1 || result.Orphaned != 1 {
		t.Fatalf("second scan = %+v, want New=1 Orphaned=1", result)
	}

	replacement := bookByPath(t, ctx, db, "book.epub")
	if replacement.ID == book.ID {
		t.Fatal("the rewrite did not create a new book, so this test proves nothing")
	}
	if replacement.Title != "The Title I Typed" {
		t.Errorf("Title = %q, want the hand-edited one carried onto the replacement", replacement.Title)
	}
	sources, err := db.FieldSourcesForBook(ctx, replacement.ID)
	if err != nil {
		t.Fatalf("FieldSourcesForBook: %v", err)
	}
	if sources[storage.FieldTitle] != "manual" {
		t.Errorf("sources[title] = %q, want manual", sources[storage.FieldTitle])
	}
}

// A row the walk did not name whose Lstat nevertheless succeeds is a
// spelling the walk disagrees with, not an unknown: the walk is the
// authority on names. A row whose path names a directory reproduces that on
// every filesystem, where the case-alias shape it stands in for needs a
// case-insensitive one.
func TestReconcileMarksUnseenRowWhoseLstatSucceeds(t *testing.T) {
	cases := []struct {
		name           string
		topLevelKeepsA bool
		wantPruned     bool
	}{
		{"top-level directory still holds a book", true, true},
		{"top-level directory holds nothing", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			libDir := t.TempDir()
			coversDir := t.TempDir()
			db := openTestDB(t)
			ctx := context.Background()

			// A file elsewhere, so the sweep is never the empty-library case.
			writeTestEPUB(t, filepath.Join(libDir, "elsewhere.epub"), "Elsewhere", "Author", nil)
			if err := os.MkdirAll(filepath.Join(libDir, "top"), 0o755); err != nil {
				t.Fatalf("mkdir top: %v", err)
			}
			if tc.topLevelKeepsA {
				writeTestEPUB(t, filepath.Join(libDir, "top", "sibling.epub"), "Sibling", "Author", nil)
			}
			// The row's path is a real directory, which WalkDir visits and
			// never records in seen, and which Lstat resolves happily.
			if err := os.MkdirAll(filepath.Join(libDir, "top", "shelf"), 0o755); err != nil {
				t.Fatalf("mkdir top/shelf: %v", err)
			}

			if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
				t.Fatalf("seed Scan: %v", err)
			}
			if _, _, _, _, err := db.CreateBookWithFile(ctx,
				storage.Book{ContentHash: "hash-phantom", Title: "Phantom", SortTitle: "phantom", Format: "epub"},
				nil, "top/shelf", 100, time.Now()); err != nil {
				t.Fatalf("CreateBookWithFile: %v", err)
			}

			marked, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
			if err != nil {
				t.Fatalf("marking Scan: %v", err)
			}
			if marked.Missing != 1 {
				t.Fatalf("marking scan = %+v, want Missing=1 (a successful Lstat on an unseen row is a spelling mismatch)", marked)
			}
			f, err := db.FindFileByPath(ctx, "top/shelf")
			if err != nil || f == nil || !f.MissingSince.Valid {
				t.Fatalf("FindFileByPath = %+v, %v; want it marked", f, err)
			}

			if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = ? WHERE file_path = ?`,
					"2020-01-01T00:00:00.000000000Z", "top/shelf")
				return err
			}); err != nil {
				t.Fatalf("backdate missing_since: %v", err)
			}

			pruning, err := Scan(ctx, db, libDir, coversDir, time.Hour)
			if err != nil {
				t.Fatalf("pruning Scan: %v", err)
			}
			gone, err := db.FindFileByPath(ctx, "top/shelf")
			if err != nil {
				t.Fatalf("FindFileByPath: %v", err)
			}
			if tc.wantPruned {
				if pruning.Pruned != 1 || gone != nil {
					t.Errorf("pruning scan = %+v, row = %+v; want the overdue row pruned", pruning, gone)
				}
				return
			}
			if pruning.Pruned != 0 || gone == nil {
				t.Errorf("pruning scan = %+v, row = %+v; want the row kept: its directory yielded no files", pruning, gone)
			}
			if pruning.Unconfirmed != 1 {
				t.Errorf("pruning scan = %+v, want Unconfirmed=1", pruning)
			}
		})
	}
}

// The other half of the rule above: a *failed* Lstat is still an unknown,
// and an unknown is not evidence. Only success became a spelling mismatch.
func TestReconcileLstatErrorStillLeavesRowAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory mode bits aren't enforced")
	}
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	// top/ keeps a file throughout, so the row reaches Lstat rather than
	// the unconfirmed-directory guard, and elsewhere.epub keeps the sweep
	// out of the empty-library case even once top/ is unreadable.
	locked := filepath.Join(libDir, "top", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir top/locked: %v", err)
	}
	writeTestEPUB(t, filepath.Join(libDir, "elsewhere.epub"), "Elsewhere", "Author", nil)
	writeTestEPUB(t, filepath.Join(libDir, "top", "sibling.epub"), "Sibling", "Author", nil)
	writeTestEPUB(t, filepath.Join(locked, "book.epub"), "Locked Book", "Author", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 3 {
		t.Fatalf("first scan = %+v, want New=3", first)
	}

	// EACCES on the leaf's parent: the walk reports the directory itself as
	// unreadable, and Lstat on the file under it fails with EACCES rather
	// than ErrNotExist.
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if result.Missing != 0 {
		t.Errorf("second scan = %+v, want Missing=0 (EACCES is not proof of absence)", result)
	}
	f, err := db.FindFileByPath(ctx, "top/locked/book.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath = %+v, %v; want the row untouched", f, err)
	}
	if f.MissingSince.Valid {
		t.Errorf("missing_since = %v, want NULL", f.MissingSince)
	}
}

// Renaming a folder by case only is a rename the filesystem then hides: the
// walk records the new spelling, and Lstat on the old one succeeds because
// the filesystem matches it to the new name. Without Decision 5's rule the
// old row stays live for good, with no annotation and nothing in the log.
func TestScanCaseOnlyRenameMarksOldSpelling(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	if err := os.MkdirAll(filepath.Join(libDir, "Books"), 0o755); err != nil {
		t.Fatalf("mkdir Books: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(libDir, "books")); err != nil {
		t.Skip("case-sensitive filesystem: the old spelling fails Lstat with ErrNotExist, which TestScanMarksMissingFileWithoutDeleting already covers")
	}

	writeTestEPUB(t, filepath.Join(libDir, "Books", "book.epub"), "Book", "Author", nil)
	// Outside the renamed folder, so the sweep is never the empty-library
	// case and the rename is the only thing under test.
	writeTestEPUB(t, filepath.Join(libDir, "elsewhere.epub"), "Elsewhere", "Author B", nil)

	if first, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("first Scan: %v", err)
	} else if first.New != 2 {
		t.Fatalf("first scan = %+v, want New=2", first)
	}

	if err := os.Rename(filepath.Join(libDir, "Books"), filepath.Join(libDir, "books")); err != nil {
		t.Fatalf("rename Books to books: %v", err)
	}

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if result.Moved != 1 {
		t.Errorf("second scan = %+v, want Moved=1 (the new spelling is a new path for known content)", result)
	}
	if result.Missing != 1 {
		t.Errorf("second scan = %+v, want Missing=1 (the old spelling must be marked, not left live)", result)
	}

	old, err := db.FindFileByPath(ctx, "Books/book.epub")
	if err != nil || old == nil {
		t.Fatalf("FindFileByPath(Books/book.epub) = %+v, %v", old, err)
	}
	if !old.MissingSince.Valid {
		t.Error("the old spelling's row is still live, so the book shows two paths with no annotation")
	}
	fresh, err := db.FindFileByPath(ctx, "books/book.epub")
	if err != nil || fresh == nil || fresh.MissingSince.Valid {
		t.Errorf("FindFileByPath(books/book.epub) = %+v, %v; want a live row", fresh, err)
	}
}
