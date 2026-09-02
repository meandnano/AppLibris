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
