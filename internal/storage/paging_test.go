package storage

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
)

// seedPagingBooks creates one book per sort_title given, in that order, and
// returns their ids keyed by sort_title. sort_title is set explicitly
// rather than derived, so a test can arrange the exact collisions and
// case mixtures the cursor has to survive.
func seedPagingBooks(t *testing.T, db *DB, sortTitles ...string) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	for i, st := range sortTitles {
		hash := "page-" + st + "-" + strconv.Itoa(i)
		id, err := db.CreateBook(context.Background(), Book{
			ContentHash: hash, Title: st, SortTitle: st, Format: "epub",
		}, nil)
		if err != nil {
			t.Fatalf("CreateBook %q: %v", st, err)
		}
		ids[st] = id
	}
	return ids
}

func titlesOf(books []Book) []string {
	out := make([]string, len(books))
	for i, b := range books {
		out[i] = b.SortTitle
	}
	return out
}

// walkAllPages pages through everything in limit-sized steps and returns
// every book seen, in order. It fails rather than looping forever if a
// page fails to advance, which is what a broken cursor actually does.
func walkAllPages(t *testing.T, db *DB, limit int) []Book {
	t.Helper()
	ctx := context.Background()
	var all []Book
	page := BookPage{Limit: limit}
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not terminate after 100 pages — the cursor is not advancing")
		}
		books, err := db.ListBooks(ctx, page)
		if err != nil {
			t.Fatalf("ListBooks: %v", err)
		}
		if len(books) == 0 {
			return all
		}
		all = append(all, books...)
		last := books[len(books)-1]
		page.AfterTitle, page.AfterID = last.SortTitle, last.ID
	}
}

func TestListBooksPageRespectsLimitAndCursor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedPagingBooks(t, db, "alpha", "bravo", "charlie", "delta", "echo")

	first, err := db.ListBooks(ctx, BookPage{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := titlesOf(first); len(got) != 2 || got[0] != "alpha" || got[1] != "bravo" {
		t.Fatalf("first page = %v, want [alpha bravo]", got)
	}

	last := first[len(first)-1]
	second, err := db.ListBooks(ctx, BookPage{AfterTitle: last.SortTitle, AfterID: last.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := titlesOf(second); len(got) != 2 || got[0] != "charlie" || got[1] != "delta" {
		t.Fatalf("second page = %v, want [charlie delta] — no overlap, no gap", got)
	}
}

// A Limit of zero is the whole-library call the scanner and most tests
// make, and it has to keep working untouched.
func TestListBooksUnboundedReturnsEverything(t *testing.T) {
	db := openTestDB(t)
	seedPagingBooks(t, db, "alpha", "bravo", "charlie")

	books, err := db.ListBooks(context.Background(), BookPage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 3 {
		t.Errorf("unbounded page returned %d books, want 3", len(books))
	}
}

// sort_title is COLLATE NOCASE, so the cursor comparison must be too. A
// case-sensitive comparison disagrees with the ORDER BY and pages then
// overlap or drop rows between them — the single most likely bug here.
func TestPagingRespectsNOCASECollation(t *testing.T) {
	db := openTestDB(t)
	// Interleaved deliberately: under a binary comparison every
	// upper-case title sorts before every lower-case one, so a
	// case-sensitive cursor and a NOCASE ORDER BY disagree about what
	// comes next.
	seedPagingBooks(t, db, "apple", "Banana", "cherry", "Date", "elder")

	all := walkAllPages(t, db, 2)
	got := titlesOf(all)
	want := []string{"apple", "Banana", "cherry", "Date", "elder"}
	if len(got) != len(want) {
		t.Fatalf("paged titles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paged titles = %v, want %v", got, want)
		}
	}
}

// sort_title is not unique — it is a normalised title, so two editions of
// one book collide by construction. A cursor on the title alone either
// loops on the collision or skips past it.
func TestPagingWithDuplicateSortTitles(t *testing.T) {
	db := openTestDB(t)
	seedPagingBooks(t, db, "alpha", "same", "same", "same", "omega")

	all := walkAllPages(t, db, 2)
	if len(all) != 5 {
		t.Fatalf("paged %d books, want 5 — a duplicate sort_title was skipped or repeated: %v", len(all), titlesOf(all))
	}

	seen := map[int64]int{}
	for _, b := range all {
		seen[b.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("book %d appeared %d times across pages, want exactly once", id, n)
		}
	}
}

// The case that rules out OFFSET. A book inserted above the reader's
// cursor between pages shifts every later row down by one under OFFSET, so
// the next page repeats a card; a keyset cursor names the last row seen
// and is unaffected.
func TestPagingIsUnaffectedByAnInsertAboveTheCursor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedPagingBooks(t, db, "bravo", "charlie", "delta", "echo")

	first, err := db.ListBooks(ctx, BookPage{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := titlesOf(first); got[0] != "bravo" || got[1] != "charlie" {
		t.Fatalf("first page = %v, want [bravo charlie]", got)
	}

	// The scanner inserts a book that sorts above everything already seen
	// — exactly what a 15-minute sweep does while someone is scrolling.
	seedPagingBooks(t, db, "alpha")

	last := first[len(first)-1]
	second, err := db.ListBooks(ctx, BookPage{AfterTitle: last.SortTitle, AfterID: last.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := titlesOf(second)
	if len(got) != 2 || got[0] != "delta" || got[1] != "echo" {
		t.Fatalf("second page = %v, want [delta echo] — an insert above the cursor must not repeat or skip a card", got)
	}
}

// A deletion above the cursor is the same argument in the other direction:
// under OFFSET it silently skips a row.
func TestPagingIsUnaffectedByADeleteAboveTheCursor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ids := seedPagingBooks(t, db, "alpha", "bravo", "charlie", "delta", "echo")

	first, err := db.ListBooks(ctx, BookPage{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	// No exported single-book delete exists — books die through the
	// scanner's pruning paths — and this test only needs the row gone.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, ids["alpha"])
		return err
	}); err != nil {
		t.Fatal(err)
	}

	last := first[len(first)-1]
	second, err := db.ListBooks(ctx, BookPage{AfterTitle: last.SortTitle, AfterID: last.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := titlesOf(second); len(got) != 2 || got[0] != "charlie" || got[1] != "delta" {
		t.Fatalf("second page = %v, want [charlie delta]", got)
	}
}

// One cursor serves both list paths, which is only possible because the
// search orders by sort_title rather than relevance.
func TestSearchBooksPages(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedPagingBooks(t, db, "alpha novel", "bravo novel", "charlie novel", "delta other")

	query := SanitizeFTSQuery("novel")
	first, err := db.SearchBooks(ctx, query, BookPage{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := titlesOf(first); len(got) != 2 || got[0] != "alpha novel" || got[1] != "bravo novel" {
		t.Fatalf("first search page = %v, want the first two novels", got)
	}

	last := first[len(first)-1]
	second, err := db.SearchBooks(ctx, query, BookPage{AfterTitle: last.SortTitle, AfterID: last.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := titlesOf(second); len(got) != 1 || got[0] != "charlie novel" {
		t.Fatalf("second search page = %v, want just [charlie novel] — the non-matching book must stay filtered out", got)
	}
}

func TestCountSearchBooksIsIndependentOfThePage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedPagingBooks(t, db, "alpha novel", "bravo novel", "charlie novel", "delta other")

	query := SanitizeFTSQuery("novel")
	n, err := db.CountSearchBooks(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("CountSearchBooks = %d, want 3 — the match total, not one page of it", n)
	}
}
