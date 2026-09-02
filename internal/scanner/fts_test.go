package scanner

import (
	"context"
	"path/filepath"
	"testing"

	"library/internal/storage"
)

// A scanner-created book must be searchable without any extra step: the
// FTS sync runs inside the same composed write transaction CreateBookWithFile
// uses, so there is no window where a scanned book exists but isn't indexed.
func TestScanEnrollsBookIntoSearchIndex(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	writeTestEPUB(t, filepath.Join(libDir, "flights.epub"), "Flights", "Olga Tokarczuk", nil)

	result, err := Scan(ctx, db, libDir, coversDir, testMissingGrace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.New != 1 || result.Errors != 0 {
		t.Fatalf("scan = %+v, want New=1 Errors=0", result)
	}

	books, err := db.SearchBooks(ctx, storage.SanitizeFTSQuery("Tokarczuk"))
	if err != nil {
		t.Fatalf("SearchBooks: %v", err)
	}
	if len(books) != 1 || books[0].Title != "Flights" {
		t.Fatalf("SearchBooks(Tokarczuk) = %+v, want the scanned book", books)
	}
}
