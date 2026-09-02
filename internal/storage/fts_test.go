package storage

import (
	"context"
	"testing"
	"time"
)

func TestSearchBooksFindsByEachIndexedField(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, Book{
		ContentHash: "hash-1",
		Title:       "Drive Your Plow Over the Bones of the Dead",
		SortTitle:   "Drive Your Plow Over the Bones of the Dead",
		Description: "A reclusive woman investigates a string of deaths",
		ISBN:        "9780857059985",
		Format:      "epub",
	}, []string{"Olga Tokarczuk"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	cases := []struct {
		name  string
		query string
	}{
		{"title token", "Plow"},
		{"title token, prefix", "Pl"},
		{"author token", "Tokarczuk"},
		{"author token, prefix", "Tokarcz"},
		{"description token", "reclusive"},
		{"description token, prefix", "reclus"},
		{"isbn, full", "9780857059985"},
		{"isbn, prefix", "978085705"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			books, err := db.SearchBooks(ctx, SanitizeFTSQuery(c.query))
			if err != nil {
				t.Fatalf("SearchBooks(%q): %v", c.query, err)
			}
			if len(books) != 1 || books[0].ID != id {
				t.Errorf("SearchBooks(%q) = %+v, want exactly the one book", c.query, books)
			}
		})
	}
}

func TestSearchBooksIsDiacriticInsensitive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	tokarczukID, err := db.CreateBook(ctx, Book{ContentHash: "hash-tok", Title: "Flights", SortTitle: "Flights", Format: "epub"}, []string{"Olga Tokarczuk"})
	if err != nil {
		t.Fatalf("CreateBook Tokarczuk: %v", err)
	}
	garciaID, err := db.CreateBook(ctx, Book{ContentHash: "hash-gar", Title: "One Hundred Years of Solitude", SortTitle: "One Hundred Years of Solitude", Format: "epub"}, []string{"Gabriel García Márquez"})
	if err != nil {
		t.Fatalf("CreateBook García: %v", err)
	}

	books, err := db.SearchBooks(ctx, SanitizeFTSQuery("tokarczúk"))
	if err != nil {
		t.Fatalf("SearchBooks accented query for unaccented stored name: %v", err)
	}
	if len(books) != 1 || books[0].ID != tokarczukID {
		t.Errorf("SearchBooks(tokarczúk) = %+v, want [%d]", books, tokarczukID)
	}

	books, err = db.SearchBooks(ctx, SanitizeFTSQuery("garcia"))
	if err != nil {
		t.Fatalf("SearchBooks unaccented query for accented stored name: %v", err)
	}
	if len(books) != 1 || books[0].ID != garciaID {
		t.Errorf("SearchBooks(garcia) = %+v, want [%d]", books, garciaID)
	}
}

func TestSearchBooksAndsTokensAcrossColumns(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Piranesi", SortTitle: "Piranesi", Format: "epub"}, []string{"Susanna Clarke"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	matching, err := db.SearchBooks(ctx, SanitizeFTSQuery("Piranesi Clarke"))
	if err != nil {
		t.Fatalf("SearchBooks title+author: %v", err)
	}
	if len(matching) != 1 || matching[0].ID != id {
		t.Errorf("SearchBooks(Piranesi Clarke) = %+v, want exactly the one book", matching)
	}

	notMatching, err := db.SearchBooks(ctx, SanitizeFTSQuery("Piranesi Tokarczuk"))
	if err != nil {
		t.Fatalf("SearchBooks title+wrong-author: %v", err)
	}
	if len(notMatching) != 0 {
		t.Errorf("SearchBooks(Piranesi Tokarczuk) = %+v, want no matches (tokens AND together)", notMatching)
	}
}

func TestDeletingBookViaOrphanPruneRemovesFTSRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	oldID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-old", Title: "Old Content", SortTitle: "Old Content", Format: "epub"},
		[]string{"Jane Doe"}, "/x.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile old: %v", err)
	}
	newID, err := db.CreateBook(ctx, Book{ContentHash: "hash-new", Title: "New Content", SortTitle: "New Content", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook new: %v", err)
	}

	assertFTSCount(t, db, 2)

	_, orphanedID, _, err := db.ReassignFileAndPruneOrphan(ctx, newID, "/x.epub", 200, mtime)
	if err != nil {
		t.Fatalf("ReassignFileAndPruneOrphan: %v", err)
	}
	if orphanedID != oldID {
		t.Fatalf("orphanedID = %d, want %d", orphanedID, oldID)
	}

	assertFTSCount(t, db, 1)

	books, err := db.SearchBooks(ctx, SanitizeFTSQuery("Old Content"))
	if err != nil {
		t.Fatalf("SearchBooks: %v", err)
	}
	if len(books) != 0 {
		t.Errorf("SearchBooks(Old Content) = %+v, want no matches — the orphaned book's FTS row should be gone", books)
	}
}

func TestDeletingBookViaPruneMissingFilesRemovesFTSRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Target Book", SortTitle: "Target Book", Format: "epub"}, nil, "target.epub", 100, mtime); err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	f, err := db.FindFileByPath(ctx, "target.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}

	assertFTSCount(t, db, 1)

	if err := db.SetFilesMissing(ctx, []int64{f.ID}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}
	files, books, err := db.PruneMissingFiles(ctx, []int64{f.ID})
	if err != nil {
		t.Fatalf("PruneMissingFiles: %v", err)
	}
	if files != 1 || books != 1 {
		t.Fatalf("PruneMissingFiles = files=%d books=%d, want 1, 1", files, books)
	}

	assertFTSCount(t, db, 0)
}

func assertFTSCount(t *testing.T, db *DB, want int) {
	t.Helper()
	var got int
	if err := db.Read().QueryRow(`SELECT count(*) FROM books_fts`).Scan(&got); err != nil {
		t.Fatalf("count books_fts: %v", err)
	}
	if got != want {
		t.Errorf("books_fts row count = %d, want %d", got, want)
	}
}
