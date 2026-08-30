package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateAndFindBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	id, err := db.CreateBook(ctx, Book{
		ContentHash: "hash-1",
		Title:       "Example Book",
		SortTitle:   "Example Book",
		Format:      "epub",
	}, []string{"Jane Doe", "John Roe"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	fileID, err := db.UpsertBookFile(ctx, id, "/library/example.epub", 1234, mtime)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if fileID == 0 {
		t.Fatal("UpsertBookFile: want a nonzero file id")
	}

	byHash, err := db.FindBookByContentHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindBookByContentHash: %v", err)
	}
	if byHash == nil || byHash.ID != id {
		t.Errorf("FindBookByContentHash = %+v, want id %d", byHash, id)
	}

	byPath, err := db.FindFileByPath(ctx, "/library/example.epub")
	if err != nil {
		t.Fatalf("FindFileByPath: %v", err)
	}
	if byPath == nil {
		t.Fatal("FindFileByPath: want a book_files row, got nil")
	}
	if byPath.ID != fileID || byPath.BookID != id || byPath.FileSize != 1234 {
		t.Errorf("FindFileByPath = %+v, unexpected fields", byPath)
	}
	if !byPath.ModifiedAt.Equal(mtime) {
		t.Errorf("ModifiedAt = %v, want %v", byPath.ModifiedAt, mtime)
	}

	var authorCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM book_authors WHERE book_id = ?`, id).Scan(&authorCount); err != nil {
		t.Fatalf("count book_authors: %v", err)
	}
	if authorCount != 2 {
		t.Errorf("book_authors count = %d, want 2", authorCount)
	}
}

func TestFindNotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	byHash, err := db.FindBookByContentHash(ctx, "no-such-hash")
	if err != nil || byHash != nil {
		t.Errorf("FindBookByContentHash = %+v, %v; want nil, nil", byHash, err)
	}

	byPath, err := db.FindFileByPath(ctx, "/nowhere.epub")
	if err != nil || byPath != nil {
		t.Errorf("FindFileByPath = %+v, %v; want nil, nil", byPath, err)
	}
}

func TestSharedAuthorIsReused(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateBook(ctx, Book{ContentHash: "h1", Title: "Book One", Format: "epub"}, []string{"Shared Author"}); err != nil {
		t.Fatalf("CreateBook 1: %v", err)
	}
	if _, err := db.CreateBook(ctx, Book{ContentHash: "h2", Title: "Book Two", Format: "epub"}, []string{"Shared Author"}); err != nil {
		t.Fatalf("CreateBook 2: %v", err)
	}

	var authorCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM authors WHERE name = ?`, "Shared Author").Scan(&authorCount); err != nil {
		t.Fatalf("count authors: %v", err)
	}
	if authorCount != 1 {
		t.Errorf("authors count for shared name = %d, want 1", authorCount)
	}
}

func TestUpsertBookFileInsertsNewLocation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Multi-location Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	id1, err := db.UpsertBookFile(ctx, bookID, "/a/copy.epub", 100, mtime)
	if err != nil {
		t.Fatalf("UpsertBookFile first location: %v", err)
	}
	id2, err := db.UpsertBookFile(ctx, bookID, "/b/copy.epub", 100, mtime)
	if err != nil {
		t.Fatalf("UpsertBookFile second location: %v", err)
	}
	if id1 == id2 {
		t.Errorf("two distinct paths got the same book_files id %d", id1)
	}

	f1, err := db.FindFileByPath(ctx, "/a/copy.epub")
	if err != nil || f1 == nil || f1.BookID != bookID {
		t.Errorf("FindFileByPath /a/copy.epub = %+v, %v; want book id %d", f1, err, bookID)
	}
	f2, err := db.FindFileByPath(ctx, "/b/copy.epub")
	if err != nil || f2 == nil || f2.BookID != bookID {
		t.Errorf("FindFileByPath /b/copy.epub = %+v, %v; want book id %d", f2, err, bookID)
	}
}

func TestUpsertBookFileUpdatesOnConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	firstBookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Old Content", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook 1: %v", err)
	}
	secondBookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-2", Title: "New Content", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook 2: %v", err)
	}

	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	originalFileID, err := db.UpsertBookFile(ctx, firstBookID, "/path.epub", 100, mtime)
	if err != nil {
		t.Fatalf("UpsertBookFile initial: %v", err)
	}

	newMtime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	updatedFileID, err := db.UpsertBookFile(ctx, secondBookID, "/path.epub", 200, newMtime)
	if err != nil {
		t.Fatalf("UpsertBookFile conflict: %v", err)
	}
	if updatedFileID != originalFileID {
		t.Errorf("conflict path returned file id %d, want the same row %d", updatedFileID, originalFileID)
	}

	f, err := db.FindFileByPath(ctx, "/path.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	if f.BookID != secondBookID || f.FileSize != 200 || !f.ModifiedAt.Equal(newMtime) {
		t.Errorf("book_files row after conflict = %+v, want book %d size 200 mtime %v", f, secondBookID, newMtime)
	}
}

func TestUpdateBookFileStat(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Touched Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	fileID, err := db.UpsertBookFile(ctx, bookID, "/path.epub", 100, time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	newMtime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := db.UpdateBookFileStat(ctx, fileID, 150, newMtime); err != nil {
		t.Fatalf("UpdateBookFileStat: %v", err)
	}

	updated, err := db.FindFileByPath(ctx, "/path.epub")
	if err != nil || updated == nil {
		t.Fatalf("FindFileByPath: %+v, %v", updated, err)
	}
	if updated.FileSize != 150 || !updated.ModifiedAt.Equal(newMtime) {
		t.Errorf("updated file = %+v, unexpected fields", updated)
	}
}

func TestListBooks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bID, err := db.CreateBook(ctx, Book{ContentHash: "hash-b", Title: "Book B", SortTitle: "Book B"}, nil)
	if err != nil {
		t.Fatalf("CreateBook B: %v", err)
	}
	aID, err := db.CreateBook(ctx, Book{ContentHash: "hash-a", Title: "Book A", SortTitle: "Book A"}, nil)
	if err != nil {
		t.Fatalf("CreateBook A: %v", err)
	}

	books, err := db.ListBooks(ctx)
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("ListBooks returned %d books, want 2", len(books))
	}
	if books[0].ID != aID || books[1].ID != bID {
		t.Errorf("ListBooks order = [%d, %d], want [%d, %d] (sort_title order)", books[0].ID, books[1].ID, aID, bID)
	}
}

func TestListBookAuthors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	soloID, err := db.CreateBook(ctx, Book{ContentHash: "hash-solo", Title: "Solo Author Book"}, []string{"Jane Doe"})
	if err != nil {
		t.Fatalf("CreateBook solo: %v", err)
	}
	multiID, err := db.CreateBook(ctx, Book{ContentHash: "hash-multi", Title: "Multi Author Book"}, []string{"Jane Doe", "John Roe"})
	if err != nil {
		t.Fatalf("CreateBook multi: %v", err)
	}
	noneID, err := db.CreateBook(ctx, Book{ContentHash: "hash-none", Title: "No Author Book"}, nil)
	if err != nil {
		t.Fatalf("CreateBook none: %v", err)
	}

	authors, err := db.ListBookAuthors(ctx)
	if err != nil {
		t.Fatalf("ListBookAuthors: %v", err)
	}

	if got := authors[soloID]; len(got) != 1 || got[0] != "Jane Doe" {
		t.Errorf("authors[solo] = %v, want [Jane Doe]", got)
	}
	if got := authors[multiID]; len(got) != 2 {
		t.Errorf("authors[multi] = %v, want 2 authors", got)
	}
	if got := authors[noneID]; got != nil {
		t.Errorf("authors[none] = %v, want no entry", got)
	}
}
