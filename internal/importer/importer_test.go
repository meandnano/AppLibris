package importer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStageReadsTheFileAndOffersIt(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if staged.Verdict != VerdictNew {
		t.Errorf("Verdict = %q, want %q", staged.Verdict, VerdictNew)
	}
	if staged.Title != "Dune" {
		t.Errorf("Title = %q, want %q", staged.Title, "Dune")
	}
	if !slices.Equal(staged.Authors, []string{"Frank Herbert"}) {
		t.Errorf("Authors = %v, want [Frank Herbert]", staged.Authors)
	}
	if staged.Format != "epub" {
		t.Errorf("Format = %q, want %q", staged.Format, "epub")
	}
	if staged.LibraryName != "Dune.epub" {
		t.Errorf("LibraryName = %q, want %q", staged.LibraryName, "Dune.epub")
	}
	if got, ok := stager.Get(staged.ID); !ok || got.ID != staged.ID {
		t.Errorf("Get(%q) = %+v, %v; want the staged import", staged.ID, got, ok)
	}
}

// An FB2 offered under an .epub name is written .fb2: the suffix comes out
// of the content, and the preview says which name the library will use.
func TestStageNamesTheFileByWhatItIsAndNotByWhatItWasCalled(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "book.epub", bytes.NewReader(fb2Bytes("Dune", "Frank")))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if staged.Format != "fb2" {
		t.Errorf("Format = %q, want %q", staged.Format, "fb2")
	}
	if staged.LibraryName != "book.fb2" {
		t.Errorf("LibraryName = %q, want %q", staged.LibraryName, "book.fb2")
	}
}

func TestStageRefusesAFileOverTheCapAndKeepsNothing(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	// Sized from a real EPUB so the cap is exercised against the bytes an
	// import actually carries rather than against filler.
	book := epubBytes(t, "Dune", "Frank Herbert", 4096)
	stager, _ := testStager(t, db, int64(len(book))-1)

	if _, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(book)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Stage over the cap = %v, want ErrTooLarge", err)
	}
	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("a refused upload left %v in staging", names)
	}

	atCap, _ := testStager(t, db, int64(len(book)))
	if _, err := atCap.Stage(ctx, "Dune.epub", bytes.NewReader(book)); err != nil {
		t.Fatalf("Stage at exactly the cap: %v", err)
	}
}

func TestStageRefusesSomethingThatIsNotABook(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	if _, err := stager.Stage(ctx, "notes.txt", strings.NewReader("just some text")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Stage of a text file = %v, want ErrUnsupportedFormat", err)
	}
	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("a refused upload left %v in staging", names)
	}
}

// Byte-identical content the library already holds has nothing to confirm,
// so the staged file goes at once and only the record survives.
func TestStageReportsContentTheLibraryAlreadyHolds(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	book := epubBytes(t, "Dune", "Frank Herbert", 0)
	first, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(book))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := stager.Confirm(ctx, first.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	again, err := stager.Stage(ctx, "Dune-copy.epub", bytes.NewReader(book))
	if err != nil {
		t.Fatalf("Stage the same bytes again: %v", err)
	}
	if again.Verdict != VerdictExists {
		t.Fatalf("Verdict = %q, want %q", again.Verdict, VerdictExists)
	}
	if again.ExistingTitle != "Dune" {
		t.Errorf("ExistingTitle = %q, want %q", again.ExistingTitle, "Dune")
	}
	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("a duplicate left %v in staging, want nothing", names)
	}
}

func TestStageWarnsAboutATitleTheLibraryAlreadyHolds(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	first, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := stager.Confirm(ctx, first.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	// Different bytes, same title — a second edition, which is a
	// legitimate thing to own and so is offered under a warning.
	other, err := stager.Stage(ctx, "Dune-2.epub", bytes.NewReader(epubBytes(t, "Dune", "F. Herbert", 512)))
	if err != nil {
		t.Fatalf("Stage a second edition: %v", err)
	}
	if other.Verdict != VerdictTitleMatch {
		t.Errorf("Verdict = %q, want %q", other.Verdict, VerdictTitleMatch)
	}
	if other.ExistingTitle != "Dune" {
		t.Errorf("ExistingTitle = %q, want %q", other.ExistingTitle, "Dune")
	}
}

func TestConfirmWritesTheLibraryAndIndexesIt(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	bookID, err := stager.Confirm(ctx, staged.ID)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	if names := libraryNames(t, libraryDir); !slices.Equal(names, []string{"Dune.epub"}) {
		t.Errorf("the library holds %v, want [Dune.epub] and no .part", names)
	}
	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("a confirmed import left %v in staging", names)
	}

	file, err := db.FindFileByPath(ctx, "Dune.epub")
	if err != nil {
		t.Fatalf("FindFileByPath: %v", err)
	}
	if file == nil || file.BookID != bookID {
		t.Fatalf("FindFileByPath = %+v, want a row for book %d", file, bookID)
	}

	book, err := db.FindBookByID(ctx, bookID)
	if err != nil {
		t.Fatalf("FindBookByID: %v", err)
	}
	if book == nil || book.Title != "Dune" {
		t.Errorf("the indexed book is %+v, want one titled Dune", book)
	}

	// A repeat is a double click, not a second import.
	again, err := stager.Confirm(ctx, staged.ID)
	if err != nil {
		t.Fatalf("Confirm twice: %v", err)
	}
	if again != bookID {
		t.Errorf("the second Confirm answered book %d, want %d", again, bookID)
	}
	if names := libraryNames(t, libraryDir); len(names) != 1 {
		t.Errorf("a second Confirm wrote %v, want the one file", names)
	}
}

func TestConfirmSidestepsANameTheLibraryAlreadyUses(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	writeFile(t, filepath.Join(libraryDir, "Dune.epub"), epubBytes(t, "Dune", "Someone", 128))

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := stager.Confirm(ctx, staged.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	names := libraryNames(t, libraryDir)
	slices.Sort(names)
	if !slices.Equal(names, []string{"Dune (2).epub", "Dune.epub"}) {
		t.Errorf("the library holds %v, want both names", names)
	}
}

// Two confirms of different books offered under one name get two files.
func TestConcurrentConfirmsOfOneNameGetTwoFiles(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	first, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	second, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune Messiah", "Frank Herbert", 64)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{first.ID, second.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = stager.Confirm(ctx, id)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Confirm %d: %v", i, err)
		}
	}
	names := libraryNames(t, libraryDir)
	slices.Sort(names)
	if !slices.Equal(names, []string{"Dune (2).epub", "Dune.epub"}) {
		t.Errorf("the library holds %v, want two distinct names", names)
	}
}

// The stage is gone half an hour after it was made, its file with it, and a
// click on the tab that made it says so rather than acting on a file the
// janitor deleted a moment ago.
func TestAStageExpires(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	now := time.Now()
	clock := func() time.Time { return now }
	stager, libraryDir := testStagerAt(t, db, 1<<20, clock)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	now = now.Add(StageTTL)

	if _, ok := stager.Get(staged.ID); ok {
		t.Error("Get answered an expired stage")
	}
	if _, err := stager.Confirm(ctx, staged.ID); !errors.Is(err, ErrExpired) {
		t.Errorf("Confirm on an expired stage = %v, want ErrExpired", err)
	}

	stager.sweep()
	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("the janitor left %v in staging", names)
	}
	if names := libraryNames(t, libraryDir); len(names) != 0 {
		t.Errorf("an expired stage reached the library as %v", names)
	}
}

func TestConfirmOnAnUnknownIDIsExpired(t *testing.T) {
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	if _, err := stager.Confirm(context.Background(), "nosuchstage"); !errors.Is(err, ErrExpired) {
		t.Errorf("Confirm on an unknown id = %v, want ErrExpired", err)
	}
}

func TestDiscardDropsTheStagedFile(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	stager.Discard(staged.ID)
	stager.Discard(staged.ID) // idempotent: what was asked for has happened

	if _, ok := stager.Get(staged.ID); ok {
		t.Error("Get answered a discarded stage")
	}
	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("a discard left %v in staging", names)
	}
	if names := libraryNames(t, libraryDir); len(names) != 0 {
		t.Errorf("a discarded stage reached the library as %v", names)
	}
}

// An index write that fails leaves the bytes where they are: the next sweep
// is the recovery the scanner already promises, and deleting the file would
// throw a book away over a database error.
func TestConfirmLeavesTheLibraryFileWhenIndexingFails(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	db.Close()

	if _, err := stager.Confirm(ctx, staged.ID); !errors.Is(err, ErrNotIndexed) {
		t.Fatalf("Confirm with the database closed = %v, want ErrNotIndexed", err)
	}
	if names := libraryNames(t, libraryDir); !slices.Equal(names, []string{"Dune.epub"}) {
		t.Errorf("the library holds %v, want the imported file to have stayed", names)
	}
	// And a second attempt does not copy it in again.
	if _, err := stager.Confirm(ctx, staged.ID); !errors.Is(err, ErrNotIndexed) {
		t.Fatalf("Confirm again = %v, want ErrNotIndexed", err)
	}
	if names := libraryNames(t, libraryDir); len(names) != 1 {
		t.Errorf("the library holds %v, want the one file", names)
	}
}

func TestAReadOnlyLibraryRefusesEverything(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	root := t.TempDir()
	stager, err := New(db, Options{
		LibraryDir: root,
		CoversDir:  root,
		TempDir:    filepath.Join(root, "staging"),
		MaxSize:    1 << 20,
		Writable:   false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if stager.Enabled() {
		t.Error("Enabled reported true for an unwritable library")
	}
	if _, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(nil)); !errors.Is(err, ErrLibraryNotWritable) {
		t.Errorf("Stage = %v, want ErrLibraryNotWritable", err)
	}
	if _, err := stager.Confirm(ctx, "whatever"); !errors.Is(err, ErrLibraryNotWritable) {
		t.Errorf("Confirm = %v, want ErrLibraryNotWritable", err)
	}
}

// Nothing about a stage is in the database, so a restart has nothing to
// recover and the wipe is the whole story.
func TestNewEmptiesTheStagingDirectory(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	tempDir := filepath.Join(root, "staging")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(tempDir, "leftover.epub"), []byte("from a previous run"))

	stager, err := New(db, Options{LibraryDir: root, CoversDir: root, TempDir: tempDir, MaxSize: 1 << 20, Writable: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("New left %v in staging", names)
	}
}
