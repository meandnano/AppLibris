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

// The janitor is what reclaims a stage nobody came back to. Confirm is
// deliberately not called first: it removes an expired record itself, so a
// test that confirmed before sweeping would pass with sweep's body deleted.
func TestTheJanitorReclaimsAnExpiredStage(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	now := time.Now()
	stager, libraryDir := testStagerAt(t, db, 1<<20, func() time.Time { return now })

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if names := stagingNames(t, stager); len(names) != 1 {
		t.Fatalf("staging holds %v, want the one staged file", names)
	}

	// One second short of the time to live, the janitor must leave it be:
	// a sweep that reclaimed everything would pass the assertions below
	// without expiry meaning anything.
	now = now.Add(StageTTL - time.Second)
	stager.sweep()
	if names := stagingNames(t, stager); len(names) != 1 {
		t.Fatalf("the janitor reclaimed a live stage, leaving %v", names)
	}
	if _, ok := stager.Get(staged.ID); !ok {
		t.Fatal("Get lost a stage that has not expired")
	}

	now = now.Add(time.Second)
	stager.sweep()

	if names := stagingNames(t, stager); len(names) != 0 {
		t.Errorf("the janitor left %v in staging", names)
	}
	if _, ok := stager.Get(staged.ID); ok {
		t.Error("the janitor left the record behind")
	}
	if names := libraryNames(t, libraryDir); len(names) != 0 {
		t.Errorf("an expired stage reached the library as %v", names)
	}
}

// Confirm rechecks expiry itself, so a click on a tab left open since lunch
// says so rather than acting on a file the janitor is about to delete —
// without depending on whether the janitor has run yet.
func TestConfirmRechecksExpiryWithoutTheJanitor(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	now := time.Now()
	stager, libraryDir := testStagerAt(t, db, 1<<20, func() time.Time { return now })

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
	if names := libraryNames(t, libraryDir); len(names) != 0 {
		t.Errorf("an expired confirm reached the library as %v", names)
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

// A double click, a slow no-JS submit, two open tabs: whatever produces
// them, several confirms of one stage must import the book once and all
// answer the same id. The per-stage mutex is what makes the second wait for
// the first rather than race it into a second copy of the same bytes, so
// this is the test that fails if it goes.
func TestConcurrentConfirmsOfOneStageImportOnce(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	const confirms = 6
	ids := make([]int64, confirms)
	errs := make([]error, confirms)

	// Released together, so the calls actually overlap rather than
	// queueing behind each other's goroutine start-up.
	var start, wg sync.WaitGroup
	start.Add(1)
	for i := range confirms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			ids[i], errs[i] = stager.Confirm(ctx, staged.ID)
		}()
	}
	start.Done()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Confirm %d: %v", i, err)
		}
		if ids[i] == 0 {
			t.Fatalf("Confirm %d named no book", i)
		}
		if ids[i] != ids[0] {
			t.Errorf("Confirm %d answered book %d, want %d — every caller must get the first call's answer", i, ids[i], ids[0])
		}
	}

	if names := libraryNames(t, libraryDir); !slices.Equal(names, []string{"Dune.epub"}) {
		t.Errorf("the library holds %v, want the one file %d confirms asked for", names, confirms)
	}
	if count, err := db.CountBooks(ctx); err != nil || count != 1 {
		t.Errorf("CountBooks = %d, %v; want 1", count, err)
	}
}

func TestStageKeepsACoverItCanIdentify(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	art := solidPNG(t)
	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytesWithCover(t, "Dune", "Frank Herbert", 0, art)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !staged.HasCover {
		t.Fatal("HasCover is false for a book with a real cover")
	}

	data, contentType, ok := stager.Cover(staged.ID)
	if !ok {
		t.Fatal("Cover answered nothing for a book with a cover")
	}
	if !bytes.Equal(data, art) {
		t.Errorf("Cover returned %d bytes, want the %d embedded", len(data), len(art))
	}
	if contentType != "image/png" {
		t.Errorf("Cover reported %q, want %q", contentType, "image/png")
	}
}

// Neither format reader checks that what a manifest calls a cover is an
// image, so an upload can put anything at all there. It must not become
// something the preview route will hand a browser: an HTML document served
// from this app's own origin is the thing sameSiteOnly admits.
func TestStageDropsACoverThatIsNotAnImage(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	stager, _ := testStager(t, db, 1<<20)

	html := []byte(`<!doctype html><script>fetch("/recipients/remove",{method:"POST"})</script>`)
	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytesWithCover(t, "Dune", "Frank Herbert", 0, html)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if staged.HasCover {
		t.Error("HasCover is true for a cover that is an HTML document")
	}
	if data, contentType, ok := stager.Cover(staged.ID); ok {
		t.Errorf("Cover served %d bytes as %q, want nothing", len(data), contentType)
	}
}
