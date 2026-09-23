package importer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"library/internal/scanner"
	"library/internal/storage"
	"library/internal/storage/storagetest"
)

func TestStageReadsTheFileAndOffersIt(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)

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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)

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
	// Expiry is one of the four paths that drop a stage, and each has to
	// give the budget back or the next upload is charged for a file that
	// is gone.
	stager.mu.Lock()
	held := stager.staged
	stager.mu.Unlock()
	if held != 0 {
		t.Errorf("an expired stage still holds %d bytes of the budget", held)
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
	db := storagetest.Open(t)

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
	db := storagetest.Open(t)
	stager, _ := testStager(t, db, 1<<20)

	if _, err := stager.Confirm(context.Background(), "nosuchstage"); !errors.Is(err, ErrExpired) {
		t.Errorf("Confirm on an unknown id = %v, want ErrExpired", err)
	}
}

func TestDiscardDropsTheStagedFile(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
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
	stager.mu.Lock()
	held := stager.staged
	stager.mu.Unlock()
	if held != 0 {
		t.Errorf("a discarded stage still holds %d bytes of the budget", held)
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
	db := storagetest.Open(t)
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

// Nothing about a stage is in the database, so a restart has nothing to
// recover and the wipe is the whole story.
func TestNewEmptiesTheStagingDirectory(t *testing.T) {
	db := storagetest.Open(t)
	root := t.TempDir()
	tempDir := filepath.Join(root, "staging")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(tempDir, "leftover.epub"), []byte("from a previous run"))

	stager, err := New(db, Options{LibraryDir: root, CoversDir: root, TempDir: tempDir, MaxSize: 1 << 20})
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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
	db := storagetest.Open(t)
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

// The preview is rendered into a page, and what the parsers hand back is
// bounded only by their own document caps — megabytes of description for an
// EPUB, and more for an FB2. It is also what the title-match verdict
// compares, so an uncapped title could never equal a sort_title the scanner
// derived from a capped one.
// A name is charged to the budget after the reservation, so one past the
// whole budget would lock every later import out until the stage expired
func TestStageBoundsTheOfferedName(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
	stager, _ := testStager(t, db, 1<<20)

	longName := strings.Repeat("é", 5<<20/2) + ".epub"
	staged, err := stager.Stage(ctx, longName, bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(staged.OriginalName) > maxOfferedNameBytes {
		t.Errorf("OriginalName is %d bytes, want at most %d", len(staged.OriginalName), maxOfferedNameBytes)
	}
	if !utf8.ValidString(staged.OriginalName) {
		t.Error("the cut name is not valid UTF-8")
	}

	if _, err := stager.Stage(ctx, "Emma.epub", bytes.NewReader(epubBytes(t, "Emma", "Jane Austen", 0))); err != nil {
		t.Fatalf("a second Stage after a long name: %v", err)
	}
}

func TestStageCapsWhatThePreviewShows(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
	stager, _ := testStager(t, db, 8<<20)

	longTitle := strings.Repeat("Dune ", storage.MaxTitleBytes)
	longDescription := strings.Repeat("sand. ", storage.MaxDescriptionBytes)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(
		epubBytesWithDescription(t, longTitle, "Frank Herbert", longDescription)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if len(staged.Title) > storage.MaxTitleBytes {
		t.Errorf("the preview title is %d bytes, want at most %d", len(staged.Title), storage.MaxTitleBytes)
	}
	if len(staged.Description) > storage.MaxDescriptionBytes {
		t.Errorf("the preview description is %d bytes, want at most %d", len(staged.Description), storage.MaxDescriptionBytes)
	}
	if !utf8.ValidString(staged.Title) || !utf8.ValidString(staged.Description) {
		t.Error("a capped value is not valid UTF-8")
	}
}

// The verdict compares sort_title, which the scanner derives from a capped
// title — so the importer has to cap before it compares, or a book whose
// title runs past the limit can never be recognised as one the library
// already has.
func TestATitleOverTheCapStillMatches(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
	stager, _ := testStager(t, db, 8<<20)

	longTitle := strings.Repeat("Dune ", storage.MaxTitleBytes)

	first, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, longTitle, "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := stager.Confirm(ctx, first.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	second, err := stager.Stage(ctx, "Dune-2.epub", bytes.NewReader(epubBytes(t, longTitle, "F. Herbert", 64)))
	if err != nil {
		t.Fatalf("Stage a second edition: %v", err)
	}
	if second.Verdict != VerdictTitleMatch {
		t.Errorf("Verdict = %q, want %q: the long title did not match the capped one stored", second.Verdict, VerdictTitleMatch)
	}
}

// Each stage holds up to the import cap in os.TempDir(), which is tmpfs in
// a container — so what forgotten tabs hold for half an hour is RAM.
func TestStagingIsBounded(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)

	// A cap small enough that the budget is a handful of fixtures.
	const size = 4 << 10
	stager, _ := testStager(t, db, size)

	var ids []string
	for i := 0; ; i++ {
		staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune "+strconv.Itoa(i), "Frank Herbert", size/2)))
		if errors.Is(err, ErrStagingFull) {
			break
		}
		if err != nil {
			t.Fatalf("Stage %d: %v", i, err)
		}
		ids = append(ids, staged.ID)
		if i > stagingBudgetFactor*4 {
			t.Fatalf("staged %d files without ever reaching the budget", i)
		}
	}
	if len(ids) == 0 {
		t.Fatal("the budget refused the very first upload")
	}

	// Discarding one makes room again, which is what the refusal tells the
	// person to do.
	stager.Discard(ids[0])
	if _, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "After a discard", "Frank Herbert", size/2))); err != nil {
		t.Errorf("Stage after a discard = %v, want room to have been freed", err)
	}
}

// A confirmed stage is holding no bytes, so it must not hold budget either
// — the record outlives the confirm only to answer a double click.
func TestConfirmingGivesTheBudgetBack(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)

	const size = 4 << 10
	stager, _ := testStager(t, db, size)

	for i := range stagingBudgetFactor * 3 {
		staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune "+strconv.Itoa(i), "Frank Herbert", size/2)))
		if err != nil {
			t.Fatalf("Stage %d: %v", i, err)
		}
		if _, err := stager.Confirm(ctx, staged.ID); err != nil {
			t.Fatalf("Confirm %d: %v", i, err)
		}
	}

	stager.mu.Lock()
	held := stager.staged
	stager.mu.Unlock()
	if held != 0 {
		t.Errorf("confirmed stages still hold %d bytes of the budget", held)
	}
}

// A duplicate's file is deleted the moment the verdict is decided, so the
// file's share of the budget goes back at once — but the record survives to
// render the preview, cover and all, and what it still holds stays charged.
// Charging only the file is what lets a small upload hold megabytes.
func TestADuplicateReleasesItsFileButKeepsHoldingItsCover(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
	stager, _ := testStager(t, db, 1<<20)

	art := solidPNG(t)
	book := epubBytesWithCover(t, "Dune", "Frank Herbert", 4096, art)

	first, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(book))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := stager.Confirm(ctx, first.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	duplicate, err := stager.Stage(ctx, "Dune-copy.epub", bytes.NewReader(book))
	if err != nil {
		t.Fatalf("Stage a duplicate: %v", err)
	}
	if duplicate.Verdict != VerdictExists {
		t.Fatalf("Verdict = %q, want %q", duplicate.Verdict, VerdictExists)
	}

	stager.mu.Lock()
	held := stager.staged
	stager.mu.Unlock()

	if held >= int64(len(book)) {
		t.Errorf("a duplicate holds %d bytes, want the file's %d back", held, len(book))
	}
	if held < int64(len(art)) {
		t.Errorf("a duplicate holds %d bytes, want at least the %d its cover keeps in memory", held, len(art))
	}
}

// A confirmed import drops the cover with the file — the redirect goes to
// the book's own page — so nothing of it stays charged.
func TestConfirmingDropsTheCoverAndReleasesEverything(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
	stager, _ := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(
		epubBytesWithCover(t, "Dune", "Frank Herbert", 0, solidPNG(t))))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !staged.HasCover {
		t.Fatal("the fixture carries no cover, so this proves nothing")
	}
	if _, err := stager.Confirm(ctx, staged.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	stager.mu.Lock()
	held := stager.staged
	stager.mu.Unlock()
	if held != 0 {
		t.Errorf("a confirmed import still holds %d bytes of the budget", held)
	}
	if _, _, ok := stager.Cover(staged.ID); ok {
		t.Error("a confirmed import still holds its cover in memory")
	}
}

// Expiry is checked under the lock and the file is opened after it is
// dropped, so a sweep crossing the time to live in that window takes the
// file away. That is the expiry the caller has a sentence for, not a
// filesystem error nobody can name.
func TestAFileSweptMidConfirmReadsAsExpired(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
	stager, _ := testStager(t, db, 1<<20)

	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Exactly what the janitor does to the file, with the record left in
	// place so Confirm gets past its own expiry check.
	stager.mu.Lock()
	if err := os.Remove(stager.stages[staged.ID].path); err != nil {
		t.Fatalf("remove the staged file: %v", err)
	}
	stager.mu.Unlock()

	if _, err := stager.Confirm(ctx, staged.ID); !errors.Is(err, ErrExpired) {
		t.Errorf("Confirm after a sweep took the file = %v, want ErrExpired", err)
	}
}

// The library owns the bytes before the index write starts, so a browser
// that closed its tab must not cancel it: a cancelled write would mark the
// stage done with no book and make every later confirm report a failure
// that did not happen.
func TestConfirmIndexesEvenIfTheRequestIsCancelled(t *testing.T) {
	db := storagetest.Open(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	ctx, cancel := context.WithCancel(context.Background())
	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(epubBytes(t, "Dune", "Frank Herbert", 0)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	cancel()

	bookID, err := stager.Confirm(ctx, staged.ID)
	if err != nil {
		t.Fatalf("Confirm on a cancelled request: %v", err)
	}
	if bookID == 0 {
		t.Fatal("Confirm named no book")
	}
	if names := libraryNames(t, libraryDir); !slices.Equal(names, []string{"Dune.epub"}) {
		t.Errorf("the library holds %v, want the imported file", names)
	}
	book, err := db.FindBookByID(context.Background(), bookID)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID = %+v, %v; want the indexed book", book, err)
	}
}

// The preview's verdict is a snapshot, and a sweep can index the same bytes
// between it and the confirm. The copy just written is then a second
// location of a book that already existed — the correct outcome, which the
// detail page shows as "2 paths" — and the confirm answers that book rather
// than a new one.
func TestConfirmJoinsABookIndexedSinceThePreview(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)
	stager, libraryDir := testStager(t, db, 1<<20)

	book := epubBytes(t, "Dune", "Frank Herbert", 0)
	staged, err := stager.Stage(ctx, "Dune.epub", bytes.NewReader(book))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if staged.Verdict != VerdictNew {
		t.Fatalf("Verdict = %q, want %q at preview time", staged.Verdict, VerdictNew)
	}

	// A sweep gets there first, with the same bytes under another name.
	swept := writeFile(t, filepath.Join(libraryDir, "Dune-from-a-sweep.epub"), book)
	sweptID, created, err := scanner.IndexFile(ctx, db, libraryDir, swept, t.TempDir())
	if err != nil || !created {
		t.Fatalf("IndexFile = %d, %v, %v; want a new book", sweptID, created, err)
	}

	bookID, err := stager.Confirm(ctx, staged.ID)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if bookID != sweptID {
		t.Errorf("Confirm answered book %d, want the %d the sweep indexed", bookID, sweptID)
	}

	files, err := db.ListBookFiles(ctx, bookID)
	if err != nil {
		t.Fatalf("ListBookFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("the book has %d locations, want the sweep's and the import's", len(files))
	}
	if count, err := db.CountBooks(ctx); err != nil || count != 1 {
		t.Errorf("CountBooks = %d, %v; want the one book both paths belong to", count, err)
	}
}
