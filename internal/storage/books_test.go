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
		FilePath:    "/library/example.epub",
		Format:      "epub",
		FileSize:    1234,
		ModifiedAt:  mtime,
	}, []string{"Jane Doe", "John Roe"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	byPath, err := db.FindBookByPath(ctx, "/library/example.epub")
	if err != nil {
		t.Fatalf("FindBookByPath: %v", err)
	}
	if byPath == nil {
		t.Fatal("FindBookByPath: want a book, got nil")
	}
	if byPath.ID != id || byPath.ContentHash != "hash-1" || byPath.FileSize != 1234 {
		t.Errorf("FindBookByPath = %+v, unexpected fields", byPath)
	}
	if !byPath.ModifiedAt.Equal(mtime) {
		t.Errorf("ModifiedAt = %v, want %v", byPath.ModifiedAt, mtime)
	}

	byHash, err := db.FindBookByContentHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindBookByContentHash: %v", err)
	}
	if byHash == nil || byHash.ID != id {
		t.Errorf("FindBookByContentHash = %+v, want id %d", byHash, id)
	}

	var authorCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM book_authors WHERE book_id = ?`, id).Scan(&authorCount); err != nil {
		t.Fatalf("count book_authors: %v", err)
	}
	if authorCount != 2 {
		t.Errorf("book_authors count = %d, want 2", authorCount)
	}
}

func TestFindBookNotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	byPath, err := db.FindBookByPath(ctx, "/nowhere.epub")
	if err != nil || byPath != nil {
		t.Errorf("FindBookByPath = %+v, %v; want nil, nil", byPath, err)
	}

	byHash, err := db.FindBookByContentHash(ctx, "no-such-hash")
	if err != nil || byHash != nil {
		t.Errorf("FindBookByContentHash = %+v, %v; want nil, nil", byHash, err)
	}
}

func TestSharedAuthorIsReused(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateBook(ctx, Book{ContentHash: "h1", Title: "Book One", FilePath: "/a.epub", Format: "epub"}, []string{"Shared Author"}); err != nil {
		t.Fatalf("CreateBook 1: %v", err)
	}
	if _, err := db.CreateBook(ctx, Book{ContentHash: "h2", Title: "Book Two", FilePath: "/b.epub", Format: "epub"}, []string{"Shared Author"}); err != nil {
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

func TestUpdateBookFileLocation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Moved Book", FilePath: "/old/path.epub", Format: "epub", FileSize: 100}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	newMtime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := db.UpdateBookFileLocation(ctx, id, "/new/path.epub", 200, newMtime); err != nil {
		t.Fatalf("UpdateBookFileLocation: %v", err)
	}

	moved, err := db.FindBookByContentHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindBookByContentHash: %v", err)
	}
	if moved.ID != id {
		t.Errorf("moved book id = %d, want %d (should be same row, not a new one)", moved.ID, id)
	}
	if moved.FilePath != "/new/path.epub" || moved.FileSize != 200 || !moved.ModifiedAt.Equal(newMtime) {
		t.Errorf("moved book = %+v, unexpected fields", moved)
	}
	if moved.Title != "Moved Book" {
		t.Errorf("Title = %q, want unchanged %q", moved.Title, "Moved Book")
	}

	stillFindableAtOldPath, err := db.FindBookByPath(ctx, "/old/path.epub")
	if err != nil {
		t.Fatalf("FindBookByPath old: %v", err)
	}
	if stillFindableAtOldPath != nil {
		t.Errorf("old path still resolves to a book: %+v", stillFindableAtOldPath)
	}
}

func TestUpdateBookFileStat(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Touched Book", FilePath: "/path.epub", Format: "epub", FileSize: 100}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	newMtime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := db.UpdateBookFileStat(ctx, id, 150, newMtime); err != nil {
		t.Fatalf("UpdateBookFileStat: %v", err)
	}

	updated, err := db.FindBookByPath(ctx, "/path.epub")
	if err != nil {
		t.Fatalf("FindBookByPath: %v", err)
	}
	if updated.FileSize != 150 || !updated.ModifiedAt.Equal(newMtime) {
		t.Errorf("updated book = %+v, unexpected fields", updated)
	}
}
