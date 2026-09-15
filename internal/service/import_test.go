package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"library/internal/importer"
	"library/internal/storage"
)

const importContainerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

const importOPFTemplate = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
  </metadata>
  <manifest></manifest>
</package>`

func importTestEPUB(t *testing.T, title, author string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": importContainerXML,
		"OEBPS/content.opf":      fmt.Sprintf(importOPFTemplate, title, author),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

// newImportTestService builds a Service over a real Stager, in the shape
// newMetadataTestService uses: what these tests are about is the mapping
// this layer makes from the importer's answers to the transport's, which a
// stand-in importer could not produce.
func newImportTestService(t *testing.T) (*Service, *storage.DB, string) {
	t.Helper()

	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	root := t.TempDir()
	libraryDir := filepath.Join(root, "library")
	coversDir := filepath.Join(root, "covers")
	for _, dir := range []string{libraryDir, coversDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	stager, err := importer.New(db, importer.Options{
		LibraryDir: libraryDir,
		CoversDir:  coversDir,
		TempDir:    filepath.Join(root, "staging"),
		MaxSize:    1 << 20,
	})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	return New(db, WithImporter(stager)), db, libraryDir
}

func TestStageImportShapesThePreview(t *testing.T) {
	svc, _, _ := newImportTestService(t)

	preview, err := svc.StageImport(context.Background(), "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}

	if preview.Title != "Dune" {
		t.Errorf("Title = %q, want %q", preview.Title, "Dune")
	}
	if !slices.Equal(preview.Authors, []string{"Frank Herbert"}) {
		t.Errorf("Authors = %v, want [Frank Herbert]", preview.Authors)
	}
	if preview.Format != "epub" {
		t.Errorf("Format = %q, want %q", preview.Format, "epub")
	}
	if preview.LibraryName != "Dune.epub" {
		t.Errorf("LibraryName = %q, want %q", preview.LibraryName, "Dune.epub")
	}
	if preview.Verdict != importer.VerdictNew {
		t.Errorf("Verdict = %q, want %q", preview.Verdict, importer.VerdictNew)
	}
	if preview.ID == "" {
		t.Error("the preview carries no id, so nothing can confirm it")
	}

	// And it reads back by id, which is what the no-JS path's redirect
	// lands on.
	again, err := svc.StagedImport(context.Background(), preview.ID)
	if err != nil {
		t.Fatalf("StagedImport: %v", err)
	}
	if again == nil || again.Title != "Dune" {
		t.Errorf("StagedImport = %+v, want the staged preview", again)
	}
}

func TestConfirmImportReportsTheBookItIndexed(t *testing.T) {
	ctx := context.Background()
	svc, db, libraryDir := newImportTestService(t)

	preview, err := svc.StageImport(ctx, "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}

	bookID, indexed, err := svc.ConfirmImport(ctx, preview.ID)
	if err != nil {
		t.Fatalf("ConfirmImport: %v", err)
	}
	if !indexed {
		t.Fatal("indexed is false for an import that succeeded")
	}
	if bookID == 0 {
		t.Fatal("ConfirmImport named no book")
	}
	if _, err := os.Stat(filepath.Join(libraryDir, "Dune.epub")); err != nil {
		t.Errorf("the library file is not there: %v", err)
	}
	book, err := db.FindBookByID(ctx, bookID)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID = %+v, %v", book, err)
	}
}

// The one outcome that is neither success nor failure, and the reason this
// method has three results: the bytes are in the library and the index
// write failed, so there is no book to redirect to but nothing to undo
// either. The handler needs those apart, and a nil error is what says "the
// file is safe" — the importer has already logged the failure at Warn.
func TestConfirmImportReportsAnUnindexedFileAsNeitherFailureNorSuccess(t *testing.T) {
	ctx := context.Background()
	svc, db, libraryDir := newImportTestService(t)

	preview, err := svc.StageImport(ctx, "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}
	db.Close()

	bookID, indexed, err := svc.ConfirmImport(ctx, preview.ID)
	if err != nil {
		t.Fatalf("ConfirmImport = %v, want a nil error: the file reached the library", err)
	}
	if indexed {
		t.Error("indexed is true although the index write failed")
	}
	if bookID != 0 {
		t.Errorf("ConfirmImport named book %d, want none: nothing was indexed", bookID)
	}
	if _, err := os.Stat(filepath.Join(libraryDir, "Dune.epub")); err != nil {
		t.Errorf("the library file was discarded over an index error: %v", err)
	}
}

func TestDiscardImportDropsTheStage(t *testing.T) {
	ctx := context.Background()
	svc, _, libraryDir := newImportTestService(t)

	preview, err := svc.StageImport(ctx, "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}
	if err := svc.DiscardImport(ctx, preview.ID); err != nil {
		t.Fatalf("DiscardImport: %v", err)
	}

	staged, err := svc.StagedImport(ctx, preview.ID)
	if err != nil {
		t.Fatalf("StagedImport: %v", err)
	}
	if staged != nil {
		t.Errorf("StagedImport = %+v, want nil after a discard", staged)
	}
	entries, err := os.ReadDir(libraryDir)
	if err != nil {
		t.Fatalf("read library: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a discarded import reached the library: %v", entries)
	}
}

// Absent is not an error at this layer, the same contract GetBook keeps, so
// the handler turns it into a 404 the same way.
func TestStagedImportOfAnUnknownIDIsNotAnError(t *testing.T) {
	svc, _, _ := newImportTestService(t)

	staged, err := svc.StagedImport(context.Background(), "nosuchstage")
	if err != nil {
		t.Fatalf("StagedImport on an unknown id = %v, want nil", err)
	}
	if staged != nil {
		t.Errorf("StagedImport = %+v, want nil", staged)
	}
}

// A Service built without an importer — every test in this package but
// these, and cmd/server when the library cannot be written — answers every
// import method rather than panicking on a nil field.
func TestEveryImportMethodRefusesWithoutAnImporter(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc := New(db)

	if svc.ImportEnabled() {
		t.Error("ImportEnabled is true with no importer")
	}
	if got := svc.MaxImportBytes(); got != 0 {
		t.Errorf("MaxImportBytes = %d, want 0", got)
	}
	if data, contentType := svc.StagedCover("anything"); data != nil || contentType != "" {
		t.Errorf("StagedCover = %d bytes, %q; want nothing", len(data), contentType)
	}

	if _, err := svc.StageImport(ctx, "Dune.epub", strings.NewReader("")); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("StageImport = %v, want ErrImportDisabled", err)
	}
	if _, err := svc.StagedImport(ctx, "anything"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("StagedImport = %v, want ErrImportDisabled", err)
	}
	if _, _, err := svc.ConfirmImport(ctx, "anything"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("ConfirmImport = %v, want ErrImportDisabled", err)
	}
	if err := svc.DiscardImport(ctx, "anything"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("DiscardImport = %v, want ErrImportDisabled", err)
	}
}

// A Stager exists exactly when importing is available: cmd/server builds
// one only for a writable library, so there is no disabled Stager to ask.
func TestImportEnabledFollowsWhetherAStagerWasGiven(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	root := t.TempDir()
	stager, err := importer.New(db, importer.Options{
		LibraryDir: root,
		CoversDir:  root,
		TempDir:    filepath.Join(root, "staging"),
		MaxSize:    64 << 20,
	})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}

	svc := New(db, WithImporter(stager))
	if !svc.ImportEnabled() {
		t.Error("ImportEnabled is false though a Stager was given")
	}
	if got := svc.MaxImportBytes(); got != 64<<20 {
		t.Errorf("MaxImportBytes = %d, want the configured cap", got)
	}
}
