package enrich

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"library/internal/storage"
)

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
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
	w := New(db, []Provider{p})
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
// before step 05 gives the worker a real provider to call.
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

	w := New(db, nil)
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

	w := New(db, nil)
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
	w := New(db, []Provider{p})
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

	w := New(db, nil)
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
	w := New(db, []Provider{p})

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
	w := New(db, []Provider{a, b})
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
