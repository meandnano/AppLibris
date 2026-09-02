package service

import (
	"context"
	"path/filepath"
	"testing"

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
