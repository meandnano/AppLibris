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
