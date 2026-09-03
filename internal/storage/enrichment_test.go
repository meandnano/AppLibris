package storage

import (
	"context"
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
	if err := db.MarkEnrichmentDone(ctx, job.ID, now.Add(2*time.Minute)); err != nil {
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
	if err := db.MarkEnrichmentDone(ctx, jobID, now); err != nil {
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
	if err := db.MarkEnrichmentDone(ctx, jobID, now); err != nil {
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
	if err := db.MarkEnrichmentDone(ctx, job.ID, now); err != nil {
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

	exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
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

	books, err := db.SearchBooks(ctx, `"story"*`)
	if err != nil || len(books) != 1 {
		t.Errorf("search on enriched description = %v, %v", books, err)
	}
}

// A partial failure must not leave a partially-enriched book: one field
// applying and a sibling failing has to roll both back, or a book ends up
// with some fields carrying provider provenance and others not reflecting
// what was actually saved. This mixes two distinct sources in the same
// call — the shape a real multi-provider job produces — so the rollback
// has to undo the valid field's write together with the invalid one's,
// not just the invalid one's own no-op.
func TestApplyEnrichedFieldsRollsBackOnFailureAcrossMixedProviders(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "enrich-10", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
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

// The regression a review comment called out: Resolve takes its snapshot
// of what's missing before any provider is asked, and a provider can take
// a while to answer. If a person fills or deliberately clears the same
// field in between, ApplyEnrichedFields must not clobber it with the
// provider's now-stale answer — the field_sources/manual guarantee has to
// win. fieldIsStillMissingTx's fresh re-read inside the write transaction
// is what makes this safe without any explicit locking: the manual edit
// below stands in for one that committed while a provider call was still
// in flight.
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
	exists, err := db.ApplyEnrichedFields(ctx, id, map[MetadataField]string{
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

func TestApplyEnrichedFieldsUnknownBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	exists, err := db.ApplyEnrichedFields(ctx, 99999, map[MetadataField]string{FieldPublisher: "Press"}, map[MetadataField]string{FieldPublisher: "openlibrary"}, time.Now())
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
