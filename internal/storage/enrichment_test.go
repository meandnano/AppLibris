package storage

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestEnqueueEnrichmentIsIdempotentWhileQueued(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-1", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	queued, err := db.EnqueueEnrichment(ctx, id, now)
	if err != nil || !queued {
		t.Fatalf("first EnqueueEnrichment = %v, %v; want queued", queued, err)
	}

	queued, err = db.EnqueueEnrichment(ctx, id, now.Add(time.Minute))
	if err != nil || queued {
		t.Fatalf("second EnqueueEnrichment while still queued = %v, %v; want not queued", queued, err)
	}

	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("enrichment_jobs rows for book = %d, want 1", count)
	}

	// Once the job is terminal, a new enqueue is a fresh promise, not a
	// duplicate of a stale one.
	job, err := db.ClaimNextEnrichment(ctx, now.Add(time.Minute))
	if err != nil || job == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", job, err)
	}
	if err := db.MarkEnrichmentDone(ctx, job.ID, nil, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkEnrichmentDone: %v", err)
	}

	queued, err = db.EnqueueEnrichment(ctx, id, now.Add(3*time.Minute))
	if err != nil || !queued {
		t.Fatalf("EnqueueEnrichment after the previous job went terminal = %v, %v; want queued", queued, err)
	}
	if err := db.Read().QueryRow(`SELECT count(*) FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("enrichment_jobs rows for book = %d, want 2 (one terminal, one fresh)", count)
	}
}

func TestClaimNextEnrichmentClaimsOldestFirstAndEmptiesToNil(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-2", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.CreateBook(ctx, Book{ContentHash: "enrich-3", Title: "Book 2", SortTitle: "book 2"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	later := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	earlier := later.Add(-time.Hour)

	// Enqueued out of claim order — id2 first but with the later
	// timestamp — so the assertion below actually exercises the ORDER BY
	// queued_at rather than coincidentally matching insertion order.
	if _, err := db.EnqueueEnrichment(ctx, id2, later); err != nil {
		t.Fatalf("EnqueueEnrichment id2: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, earlier); err != nil {
		t.Fatalf("EnqueueEnrichment id: %v", err)
	}

	claimed, err := db.ClaimNextEnrichment(ctx, later)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", claimed, err)
	}
	if claimed.BookID != id {
		t.Errorf("claimed book_id = %d, want %d (the earlier-queued job)", claimed.BookID, id)
	}
	if claimed.Status != EnrichmentRunning {
		t.Errorf("Status = %q, want running", claimed.Status)
	}
	if !claimed.StartedAt.Valid || !claimed.StartedAt.Time.Equal(later) {
		t.Errorf("StartedAt = %v, want %v", claimed.StartedAt, later)
	}

	second, err := db.ClaimNextEnrichment(ctx, later)
	if err != nil || second == nil {
		t.Fatalf("second ClaimNextEnrichment: %+v, %v", second, err)
	}
	if second.BookID != id2 {
		t.Errorf("second claimed book_id = %d, want %d", second.BookID, id2)
	}

	empty, err := db.ClaimNextEnrichment(ctx, later)
	if err != nil {
		t.Fatalf("ClaimNextEnrichment on empty queue: %v", err)
	}
	if empty != nil {
		t.Errorf("ClaimNextEnrichment on empty queue = %+v, want nil", empty)
	}
}

func TestMarkEnrichmentIsNoOpUnlessRunning(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-4", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := db.EnqueueEnrichment(ctx, id, now); err != nil {
		t.Fatal(err)
	}

	var jobID int64
	if err := db.Read().QueryRow(`SELECT id FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	// Still queued, not running: both terminal writes must no-op.
	if err := db.MarkEnrichmentDone(ctx, jobID, nil, now); err != nil {
		t.Fatalf("MarkEnrichmentDone: %v", err)
	}
	if err := db.MarkEnrichmentFailed(ctx, jobID, "reason", now); err != nil {
		t.Fatalf("MarkEnrichmentFailed: %v", err)
	}
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(EnrichmentQueued) {
		t.Fatalf("status after no-op terminal calls = %q, want queued", status)
	}

	if _, err := db.ClaimNextEnrichment(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkEnrichmentDone(ctx, jobID, nil, now); err != nil {
		t.Fatalf("MarkEnrichmentDone after claim: %v", err)
	}

	// Already terminal: a second call must not overwrite it.
	if err := db.MarkEnrichmentFailed(ctx, jobID, "late failure", now); err != nil {
		t.Fatalf("MarkEnrichmentFailed on a terminal row: %v", err)
	}
	if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(EnrichmentDone) {
		t.Fatalf("status after a late Failed call on a Done row = %q, want done (unchanged)", status)
	}
}

// RequeueInterruptedEnrichment is the inverse of FailInterruptedSends:
// where a send left "sending" is failed and never requeued, an enrichment
// job left "running" is requeued and never failed — see the doc comment on
// RequeueInterruptedEnrichment for why that inversion is correct rather
// than an inconsistency with internal/sender.
func TestRequeueInterruptedEnrichmentRequeuesRunningRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-5", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := db.EnqueueEnrichment(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimNextEnrichment(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", job, err)
	}

	n, err := db.RequeueInterruptedEnrichment(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RequeueInterruptedEnrichment: %v", err)
	}
	if n != 1 {
		t.Fatalf("RequeueInterruptedEnrichment resolved %d rows, want 1", n)
	}

	var status string
	var startedAt *string
	if err := db.Read().QueryRow(`SELECT status, started_at FROM enrichment_jobs WHERE id = ?`, job.ID).Scan(&status, &startedAt); err != nil {
		t.Fatal(err)
	}
	if status != string(EnrichmentQueued) {
		t.Fatalf("status after requeue = %q, want queued (not failed)", status)
	}
	if startedAt != nil {
		t.Errorf("started_at after requeue = %v, want NULL", *startedAt)
	}

	// And it's genuinely claimable again, not just relabelled.
	reclaimed, err := db.ClaimNextEnrichment(ctx, now.Add(2*time.Minute))
	if err != nil || reclaimed == nil {
		t.Fatalf("ClaimNextEnrichment after requeue: %+v, %v", reclaimed, err)
	}
	if reclaimed.ID != job.ID {
		t.Errorf("reclaimed id = %d, want %d", reclaimed.ID, job.ID)
	}
}

func TestRequeueInterruptedEnrichmentLeavesTerminalRowsAlone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-6", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := db.EnqueueEnrichment(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimNextEnrichment(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", job, err)
	}
	if err := db.MarkEnrichmentDone(ctx, job.ID, nil, now); err != nil {
		t.Fatal(err)
	}

	n, err := db.RequeueInterruptedEnrichment(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RequeueInterruptedEnrichment: %v", err)
	}
	if n != 0 {
		t.Fatalf("RequeueInterruptedEnrichment resolved %d rows, want 0 (nothing was running)", n)
	}
}

// EnqueueEnrichment allows a fresh queued job for a book that already has
// one running (Decision 3's per-status guard, not a bug). If the process
// then crashes, RequeueInterruptedEnrichment must not blindly requeue that
// running row too — a book with two queued rows breaks EnqueueEnrichment's
// own dedup invariant and would double the provider calls the next drain
// makes for it. The interrupted row should be deleted, not requeued and
// not marked done: it never ran to completion, so "done" would misreport
// a crash as a successful run — leaving exactly the one queued sibling
// behind.
func TestRequeueInterruptedEnrichmentRetiresARunningJobWithAQueuedSibling(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-6b", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	if _, err := db.EnqueueEnrichment(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	running, err := db.ClaimNextEnrichment(ctx, now)
	if err != nil || running == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", running, err)
	}

	// A second job for the same book, queued while the first is still
	// running — exactly what EnqueueEnrichment's guard permits.
	queued, err := db.EnqueueEnrichment(ctx, id, now.Add(time.Minute))
	if err != nil || !queued {
		t.Fatalf("EnqueueEnrichment while a sibling is running = %v, %v; want queued", queued, err)
	}
	var queuedJobID int64
	if err := db.Read().QueryRow(`SELECT id FROM enrichment_jobs WHERE book_id = ? AND status = ?`, id, string(EnrichmentQueued)).Scan(&queuedJobID); err != nil {
		t.Fatal(err)
	}

	n, err := db.RequeueInterruptedEnrichment(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("RequeueInterruptedEnrichment: %v", err)
	}
	if n != 0 {
		t.Fatalf("RequeueInterruptedEnrichment reported %d requeued, want 0 — the interrupted row has a queued sibling and should be retired, not requeued", n)
	}

	var survivingCount int
	if err := db.Read().QueryRow(`SELECT count(*) FROM enrichment_jobs WHERE id = ?`, running.ID).Scan(&survivingCount); err != nil {
		t.Fatal(err)
	}
	if survivingCount != 0 {
		t.Errorf("interrupted job rows = %d, want 0 — it should be deleted, not marked done for a run that never completed", survivingCount)
	}
	var queuedStatus string
	if err := db.Read().QueryRow(`SELECT status FROM enrichment_jobs WHERE id = ?`, queuedJobID).Scan(&queuedStatus); err != nil {
		t.Fatal(err)
	}
	if queuedStatus != string(EnrichmentQueued) {
		t.Errorf("sibling queued job status = %q, want unchanged queued", queuedStatus)
	}

	var queuedCount int
	if err := db.Read().QueryRow(`SELECT count(*) FROM enrichment_jobs WHERE book_id = ? AND status = ?`, id, string(EnrichmentQueued)).Scan(&queuedCount); err != nil {
		t.Fatal(err)
	}
	if queuedCount != 1 {
		t.Fatalf("queued jobs for book after recovery = %d, want 1 — the dedup invariant must hold after recovery", queuedCount)
	}
}

// enrichment_jobs.book_id cascades on delete: an enrichment job is a
// pending intention about a book, and once the book is gone the intention
// is meaningless — unlike send_log, which must survive its book to keep
// the record that a send happened.
func TestDeletingBookCascadesItsEnrichmentJobs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	id, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "enrich-7", Title: "Book", Format: "epub"}, nil, "a.epub", 10, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
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
		t.Fatalf("PruneMissingFiles: books=%d, err=%v, want 1 book pruned", books, err)
	}

	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM enrichment_jobs WHERE book_id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("enrichment_jobs rows for the deleted book = %d, want 0", count)
	}
}

func TestApplyEnrichedFieldsWritesValueProvenanceAndFTSInOneTransaction(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-8", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	_, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldPublisher:   "Gollancz",
		FieldISBN:        "9780575000001",
		FieldDescription: "A story.",
		FieldAuthors:     "First Author\nSecond Author",
	}, map[MetadataField]string{
		FieldPublisher:   "openlibrary",
		FieldISBN:        "openlibrary",
		FieldDescription: "openlibrary",
		FieldAuthors:     "openlibrary",
	}, when)
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields = %v, %v", exists, err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Publisher != "Gollancz" || book.ISBN != "9780575000001" || book.Description != "A story." {
		t.Errorf("book after apply = %#v", book)
	}
	if !book.ModifiedAt.Equal(when) {
		t.Errorf("ModifiedAt = %v, want %v", book.ModifiedAt, when)
	}

	authors, err := db.ListAuthorsForBook(ctx, id)
	if err != nil || len(authors) != 2 || authors[0] != "First Author" || authors[1] != "Second Author" {
		t.Errorf("authors = %v, %v", authors, err)
	}

	for _, field := range []MetadataField{FieldPublisher, FieldISBN, FieldDescription, FieldAuthors} {
		var source string
		if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, field).Scan(&source); err != nil || source != "openlibrary" {
			t.Errorf("source for %s = %q, %v, want openlibrary", field, source, err)
		}
	}

	books, err := db.SearchBooks(ctx, `"story"*`, BookPage{})
	if err != nil || len(books) != 1 {
		t.Errorf("search on enriched description = %v, %v", books, err)
	}
}

// An invalid field halts the whole call, whichever order the values map
// happens to iterate in — map order is unspecified, so this only proves
// nothing gets applied when one field errors, not that a write which
// already landed earlier in the transaction gets rolled back.
// TestApplyEnrichedFieldsTransactionRollsBackAnAlreadyAppliedFieldOnLaterFailure,
// below, proves that half deterministically.
func TestApplyEnrichedFieldsRollsBackOnFailureAcrossMixedProviders(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-10", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldPublisher:         "Gollancz",
		MetadataField("bogus"): "x",
	}, map[MetadataField]string{
		FieldPublisher:         "provider-a",
		MetadataField("bogus"): "provider-b",
	}, time.Now())
	if err == nil {
		t.Fatal("ApplyEnrichedFields with an invalid field = nil error, want one")
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Publisher != "" {
		t.Errorf("Publisher = %q after a rolled-back write, want empty", book.Publisher)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldPublisher).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("field_sources rows for publisher = %d after a rolled-back write, want 0 — provider-a's field must not survive provider-b's failure", count)
	}
}

// Proves the underlying mechanism ApplyEnrichedFields's loop depends on —
// a write already applied earlier in a db.Write transaction is undone
// when a later statement in that same transaction fails — deterministically,
// by driving the exact package-internal calls the loop makes (write the
// column, then record its own provenance) directly, in a known order,
// rather than through ApplyEnrichedFields's public map-based signature,
// whose iteration order is unspecified. This alone doesn't call
// ApplyEnrichedFields, though, so it can't by itself catch a regression to
// separate transactions *inside* ApplyEnrichedFields —
// TestApplyEnrichedFieldsRollsBackAllFieldsWhenFTSyncFails, below, is what
// drives the real public function and would catch that.
func TestApplyEnrichedFieldsTransactionRollsBackAnAlreadyAppliedFieldOnLaterFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-11b", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now()

	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := updateBookColumnTx(ctx, tx, id, FieldPublisher, "Gollancz", when); err != nil {
			return err
		}
		if err := setFieldSourceTx(ctx, tx, id, FieldPublisher, "provider-a"); err != nil {
			return err
		}
		// provider-b's field fails outright, after provider-a's has
		// already been written (but not committed) above — the same
		// failure shape as an unrecognised MetadataField reaching
		// updateBookColumnTx from ApplyEnrichedFields's loop.
		return updateBookColumnTx(ctx, tx, id, MetadataField("bogus"), "x", when)
	})
	if err == nil {
		t.Fatal("transaction with a later failing field = nil error, want one")
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Publisher != "" {
		t.Errorf("Publisher = %q after a rolled-back transaction, want empty — the earlier write must not survive the later failure", book.Publisher)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldPublisher).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("field_sources rows for publisher = %d after a rolled-back transaction, want 0", count)
	}
}

// The test that actually regression-tests ApplyEnrichedFields's own
// atomicity, calling the real public function rather than replicating its
// internals: syncBookFTSTx runs exactly once, unconditionally, after every
// field in the loop has been processed — a position fixed by
// ApplyEnrichedFields's own body, not by map iteration order — so
// dropping books_fts out from under it forces a failure there
// deterministically, regardless of which field the (unordered) values map
// happened to visit first. If ApplyEnrichedFields ever regressed to a
// separate transaction per field (the shape the original review comment
// on the worker's old per-provider grouping reported), a field applied in
// an earlier transaction would already be committed and would survive
// this failure; asserting neither field does is what proves the whole
// call still runs as one transaction.
func TestApplyEnrichedFieldsRollsBackAllFieldsWhenFTSyncFails(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-11f", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.write.ExecContext(ctx, `DROP TABLE books_fts`); err != nil {
		t.Fatalf("drop books_fts: %v", err)
	}

	_, _, err = db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldPublisher:   "Gollancz",
		FieldDescription: "A story.",
	}, map[MetadataField]string{
		FieldPublisher:   "provider-a",
		FieldDescription: "provider-b",
	}, time.Now())
	if err == nil {
		t.Fatal("ApplyEnrichedFields with books_fts gone = nil error, want one")
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Publisher != "" || book.Description != "" {
		t.Errorf("book after a rolled-back sync failure = %#v, want both fields empty", book)
	}
	for _, field := range []MetadataField{FieldPublisher, FieldDescription} {
		var count int
		if err := db.Read().QueryRow(`SELECT count(*) FROM field_sources WHERE book_id = ? AND field = ?`, id, field).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("field_sources rows for %s = %d, want 0 — a later sync failure must roll back every field the loop already applied, from both providers", field, count)
		}
	}
}

// The regression a review comment called out: Resolve takes its snapshot
// of what's missing before any provider is asked, and a provider can take
// a while to answer. If a person fills or deliberately clears the same
// field in between, ApplyEnrichedFields must not clobber it with the
// provider's now-stale answer — the field_sources/manual guarantee has to
// win. fieldIsStillMissingTx's fresh re-read inside the write transaction
// is what makes this safe without any explicit locking: the manual edit
// below stands in for one that committed while a provider call was still
// in flight.
//
// This case alone only exercises isMissing's emptiness half: the field's
// value is non-empty, so it would be skipped even if the provenance check
// were deleted outright. TestApplyEnrichedFieldsSkipsAClearedManualField
// and TestApplyEnrichedFieldsSkipsManuallySetAuthors, below, cover the
// half this one can't — a deliberately *cleared* field, and the separate
// row-count branch fieldIsStillMissingTx takes for authors.
func TestApplyEnrichedFieldsSkipsAFieldManuallyEditedSinceResolve(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-11", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Stands in for a person editing the book while Resolve's chosen
	// provider was still being asked about it.
	if exists, err := db.UpdateBookField(ctx, id, FieldDescription, "Hand-written description", time.Now()); err != nil || !exists {
		t.Fatalf("UpdateBookField: %v, %v", exists, err)
	}

	// The stale answer Resolve computed before that edit landed.
	_, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldDescription: "Provider description",
		FieldPublisher:   "Ace Books",
	}, map[MetadataField]string{
		FieldDescription: "openlibrary",
		FieldPublisher:   "openlibrary",
	}, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields = %v, %v", exists, err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Description != "Hand-written description" {
		t.Errorf("Description = %q, want the manual edit preserved, not the provider's stale answer", book.Description)
	}
	if book.Publisher != "Ace Books" {
		t.Errorf("Publisher = %q, want Ace Books — this field genuinely was still missing", book.Publisher)
	}

	var descSource, pubSource string
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldDescription).Scan(&descSource); err != nil {
		t.Fatal(err)
	}
	if descSource != "manual" {
		t.Errorf("description source = %q, want manual (unchanged)", descSource)
	}
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldPublisher).Scan(&pubSource); err != nil {
		t.Fatal(err)
	}
	if pubSource != "openlibrary" {
		t.Errorf("publisher source = %q, want openlibrary", pubSource)
	}
}

// The half TestApplyEnrichedFieldsSkipsAFieldManuallyEditedSinceResolve
// can't exercise: a field can be manually *emptied*, not just manually
// filled, and field_sources still says "manual" either way. A manually
// filled field is skipped by isMissing's emptiness check alone, whatever
// its provenance — this is the case where the provenance check is the
// only thing standing between the field and being silently refilled,
// which is Decision 1's whole point and the failure field_sources exists
// to prevent.
func TestApplyEnrichedFieldsSkipsAClearedManualField(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-11c", Title: "Book", SortTitle: "book", Publisher: "Ace Books"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately cleared, standing in for a person clearing a wrong
	// value while a provider call was still in flight.
	if exists, err := db.UpdateBookField(ctx, id, FieldPublisher, "", time.Now()); err != nil || !exists {
		t.Fatalf("UpdateBookField clear: %v, %v", exists, err)
	}

	_, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldPublisher: "Provider's Guess Press",
	}, map[MetadataField]string{
		FieldPublisher: "openlibrary",
	}, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields = %v, %v", exists, err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Publisher != "" {
		t.Errorf("Publisher = %q, want empty — a deliberately cleared field must not be refilled", book.Publisher)
	}
	var source string
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldPublisher).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "manual" {
		t.Errorf("source = %q, want manual (unchanged)", source)
	}
}

// fieldIsStillMissingTx takes a separate branch for FieldAuthors — a
// book_authors row count, not a books column — which none of the scalar
// tests above ever reach.
func TestApplyEnrichedFieldsSkipsManuallySetAuthors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-11d", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if exists, err := db.UpdateBookAuthors(ctx, id, []string{"Hand-Typed Author"}, time.Now()); err != nil || !exists {
		t.Fatalf("UpdateBookAuthors: %v, %v", exists, err)
	}

	_, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldAuthors: "Provider Author",
	}, map[MetadataField]string{
		FieldAuthors: "openlibrary",
	}, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields = %v, %v", exists, err)
	}

	authors, err := db.ListAuthorsForBook(ctx, id)
	if err != nil || len(authors) != 1 || authors[0] != "Hand-Typed Author" {
		t.Errorf("authors = %v, %v, want [Hand-Typed Author] unchanged", authors, err)
	}
	var source string
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldAuthors).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "manual" {
		t.Errorf("source = %q, want manual (unchanged)", source)
	}
}

// The authors counterpart to TestApplyEnrichedFieldsSkipsAClearedManualField:
// a manually-set author list is non-empty, so it's already skipped by
// isMissing's emptiness check alone regardless of provenance — an author
// list manually *cleared* to none is the case where the manual guard is
// the only thing stopping it from being refilled.
func TestApplyEnrichedFieldsSkipsClearedManualAuthors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-11e", Title: "Book", SortTitle: "book"}, []string{"Embedded Author"})
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately cleared to none, standing in for a person removing a
	// wrong author while a provider call was still in flight.
	if exists, err := db.UpdateBookAuthors(ctx, id, nil, time.Now()); err != nil || !exists {
		t.Fatalf("UpdateBookAuthors clear: %v, %v", exists, err)
	}

	_, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldAuthors: "Provider's Guess Author",
	}, map[MetadataField]string{
		FieldAuthors: "openlibrary",
	}, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields = %v, %v", exists, err)
	}

	authors, err := db.ListAuthorsForBook(ctx, id)
	if err != nil || len(authors) != 0 {
		t.Errorf("authors = %v, %v, want none — a deliberately cleared author list must not be refilled", authors, err)
	}
	var source string
	if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldAuthors).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "manual" {
		t.Errorf("source = %q, want manual (unchanged)", source)
	}
}

// cover is the field's provenance round-trip: ApplyEnrichedFields writes
// cover_path exactly like any other scalar column, through the same
// fieldIsStillMissingTx re-check, and FieldSourcesForBook reports the
// provider that answered it — none of which is specific to cover, but
// nothing exercised the field CHECK constraint's new value until now.
func TestApplyEnrichedFieldsWritesCoverPathWithCoverProvenance(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-cover", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldCover: "covers/ab/abcdef.jpg",
	}, map[MetadataField]string{
		FieldCover: "openlibrary",
	}, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields = %v, %v", exists, err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.CoverPath != "covers/ab/abcdef.jpg" {
		t.Errorf("CoverPath = %q, want the stored path", book.CoverPath)
	}

	sources, err := db.FieldSourcesForBook(ctx, id)
	if err != nil {
		t.Fatalf("FieldSourcesForBook: %v", err)
	}
	if sources[FieldCover] != "openlibrary" {
		t.Errorf("sources[cover] = %q, want openlibrary", sources[FieldCover])
	}
}

// A book that already has a cover is not reconsidered — the same
// isMissing rule every other field follows, exercised here because a
// non-empty cover_path is the scanner's own "looked, found nothing"
// convention and must not be confused with "not yet looked at".
func TestApplyEnrichedFieldsSkipsABookThatAlreadyHasACover(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-cover-2", Title: "Book", SortTitle: "book", CoverPath: "covers/existing.jpg"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldCover: "covers/provider-fetched.jpg",
	}, map[MetadataField]string{
		FieldCover: "openlibrary",
	}, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields = %v, %v", exists, err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.CoverPath != "covers/existing.jpg" {
		t.Errorf("CoverPath = %q, want the existing path left alone", book.CoverPath)
	}
}

func TestApplyEnrichedFieldsUnknownBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, exists, err := db.ApplyEnrichedFields(ctx, 99999, map[MetadataField]string{FieldPublisher: "Press"}, map[MetadataField]string{FieldPublisher: "openlibrary"}, time.Now())
	if err != nil || exists {
		t.Errorf("ApplyEnrichedFields for an unknown book = %v, %v; want false, nil", exists, err)
	}
}

func TestFieldSourcesForBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-9", Title: "Book", SortTitle: "book", Publisher: "Ace"}, []string{"An Author"})
	if err != nil {
		t.Fatal(err)
	}

	sources, err := db.FieldSourcesForBook(ctx, id)
	if err != nil {
		t.Fatalf("FieldSourcesForBook: %v", err)
	}
	if sources[FieldTitle] != "embedded" || sources[FieldPublisher] != "embedded" || sources[FieldAuthors] != "embedded" {
		t.Errorf("sources = %v, want embedded for title/publisher/authors", sources)
	}
	if _, ok := sources[FieldISBN]; ok {
		t.Errorf("sources = %v, want no row for an empty, never-set field", sources)
	}
}

// A book whose earlier embedded-cover store failed carries cover_retry;
// filling cover_path from a provider has to clear it, since the scanner
// skips its stat check entirely while the marker is set and would
// re-extract the embedded cover over the provider's one on the next sweep.
func TestApplyEnrichedFieldsClearsCoverRetry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, Book{
		ContentHash: "cover-retry-hash", Title: "Book", SortTitle: "book", CoverRetry: true,
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	values := map[MetadataField]string{FieldCover: "covers/cover-retry-hash.jpg"}
	sourceName := map[MetadataField]string{FieldCover: "openlibrary"}
	if _, _, err := db.ApplyEnrichedFields(ctx, id, values, sourceName, time.Now()); err != nil {
		t.Fatalf("ApplyEnrichedFields: %v", err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID: %+v, %v", book, err)
	}
	if book.CoverPath != "covers/cover-retry-hash.jpg" {
		t.Errorf("CoverPath = %q, want the enriched path", book.CoverPath)
	}
	if book.CoverRetry {
		t.Error("CoverRetry is still set after a cover was stored")
	}
}

func TestMarkEnrichmentDoneRecordsUpdatedFields(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-fields-1", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimNextEnrichment(ctx, time.Now())
	if err != nil || job == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", job, err)
	}

	if err := db.MarkEnrichmentDone(ctx, job.ID, []MetadataField{FieldPublisher, FieldDescription}, time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetEnrichmentJob(ctx, job.ID)
	if err != nil || got == nil {
		t.Fatalf("GetEnrichmentJob: %+v, %v", got, err)
	}
	if got.UpdatedFields != "publisher,description" {
		t.Errorf("UpdatedFields = %q, want %q", got.UpdatedFields, "publisher,description")
	}
	if got.Status != EnrichmentDone {
		t.Errorf("Status = %q, want done", got.Status)
	}
}

// Nothing written is the ordinary case, and it must be storable as such —
// an empty list, not an absent job or a failure.
func TestMarkEnrichmentDoneWithNoFields(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-fields-2", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, _ := db.ClaimNextEnrichment(ctx, time.Now())
	if err := db.MarkEnrichmentDone(ctx, job.ID, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetEnrichmentJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedFields != "" {
		t.Errorf("UpdatedFields = %q, want empty", got.UpdatedFields)
	}
	if got.Status != EnrichmentDone {
		t.Errorf("Status = %q, want done — nothing to add is a success", got.Status)
	}
}

func TestGetEnrichmentJobUnknownIsNotAnError(t *testing.T) {
	db := openTestDB(t)
	job, err := db.GetEnrichmentJob(context.Background(), 9999)
	if err != nil {
		t.Fatalf("GetEnrichmentJob: %v", err)
	}
	if job != nil {
		t.Errorf("job = %+v, want nil", job)
	}
}

func TestLatestEnrichmentForBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-latest", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if job, err := db.LatestEnrichmentForBook(ctx, id); err != nil || job != nil {
		t.Fatalf("LatestEnrichmentForBook before any job = %+v, %v; want nil, nil", job, err)
	}

	older := time.Now().Add(-time.Hour)
	if _, err := db.EnqueueEnrichment(ctx, id, older); err != nil {
		t.Fatal(err)
	}
	first, _ := db.ClaimNextEnrichment(ctx, older)
	if err := db.MarkEnrichmentDone(ctx, first.ID, nil, older); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if _, err := db.EnqueueEnrichment(ctx, id, now); err != nil {
		t.Fatal(err)
	}

	job, err := db.LatestEnrichmentForBook(ctx, id)
	if err != nil || job == nil {
		t.Fatalf("LatestEnrichmentForBook: %+v, %v", job, err)
	}
	if job.ID == first.ID {
		t.Errorf("returned the older job; want the newest, so a page loaded mid-run resumes the right one")
	}
	if job.Status != EnrichmentQueued {
		t.Errorf("Status = %q, want queued", job.Status)
	}
}

// The list is display text, so the same input has to read the same way
// twice — which ranging over the caller's map would not guarantee.
func TestApplyEnrichedFieldsReportsWrittenFieldsInAFixedOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-order", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	values := map[MetadataField]string{
		FieldDescription:   "A description",
		FieldPublisher:     "Gollancz",
		FieldLanguage:      "en",
		FieldPublishedDate: "1984",
	}
	sources := map[MetadataField]string{}
	for f := range values {
		sources[f] = "openlibrary"
	}

	written, exists, err := db.ApplyEnrichedFields(ctx, id, values, sources, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields: %v, exists=%v", err, exists)
	}
	want := []MetadataField{FieldPublisher, FieldPublishedDate, FieldLanguage, FieldDescription}
	if len(written) != len(want) {
		t.Fatalf("written = %v, want %v", written, want)
	}
	for i := range want {
		if written[i] != want[i] {
			t.Fatalf("written = %v, want %v (metadataFieldOrder, not map order)", written, want)
		}
	}
}

// A field a concurrent edit has already filled is skipped by the
// re-check, and must not be reported as written — the result line exists
// to say what this run did.
func TestApplyEnrichedFieldsOmitsSkippedFieldsFromWritten(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-skip", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Someone fills the publisher by hand first.
	if _, err := db.UpdateBookField(ctx, id, FieldPublisher, "Hand-typed", time.Now()); err != nil {
		t.Fatal(err)
	}

	written, exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
		FieldPublisher: "Provider Press",
		FieldLanguage:  "en",
	}, map[MetadataField]string{
		FieldPublisher: "openlibrary",
		FieldLanguage:  "openlibrary",
	}, time.Now())
	if err != nil || !exists {
		t.Fatalf("ApplyEnrichedFields: %v, exists=%v", err, exists)
	}
	if len(written) != 1 || written[0] != FieldLanguage {
		t.Errorf("written = %v, want just [language] — publisher was already manual", written)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Publisher != "Hand-typed" {
		t.Errorf("Publisher = %q, want the hand-typed value untouched", book.Publisher)
	}
}
