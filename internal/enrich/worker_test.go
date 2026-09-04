package enrich

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"library/internal/storage"
)

// solidPNG mirrors internal/cover's own test helper of the same name — a
// small, genuinely decodable image, since cover.Store must succeed for
// these tests to exercise the write path rather than its own failure
// tolerance.
func solidPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 20, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 20; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 50, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
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

// newTestWorker wires a fresh, per-test covers directory — tests that don't
// exercise the cover path never need to know it exists.
func newTestWorker(t *testing.T, db *storage.DB, providers []Provider) *Worker {
	t.Helper()
	return New(db, providers, t.TempDir())
}

func TestWorkerAppliesResolvedFieldsAndMarksDone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-1", Title: "Book", SortTitle: "book", ISBN: "9780000000001"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books"}, nil
	}}
	w := newTestWorker(t, db, []Provider{p})
	w.drain(ctx)

	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}
	book, err := db.FindBookByID(ctx, id)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID: %+v, %v", book, err)
	}
	if book.Publisher != "Ace Books" {
		t.Errorf("Publisher = %q, want Ace Books", book.Publisher)
	}
	var source string
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, storage.FieldPublisher).Scan(&source); err != nil || source != "fake" {
		t.Errorf("publisher source = %q, %v, want fake", source, err)
	}

	var status string
	if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(storage.EnrichmentDone) {
		t.Errorf("status = %q, want done", status)
	}
}

// With no providers configured, every job still resolves cleanly to done
// having called nothing and changed nothing — the wiring stays exercised
// which is what METADATA_PROVIDERS= configures.
func TestWorkerWithNoProvidersResolvesDoneAndTouchesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-2", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	w := newTestWorker(t, db, nil)
	w.drain(ctx)

	book, err := db.FindBookByID(ctx, id)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID: %+v, %v", book, err)
	}
	if book.Publisher != "" || book.Description != "" {
		t.Errorf("book was modified with no providers configured: %#v", book)
	}

	var status string
	if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(storage.EnrichmentDone) {
		t.Errorf("status = %q, want done", status)
	}
}

// A job whose book vanished between enqueue and claim fails outright — the
// job itself going wrong, not a provider having nothing to say.
func TestWorkerBookGoneFails(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "worker-3", Title: "Book", Format: "epub"}, nil, "a.epub", 10, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	// Claim the job before the book is deleted, so the claimed row exists
	// (and stays running) for process to see FindBookByID come back nil —
	// the narrow race the plan calls out, since enrichment_jobs.book_id
	// otherwise cascades the row away with the book.
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}
	job, err := db.ClaimNextEnrichment(ctx, time.Now())
	if err != nil || job == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", job, err)
	}

	f, err := db.FindFileByPath(ctx, "a.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, old); err != nil {
		t.Fatal(err)
	}
	if _, books, err := db.PruneMissingFiles(ctx, []int64{f.ID}); err != nil || books != 1 {
		t.Fatalf("PruneMissingFiles: books=%d, err=%v", books, err)
	}

	// The cascade should have removed the job along with the book — assert
	// that, then confirm process() handles the (extremely unlikely in
	// practice) case where it doesn't outlive the race gracefully too, by
	// calling it directly against the job value already in hand.
	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM enrichment_jobs WHERE id = ?`, job.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("enrichment_jobs row survived the book's deletion: %d rows", count)
	}

	w := newTestWorker(t, db, nil)
	w.process(ctx, job)

	// The row is gone, so the terminal write is a no-op — nothing to
	// assert on the (nonexistent) row beyond it not panicking or hanging.
}

func TestWorkerProviderErrorStillMarksJobDone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-4", Title: "Book", SortTitle: "book", ISBN: "9780000000001"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("network unreachable")
	}}
	w := newTestWorker(t, db, []Provider{p})
	w.drain(ctx)

	var status, reason string
	if err := db.Read().QueryRow(`SELECT status, failure_reason FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != string(storage.EnrichmentDone) {
		t.Errorf("status = %q, want done — a provider having nothing to say is not a job failure", status)
	}
	if reason != "" {
		t.Errorf("failure_reason = %q, want empty", reason)
	}
}

// Notify must wake a worker idling in Run's select, without waiting for
// the once-a-minute pollInterval tick.
func TestWorkerNotifyWakesIdleWorker(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newTestWorker(t, db, nil)
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	id, err := db.CreateBook(context.Background(), storage.Book{ContentHash: "worker-5", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(context.Background(), id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}
	w.Notify()

	deadline := time.After(3 * time.Second)
	for {
		var status string
		if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&status); err != nil {
			t.Fatalf("query status: %v", err)
		}
		if status == string(storage.EnrichmentDone) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job did not reach done within 3s of Notify (status = %q); pollInterval is 1m, so this means Notify isn't waking the worker", status)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}
}

// A job in flight when ctx is cancelled is left running, not failed — the
// opposite of internal/sender, because Resolve is deterministic and safe
// to re-run: see storage.RequeueInterruptedEnrichment's doc comment.
func TestWorkerCancellationLeavesJobRunningForRecovery(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	id, err := db.CreateBook(context.Background(), storage.Book{ContentHash: "worker-6", Title: "Book", SortTitle: "book", ISBN: "9780000000001"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(context.Background(), id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	entered := make(chan struct{})
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		close(entered)
		<-ctx.Done()
		return Metadata{}, ctx.Err()
	}}
	w := newTestWorker(t, db, []Provider{p})

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider was never called")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}

	var status string
	if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(storage.EnrichmentRunning) {
		t.Fatalf("status = %q, want running (the row a crash mid-job leaves behind)", status)
	}

	n, err := db.RequeueInterruptedEnrichment(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("RequeueInterruptedEnrichment: %v", err)
	}
	if n != 1 {
		t.Fatalf("RequeueInterruptedEnrichment resolved %d rows, want 1", n)
	}

	recovered, err := db.ClaimNextEnrichment(context.Background(), time.Now())
	if err != nil || recovered == nil {
		t.Fatalf("ClaimNextEnrichment after recovery: %+v, %v", recovered, err)
	}
}

// A job that pulls fields from two different providers must apply each
// field under its own provider's name — grouping in the worker exists so
// ApplyEnrichedFields's single "source" argument per call doesn't
// misattribute one provider's answer to another's.
func TestWorkerAppliesEachProvidersFieldsUnderItsOwnSource(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-7", Title: "Book", SortTitle: "book", ISBN: "9780000000001"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	a := &fakeProvider{name: "provider-a", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books"}, nil
	}}
	b := &fakeProvider{name: "provider-b", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Description: "A description"}, nil
	}}
	w := newTestWorker(t, db, []Provider{a, b})
	w.drain(ctx)

	var publisherSource, descriptionSource string
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, storage.FieldPublisher).Scan(&publisherSource); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, storage.FieldDescription).Scan(&descriptionSource); err != nil {
		t.Fatal(err)
	}
	if publisherSource != "provider-a" {
		t.Errorf("publisher source = %q, want provider-a", publisherSource)
	}
	if descriptionSource != "provider-b" {
		t.Errorf("description source = %q, want provider-b", descriptionSource)
	}
}

// jobStatus reads a book's job status, for tests asserting the terminal
// state rather than what was written to the book.
func jobStatus(t *testing.T, db *storage.DB, bookID int64) string {
	t.Helper()
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE book_id = ?`, bookID).Scan(&status); err != nil {
		t.Fatalf("query job status: %v", err)
	}
	return status
}

// coverServer serves img at /cover.jpg and counts how many times it was
// asked for one, which is what lets a test assert that no request is made
// at all for a book that already has a cover.
func coverServer(t *testing.T, img []byte) (url string, requests *int) {
	t.Helper()
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Write(img)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/cover.jpg", &count
}

// A fetched cover must end up resized and stored under the book's own
// content hash — never the provider's URL, which is all Resolve ever sees
// (see Metadata.CoverURL's doc comment) — with provenance recorded under
// the provider that answered it, the same as any other field.
func TestWorkerStoresFetchedCoverUnderContentHashWithProvenance(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coversDir := t.TempDir()

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-cover-1", Title: "Book", SortTitle: "book", ISBN: "9780000000001"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	coverURL, requests := coverServer(t, solidPNG(t))
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: coverURL}, nil
	}}
	w := New(db, []Provider{p}, coversDir)
	w.drain(ctx)

	if *requests != 1 {
		t.Errorf("cover requests = %d, want 1", *requests)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID: %+v, %v", book, err)
	}
	wantPath := filepath.Join(coversDir, "worker-cover-1.jpg")
	if book.CoverPath != wantPath {
		t.Errorf("CoverPath = %q, want %q (stored under the content hash, not a URL)", book.CoverPath, wantPath)
	}
	if _, err := os.Stat(book.CoverPath); err != nil {
		t.Errorf("stored cover file: %v", err)
	}

	var source string
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, storage.FieldCover).Scan(&source); err != nil || source != "fake" {
		t.Errorf("cover source = %q, %v, want fake", source, err)
	}
}

// A book that already has a cover must never have it overwritten by a
// provider's answer — mirrors ApplyEnrichedFields's own re-check, but
// exercised end to end through the worker so a regression in Resolve's
// cover handling (§Resolve) shows up here too.
func TestWorkerNeverOverwritesAnExistingCover(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coversDir := t.TempDir()
	existingPath := filepath.Join(coversDir, "already-there.jpg")

	id, err := db.CreateBook(ctx, storage.Book{
		ContentHash: "worker-cover-2", Title: "Book", SortTitle: "book",
		ISBN: "9780000000001", CoverPath: existingPath,
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	coverURL, requests := coverServer(t, solidPNG(t))
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: coverURL}, nil
	}}
	w := New(db, []Provider{p}, coversDir)
	w.drain(ctx)

	// The stronger half of "never asked for one": no image is downloaded
	// either, so a book that already has a cover costs no bandwidth.
	if *requests != 0 {
		t.Errorf("cover requests = %d, want 0 — the book already has a cover", *requests)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID: %+v, %v", book, err)
	}
	if book.CoverPath != existingPath {
		t.Errorf("CoverPath = %q, want unchanged %q", book.CoverPath, existingPath)
	}
	// cover.Store names its output by content hash, not by whatever
	// cover_path already held — checking existingPath here would pass
	// even if the worker wrongly fetched and stored a cover, since that
	// write would land at a different filename. This is the path such a
	// write would actually use.
	derivedPath := filepath.Join(coversDir, "worker-cover-2.jpg")
	if _, err := os.Stat(derivedPath); err == nil {
		t.Errorf("a cover was stored at %s for a book that should never have been asked for one", derivedPath)
	}
}

// A cover is the one field whose loss costs nothing but a dashed box in
// the grid, so a fetch or store failure must not fail a job whose text
// fields already resolved — and must not record a path or provenance for a
// cover that was never stored.
func TestWorkerCoverFailureStillFinishesTheJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"undecodable image", []byte("not-an-image")},
		{"empty body", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			coversDir := t.TempDir()

			id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-cover-fail", Title: "Book", SortTitle: "book", ISBN: "9780000000001"}, nil)
			if err != nil {
				t.Fatalf("CreateBook: %v", err)
			}
			if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
				t.Fatalf("EnqueueEnrichment: %v", err)
			}

			coverURL, _ := coverServer(t, tc.body)
			p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
				return Metadata{CoverURL: coverURL, Publisher: "Ace Books"}, nil
			}}
			New(db, []Provider{p}, coversDir).drain(ctx)

			if got := jobStatus(t, db, id); got != "done" {
				t.Errorf("job status = %q, want done — a lost cover must not fail a job whose text fields resolved", got)
			}
			book, err := db.FindBookByID(ctx, id)
			if err != nil || book == nil {
				t.Fatalf("FindBookByID: %+v, %v", book, err)
			}
			if book.Publisher != "Ace Books" {
				t.Errorf("Publisher = %q, want it applied regardless of the cover", book.Publisher)
			}
			if book.CoverPath != "" {
				t.Errorf("CoverPath = %q, want empty — nothing was stored", book.CoverPath)
			}
			var source string
			err = db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, storage.FieldCover).Scan(&source)
			if err == nil {
				t.Errorf("cover provenance = %q, want no row at all", source)
			}
		})
	}
}

// A provider answering a cover URL with a scheme other than http or https
// must be refused before any request is made — the URL is a third party's
// string, not something this process chose.
func TestWorkerRefusesANonHTTPCoverURL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coversDir := t.TempDir()

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-cover-scheme", Title: "Book", SortTitle: "book", ISBN: "9780000000001"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: "file:///etc/passwd"}, nil
	}}
	New(db, []Provider{p}, coversDir).drain(ctx)

	if got := jobStatus(t, db, id); got != "done" {
		t.Errorf("job status = %q, want done", got)
	}
	book, err := db.FindBookByID(ctx, id)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID: %+v, %v", book, err)
	}
	if book.CoverPath != "" {
		t.Errorf("CoverPath = %q, want empty", book.CoverPath)
	}
}

// Enrichment fills cover_path for a book whose earlier embedded-cover store
// failed (cover_retry set). The marker has to clear with it: the scanner
// skips its stat check entirely while cover_retry is true, so it would
// re-extract the embedded cover over the provider's one on the next sweep
// while field_sources went on naming the provider.
func TestWorkerClearsCoverRetryWhenItStoresACover(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coversDir := t.TempDir()

	id, err := db.CreateBook(ctx, storage.Book{
		ContentHash: "worker-cover-retry", Title: "Book", SortTitle: "book",
		ISBN: "9780000000001", CoverRetry: true,
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatalf("EnqueueEnrichment: %v", err)
	}

	coverURL, _ := coverServer(t, solidPNG(t))
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: coverURL}, nil
	}}
	New(db, []Provider{p}, coversDir).drain(ctx)

	book, err := db.FindBookByID(ctx, id)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID: %+v, %v", book, err)
	}
	if book.CoverPath == "" {
		t.Fatal("CoverPath is empty, want the stored cover")
	}
	if book.CoverRetry {
		t.Error("CoverRetry is still set; the scanner will overwrite this cover on its next sweep")
	}
}

// The job row has to carry what the run wrote, so the UI can name the
// fields instead of just saying "done".
func TestWorkerRecordsTheFieldsItWrote(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "worker-fields", Title: "Book", SortTitle: "book", ISBN: "9780000000009"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books", Language: "en"}, nil
	}}
	newTestWorker(t, db, []Provider{p}).drain(ctx)

	job, err := db.LatestEnrichmentForBook(ctx, id)
	if err != nil || job == nil {
		t.Fatalf("LatestEnrichmentForBook: %+v, %v", job, err)
	}
	if job.Status != storage.EnrichmentDone {
		t.Fatalf("status = %q, want done", job.Status)
	}
	if job.UpdatedFields != "publisher,language" {
		t.Errorf("UpdatedFields = %q, want %q", job.UpdatedFields, "publisher,language")
	}
}

// A book with nothing missing is the common case, and it must record an
// empty field list on a done job — "nothing to add", not a failure and not
// a phantom field.
func TestWorkerRecordsNoFieldsWhenNothingWasMissing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, storage.Book{
		ContentHash: "worker-complete", Title: "Book", SortTitle: "book",
		Publisher: "Ace Books", PublishedDate: "1984", Language: "en",
		ISBN: "9780000000010", Description: "Complete", CoverPath: "/covers/x.jpg",
	}, []string{"An Author"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		t.Error("a provider was asked about a book with nothing missing")
		return Metadata{}, nil
	}}
	newTestWorker(t, db, []Provider{p}).drain(ctx)

	job, err := db.LatestEnrichmentForBook(ctx, id)
	if err != nil || job == nil {
		t.Fatalf("LatestEnrichmentForBook: %+v, %v", job, err)
	}
	if job.Status != storage.EnrichmentDone {
		t.Errorf("status = %q, want done", job.Status)
	}
	if job.UpdatedFields != "" {
		t.Errorf("UpdatedFields = %q, want empty", job.UpdatedFields)
	}
}
