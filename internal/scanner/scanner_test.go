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
	"strings"
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
	err = db.Read().QueryRowContext(ctx, `SELECT title, format, cover_path FROM books WHERE id = ?`, f.BookID).
		Scan(&b.Title, &b.Format, &b.CoverPath)
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

	oldRel := "book1.epub"
	oldPath := filepath.Join(libDir, oldRel)
	writeTestEPUB(t, oldPath, "Book One", "Author A", nil)

	first, err := Scan(ctx, db, libDir, coversDir)
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

	second, err := Scan(ctx, db, libDir, coversDir)
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

	result, err := Scan(ctx, db, libDir, coversDir)
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
	rescan, err := Scan(ctx, db, libDir, coversDir)
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

	first, err := Scan(ctx, db, libDir, coversDir)
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

	second, err := Scan(ctx, db, libDir, coversDir)
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

	first, err := Scan(ctx, db, libDir, coversDir)
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

	second, err := Scan(ctx, db, libDir, coversDir)
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

	result, err := Scan(ctx, db, libDir, coversDir)
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

	result, err := Scan(ctx, db, missingDir, coversDir)
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

	result, err := Scan(ctx, db, libDir, coversDir)
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

	if _, err := Scan(ctx, db, libDir, coversDir); err != nil {
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

	first, err := Scan(ctx, db, rootA, coversDir)
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

	second, err := Scan(ctx, db, rootB, coversDir)
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

	if _, err := Scan(ctx, db, libraryDir, coversDir); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	books, err := db.ListBooks(ctx)
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
