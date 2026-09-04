package service

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

func TestListBooksAssemblesAuthors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	soloID, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-solo", Title: "Solo Book", SortTitle: "Solo Book", Format: "epub", CoverPath: "/covers/solo.jpg"}, []string{"Jane Doe"})
	if err != nil {
		t.Fatalf("CreateBook solo: %v", err)
	}
	noneID, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-none", Title: "No Author Book", SortTitle: "No Author Book", Format: "fb2"}, nil)
	if err != nil {
		t.Fatalf("CreateBook none: %v", err)
	}

	books, err := svc.ListBooks(ctx)
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("ListBooks returned %d books, want 2", len(books))
	}

	byID := make(map[int64]BookSummary, len(books))
	for _, b := range books {
		byID[b.ID] = b
	}

	solo, ok := byID[soloID]
	if !ok {
		t.Fatalf("solo book %d missing from ListBooks", soloID)
	}
	if len(solo.Authors) != 1 || solo.Authors[0] != "Jane Doe" {
		t.Errorf("solo.Authors = %v, want [Jane Doe]", solo.Authors)
	}
	if solo.Format != "epub" || solo.CoverPath != "/covers/solo.jpg" {
		t.Errorf("solo book = %+v, unexpected fields", solo)
	}

	none, ok := byID[noneID]
	if !ok {
		t.Fatalf("no-author book %d missing from ListBooks", noneID)
	}
	if len(none.Authors) != 0 {
		t.Errorf("none.Authors = %v, want empty", none.Authors)
	}
}

// The map-absence contract translated at the service boundary: storage's
// CountFilesByBook omits a book with one location rather than mapping it to
// zero, and ListBooks must turn that absence into 1, not 0.
func TestListBooksReportsLocations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)
	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

	oneID, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-one", Title: "One Location", SortTitle: "One Location", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook one: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, oneID, "/one.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile one: %v", err)
	}

	twoID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-two", Title: "Two Locations", SortTitle: "Two Locations", Format: "epub"}, nil, "/two-a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile two: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, twoID, "/two-b.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile two-b: %v", err)
	}

	books, err := svc.ListBooks(ctx)
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	byID := make(map[int64]BookSummary, len(books))
	for _, b := range books {
		byID[b.ID] = b
	}

	if got := byID[oneID].Locations; got != 1 {
		t.Errorf("one-location book Locations = %d, want 1", got)
	}
	if got := byID[twoID].Locations; got != 2 {
		t.Errorf("two-location book Locations = %d, want 2", got)
	}
}

// The regression this guards: a future refactor splitting ListBooks and
// SearchBooks off from their shared summarize helper could silently drop
// the marker from search results only.
func TestSearchBooksReportsLocations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)
	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

	matchID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-1", Title: "Piranesi", SortTitle: "Piranesi", Format: "epub"}, nil, "/a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, matchID, "/b.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	result, err := svc.SearchBooks(ctx, "Piranesi")
	if err != nil {
		t.Fatalf("SearchBooks: %v", err)
	}
	if len(result.Books) != 1 || result.Books[0].Locations != 2 {
		t.Fatalf("SearchBooks(Piranesi) = %+v, want one book with Locations 2", result.Books)
	}
}

func TestSearchBooksBlankQueryReturnsFullList(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	if _, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "A Book", SortTitle: "A Book", Format: "epub"}, nil); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	for _, q := range []string{"", "   ", "\t"} {
		result, err := svc.SearchBooks(ctx, q)
		if err != nil {
			t.Fatalf("SearchBooks(%q): %v", q, err)
		}
		if result.Searched {
			t.Errorf("SearchBooks(%q) reported a search; a query that sanitizes to nothing is not one", q)
		}
		if len(result.Books) != 1 {
			t.Errorf("SearchBooks(%q) = %d books, want the full list (1)", q, len(result.Books))
		}
	}
}

func TestSearchBooksMatchReturnsSummaryWithAuthors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	matchID, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "Piranesi", SortTitle: "Piranesi", Format: "epub"}, []string{"Susanna Clarke"})
	if err != nil {
		t.Fatalf("CreateBook match: %v", err)
	}
	if _, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-2", Title: "Unrelated", SortTitle: "Unrelated", Format: "epub"}, nil); err != nil {
		t.Fatalf("CreateBook unrelated: %v", err)
	}

	result, err := svc.SearchBooks(ctx, "Piranesi")
	if err != nil {
		t.Fatalf("SearchBooks: %v", err)
	}
	if !result.Searched {
		t.Error("SearchBooks(Piranesi) reported no search, want a search")
	}
	if len(result.Books) != 1 || result.Books[0].ID != matchID {
		t.Fatalf("SearchBooks(Piranesi) = %+v, want exactly the matching book", result.Books)
	}
	if len(result.Books[0].Authors) != 1 || result.Books[0].Authors[0] != "Susanna Clarke" {
		t.Errorf("SearchBooks result Authors = %v, want [Susanna Clarke]", result.Books[0].Authors)
	}
	if len(result.Fields) != 1 || result.Fields[0] != "title" {
		t.Errorf("SearchBooks result Fields = %v, want [title] — the title is what matched", result.Fields)
	}
}

func TestCountBooksIsUnaffectedBySearchFilter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	if _, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "Piranesi", SortTitle: "Piranesi", Format: "epub"}, nil); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-2", Title: "Unrelated", SortTitle: "Unrelated", Format: "epub"}, nil); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	count, err := svc.CountBooks(ctx)
	if err != nil {
		t.Fatalf("CountBooks: %v", err)
	}
	if count != 2 {
		t.Errorf("CountBooks = %d, want 2 (the library total, not a search-filtered count)", count)
	}
}

func TestGetBookAssemblesFullDetail(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	id, orphanedID, _, err := db.CreateBookWithFile(ctx, storage.Book{
		ContentHash:   "hash-1",
		Title:         "The Left Hand of Darkness",
		SortTitle:     "Left Hand of Darkness, The",
		Publisher:     "Ace Books",
		PublishedDate: "1969-03-01",
		Language:      "en",
		ISBN:          "9780441478125",
		Description:   "A lone envoy arrives on the frozen world of Winter.",
		CoverPath:     "/covers/hash-1.jpg",
		Format:        "epub",
	}, []string{"Ursula K. Le Guin"}, "b/second.epub", 1234, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if orphanedID != 0 {
		t.Fatalf("unexpected orphaned book %d", orphanedID)
	}
	if _, err := db.UpsertBookFile(ctx, id, "a/first.epub", 1234, mtime); err != nil {
		t.Fatalf("UpsertBookFile second location: %v", err)
	}

	detail, err := svc.GetBook(ctx, id)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if detail == nil {
		t.Fatal("GetBook = nil, want the assembled detail")
	}

	if detail.Title != "The Left Hand of Darkness" {
		t.Errorf("Title = %q", detail.Title)
	}
	if len(detail.Authors) != 1 || detail.Authors[0] != "Ursula K. Le Guin" {
		t.Errorf("Authors = %v, want [Ursula K. Le Guin]", detail.Authors)
	}
	if detail.Publisher != "Ace Books" || detail.PublishedDate != "1969-03-01" || detail.Language != "en" || detail.ISBN != "9780441478125" {
		t.Errorf("metadata fields = %+v, unexpected values", detail)
	}
	if detail.Description != "A lone envoy arrives on the frozen world of Winter." {
		t.Errorf("Description = %q", detail.Description)
	}
	if detail.CoverPath != "/covers/hash-1.jpg" {
		t.Errorf("CoverPath = %q, want the stored path", detail.CoverPath)
	}
	if detail.Format != "epub" {
		t.Errorf("Format = %q, want epub", detail.Format)
	}
	if detail.AddedAt.IsZero() {
		t.Error("AddedAt is zero; the detail page renders it as the added date")
	}
	if detail.FileSize != 1234 {
		t.Errorf("FileSize = %d, want 1234 (taken from a location, all locations byte-identical)", detail.FileSize)
	}
	if !detail.HasFileSize {
		t.Error("HasFileSize = false for a book with locations")
	}
	if len(detail.Locations) != 2 {
		t.Fatalf("Locations = %+v, want 2", detail.Locations)
	}
	if detail.Locations[0].Path != "a/first.epub" || detail.Locations[1].Path != "b/second.epub" {
		t.Errorf("Locations paths = [%q, %q], want ordered by path", detail.Locations[0].Path, detail.Locations[1].Path)
	}
	if detail.Locations[0].Missing || detail.Locations[1].Missing {
		t.Errorf("Locations = %+v, want neither missing", detail.Locations)
	}
}

// A book with no location has no size to report, which is a different
// thing from a size of zero — the transport renders an em dash for the
// first and "0 B" for the second, and only HasFileSize tells them apart.
func TestGetBookReportsNoFileSizeWithoutLocations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	id, err := db.CreateBook(ctx, storage.Book{
		ContentHash: "hash-1", Title: "No Locations", SortTitle: "No Locations", Format: "epub",
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	detail, err := svc.GetBook(ctx, id)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if detail == nil {
		t.Fatal("GetBook = nil for an existing book")
	}
	if detail.HasFileSize {
		t.Error("HasFileSize = true for a book with no location")
	}
	if len(detail.Locations) != 0 {
		t.Errorf("Locations = %+v, want none", detail.Locations)
	}
}

// The counterpart: a real zero-byte file is a known size.
func TestGetBookReportsZeroByteLocationAsAKnownSize(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	id, _, _, err := db.CreateBookWithFile(ctx, storage.Book{
		ContentHash: "hash-1", Title: "Empty File", SortTitle: "Empty File", Format: "epub",
	}, nil, "empty.epub", 0, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	detail, err := svc.GetBook(ctx, id)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if !detail.HasFileSize {
		t.Error("HasFileSize = false for a location that is genuinely zero bytes")
	}
	if detail.FileSize != 0 {
		t.Errorf("FileSize = %d, want 0", detail.FileSize)
	}
}

func TestGetBookUnknownIDReturnsNilNil(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	detail, err := svc.GetBook(ctx, 99999)
	if err != nil {
		t.Fatalf("GetBook(unknown): %v", err)
	}
	if detail != nil {
		t.Errorf("GetBook(unknown) = %+v, want nil", detail)
	}
}

func TestQueueSendInvalidAddressQueuesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "Book", SortTitle: "Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	state, err := svc.QueueSend(ctx, id, "not-an-address", "")
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("QueueSend error = %v, want ErrInvalidAddress", err)
	}
	if state != nil {
		t.Errorf("QueueSend = %+v, want nil on error", state)
	}

	recipients, err := svc.Recipients(ctx)
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	if len(recipients) != 0 {
		t.Errorf("Recipients = %+v, want none saved for a rejected address", recipients)
	}
}

func TestQueueSendValidAddressCreatesRecipientAndCallsNotify(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	notifyCount := 0
	svc.Notify = func() { notifyCount++ }

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "Piranesi", SortTitle: "Piranesi", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	// A display name in the pasted address must not end up stored — only
	// the mailbox does.
	state, err := svc.QueueSend(ctx, id, "Reader <reader@kindle.com>", "Mine")
	if err != nil {
		t.Fatalf("QueueSend: %v", err)
	}
	if state == nil {
		t.Fatal("QueueSend = nil, want the queued state")
	}
	if state.Status != string(storage.SendQueued) {
		t.Errorf("Status = %q, want queued", state.Status)
	}
	if state.Recipient != "reader@kindle.com" {
		t.Errorf("Recipient = %q, want the display name stripped", state.Recipient)
	}
	if notifyCount != 1 {
		t.Errorf("Notify called %d times, want exactly 1", notifyCount)
	}

	recipients, err := svc.Recipients(ctx)
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	if len(recipients) != 1 || recipients[0].Address != "reader@kindle.com" || recipients[0].Label != "Mine" {
		t.Errorf("Recipients = %+v, want one entry for reader@kindle.com labeled Mine", recipients)
	}

	latest, err := svc.LatestSend(ctx, id)
	if err != nil {
		t.Fatalf("LatestSend: %v", err)
	}
	if latest == nil || latest.ID != state.ID {
		t.Errorf("LatestSend = %+v, want the just-queued send %+v", latest, state)
	}

	got, err := svc.SendState(ctx, state.ID)
	if err != nil {
		t.Fatalf("SendState: %v", err)
	}
	if got == nil || got.ID != state.ID {
		t.Errorf("SendState = %+v, want the queued send", got)
	}
}

func TestQueueSendUnknownBookReturnsNilNil(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	state, err := svc.QueueSend(ctx, 99999, "reader@kindle.com", "")
	if err != nil {
		t.Fatalf("QueueSend(unknown book): %v", err)
	}
	if state != nil {
		t.Errorf("QueueSend(unknown book) = %+v, want nil", state)
	}
}

func TestLatestSendUnsentBookReturnsNilNil(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "Book", SortTitle: "Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	latest, err := svc.LatestSend(ctx, id)
	if err != nil {
		t.Fatalf("LatestSend: %v", err)
	}
	if latest != nil {
		t.Errorf("LatestSend(never sent) = %+v, want nil", latest)
	}
}

// enqueueSendsAt queues n sends for a fresh book, one second apart ending
// at now, so callers get a run of distinct, ordered timestamps without
// caring about the exact values.
func enqueueSendsAt(t *testing.T, db *storage.DB, now time.Time, n int) {
	t.Helper()
	ctx := context.Background()
	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-" + itoa(int(now.UnixNano())), Title: "Book", SortTitle: "Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	for i := 0; i < n; i++ {
		at := now.Add(-time.Duration(n-1-i) * time.Second)
		if _, err := db.EnqueueSend(ctx, id, "Book", "reader@kindle.com", at); err != nil {
			t.Fatalf("EnqueueSend %d: %v", i, err)
		}
	}
}

func TestSendHistoryReportsTruncatedOnlyWhenCapBites(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	t.Run("cap-1 rows", func(t *testing.T) {
		db := openTestDB(t)
		svc := New(db)
		svc.now = func() time.Time { return now }
		enqueueSendsAt(t, db, now, SendHistoryLimit-1)

		records, truncated, err := svc.SendHistory(context.Background())
		if err != nil {
			t.Fatalf("SendHistory: %v", err)
		}
		if truncated {
			t.Error("truncated = true with cap-1 rows, want false")
		}
		if len(records) != SendHistoryLimit-1 {
			t.Errorf("len(records) = %d, want %d", len(records), SendHistoryLimit-1)
		}
	})

	t.Run("cap+1 rows", func(t *testing.T) {
		db := openTestDB(t)
		svc := New(db)
		svc.now = func() time.Time { return now }
		enqueueSendsAt(t, db, now, SendHistoryLimit+1)

		records, truncated, err := svc.SendHistory(context.Background())
		if err != nil {
			t.Fatalf("SendHistory: %v", err)
		}
		if !truncated {
			t.Error("truncated = false with cap+1 rows, want true")
		}
		if len(records) != SendHistoryLimit {
			t.Errorf("len(records) = %d, want the cap %d", len(records), SendHistoryLimit)
		}
	})
}

func TestSendHistoryWindowIsMeasuredFromTheServiceClock(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "Book", SortTitle: "Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	tooOld := now.Add(-31 * 24 * time.Hour)
	inWindow := now.Add(-29 * 24 * time.Hour)
	if _, err := db.EnqueueSend(ctx, id, "Book", "excluded@kindle.com", tooOld); err != nil {
		t.Fatalf("EnqueueSend tooOld: %v", err)
	}
	if _, err := db.EnqueueSend(ctx, id, "Book", "included@kindle.com", inWindow); err != nil {
		t.Fatalf("EnqueueSend inWindow: %v", err)
	}

	records, _, err := svc.SendHistory(ctx)
	if err != nil {
		t.Fatalf("SendHistory: %v", err)
	}
	if len(records) != 1 || records[0].Recipient != "included@kindle.com" {
		t.Fatalf("SendHistory = %+v, want only the send queued 29 days ago", records)
	}
}

// The same assertion SendState already carries, pinned here too because
// sendStateFrom and sendRecordFrom now share sendAt — a regression to
// either would break only one of the two screens.
func TestSendHistoryAtIsFinishedAtForTerminalAndQueuedAtForPending(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-1", Title: "Pending Book", SortTitle: "Pending Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook pending: %v", err)
	}
	queuedAt := now.Add(-time.Hour)
	if _, err := db.EnqueueSend(ctx, id, "Pending Book", "reader@kindle.com", queuedAt); err != nil {
		t.Fatalf("EnqueueSend pending: %v", err)
	}

	deliveredID, err := db.CreateBook(ctx, storage.Book{ContentHash: "hash-2", Title: "Delivered Book", SortTitle: "Delivered Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook delivered: %v", err)
	}
	deliveredQueuedAt := now.Add(-2 * time.Hour)
	if _, err := db.EnqueueSend(ctx, deliveredID, "Delivered Book", "reader@kindle.com", deliveredQueuedAt); err != nil {
		t.Fatalf("EnqueueSend delivered: %v", err)
	}
	claimed, err := db.ClaimNextSend(ctx, deliveredQueuedAt.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextSend: %+v, %v", claimed, err)
	}
	finishedAt := now.Add(-time.Minute)
	if err := db.MarkSendDelivered(ctx, claimed.ID, "msg-1", finishedAt); err != nil {
		t.Fatalf("MarkSendDelivered: %v", err)
	}

	records, _, err := svc.SendHistory(ctx)
	if err != nil {
		t.Fatalf("SendHistory: %v", err)
	}
	byTitle := make(map[string]SendRecord, len(records))
	for _, r := range records {
		byTitle[r.BookTitle] = r
	}

	pending, ok := byTitle["Pending Book"]
	if !ok || !pending.At.Equal(queuedAt) {
		t.Errorf("pending record At = %+v, want queued_at %v", pending, queuedAt)
	}
	delivered, ok := byTitle["Delivered Book"]
	if !ok || !delivered.At.Equal(finishedAt) {
		t.Errorf("delivered record At = %+v, want finished_at %v", delivered, finishedAt)
	}
}

func TestRemoveRecipient(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	if _, err := db.CreateRecipient(ctx, "reader@kindle.com", "Mine", time.Now()); err != nil {
		t.Fatalf("CreateRecipient: %v", err)
	}

	removed, err := svc.RemoveRecipient(ctx, "reader@kindle.com")
	if err != nil {
		t.Fatalf("RemoveRecipient: %v", err)
	}
	if !removed {
		t.Error("RemoveRecipient = false, want true")
	}

	recipients, err := svc.Recipients(ctx)
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	if len(recipients) != 0 {
		t.Errorf("Recipients after removal = %+v, want none", recipients)
	}

	again, err := svc.RemoveRecipient(ctx, "reader@kindle.com")
	if err != nil {
		t.Fatalf("RemoveRecipient (already gone): %v", err)
	}
	if again {
		t.Error("RemoveRecipient on an already-removed address = true, want false")
	}
}

func TestEnrichBookQueuesAndNotifies(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)
	poked := 0
	svc.NotifyEnrichment = func() { poked++ }

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "enr-1", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	state, err := svc.EnrichBook(ctx, id)
	if err != nil {
		t.Fatalf("EnrichBook: %v", err)
	}
	if state == nil {
		t.Fatal("EnrichBook returned nil state for an existing book")
	}
	if state.Status != "queued" {
		t.Errorf("Status = %q, want queued", state.Status)
	}
	if state.BookID != id {
		t.Errorf("BookID = %d, want %d", state.BookID, id)
	}
	if len(state.UpdatedFields) != 0 {
		t.Errorf("UpdatedFields = %v on a fresh job, want none", state.UpdatedFields)
	}
	if poked != 1 {
		t.Errorf("NotifyEnrichment called %d times, want 1 — the job should start without waiting for a poll tick", poked)
	}
}

// EnqueueEnrichment is idempotent while a job is queued, so a second press
// makes no second promise — but the caller still wants the job the book
// actually has back, not nil.
func TestEnrichBookTwiceReturnsTheSameQueuedJob(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "enr-2", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	first, err := svc.EnrichBook(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.EnrichBook(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil {
		t.Fatal("second EnrichBook returned nil")
	}
	if first.ID != second.ID {
		t.Errorf("second press produced job %d, want the queued %d — one queued promise per book", second.ID, first.ID)
	}
}

func TestEnrichBookUnknownBookIsNotAnError(t *testing.T) {
	db := openTestDB(t)
	svc := New(db)
	svc.NotifyEnrichment = func() { t.Error("notified for a book that does not exist") }

	state, err := svc.EnrichBook(context.Background(), 4242)
	if err != nil {
		t.Fatalf("EnrichBook: %v", err)
	}
	if state != nil {
		t.Errorf("state = %+v, want nil for an unknown book", state)
	}
}

func TestEnrichmentStateSplitsUpdatedFields(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "enr-3", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, _ := db.ClaimNextEnrichment(ctx, time.Now())
	finished := time.Now()
	if err := db.MarkEnrichmentDone(ctx, job.ID, []storage.MetadataField{storage.FieldPublisher, storage.FieldLanguage}, finished); err != nil {
		t.Fatal(err)
	}

	state, err := svc.EnrichmentState(ctx, job.ID)
	if err != nil || state == nil {
		t.Fatalf("EnrichmentState: %+v, %v", state, err)
	}
	if len(state.UpdatedFields) != 2 || state.UpdatedFields[0] != "publisher" || state.UpdatedFields[1] != "language" {
		t.Errorf("UpdatedFields = %v, want [publisher language]", state.UpdatedFields)
	}
	// Terminal, so At is finished_at rather than queued_at — the same
	// collapse sendAt makes.
	if state.At.IsZero() || state.At.Before(job.QueuedAt) {
		t.Errorf("At = %v, want the finish time", state.At)
	}
}

func TestEnrichmentStateUnknownJobIsNotAnError(t *testing.T) {
	db := openTestDB(t)
	svc := New(db)
	state, err := svc.EnrichmentState(context.Background(), 9999)
	if err != nil {
		t.Fatalf("EnrichmentState: %v", err)
	}
	if state != nil {
		t.Errorf("state = %+v, want nil", state)
	}
}

func TestLatestEnrichmentNoJob(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)
	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "enr-4", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := svc.LatestEnrichment(ctx, id)
	if err != nil {
		t.Fatalf("LatestEnrichment: %v", err)
	}
	if state != nil {
		t.Errorf("state = %+v, want nil for a book never enriched", state)
	}
}

// The detail page needs provenance to render Decision 1's markers.
func TestGetBookCarriesFieldSources(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	svc := New(db)

	id, err := db.CreateBook(ctx, storage.Book{ContentHash: "enr-5", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplyEnrichedFields(ctx, id, map[storage.MetadataField]string{
		storage.FieldPublisher: "Gollancz",
	}, map[storage.MetadataField]string{
		storage.FieldPublisher: "openlibrary",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	detail, err := svc.GetBook(ctx, id)
	if err != nil || detail == nil {
		t.Fatalf("GetBook: %+v, %v", detail, err)
	}
	if got := detail.FieldSources["publisher"]; got != "openlibrary" {
		t.Errorf("FieldSources[publisher] = %q, want openlibrary", got)
	}
	if got := detail.FieldSources["title"]; got != "embedded" {
		t.Errorf("FieldSources[title] = %q, want embedded — the scanner set it at creation", got)
	}
}
