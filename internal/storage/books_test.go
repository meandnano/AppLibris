package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
		CoverRetry:  true,
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
	if !byHash.CoverRetry {
		t.Error("CoverRetry = false, want true before cover update")
	}

	if err := db.UpdateBookCoverPath(ctx, id, "/covers/hash-1.jpg"); err != nil {
		t.Fatalf("UpdateBookCoverPath: %v", err)
	}
	byHash, err = db.FindBookByContentHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindBookByContentHash after cover update: %v", err)
	}
	if byHash == nil || byHash.CoverPath != "/covers/hash-1.jpg" || byHash.CoverRetry {
		t.Errorf("book after cover update = %+v, want path set and retry cleared", byHash)
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
	if byPath.BookContentHash != "hash-1" || byPath.BookCoverPath != "/covers/hash-1.jpg" || byPath.BookCoverRetry {
		t.Errorf("FindFileByPath book cover fields = %+v, unexpected values", byPath)
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

func TestCountBooks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if n, err := db.CountBooks(ctx); err != nil || n != 0 {
		t.Fatalf("CountBooks on an empty library = %d, %v; want 0, nil", n, err)
	}

	for _, hash := range []string{"hash-a", "hash-b", "hash-c"} {
		if _, err := db.CreateBook(ctx, Book{ContentHash: hash, Title: hash, SortTitle: hash}, nil); err != nil {
			t.Fatalf("CreateBook %s: %v", hash, err)
		}
	}

	if n, err := db.CountBooks(ctx); err != nil || n != 3 {
		t.Errorf("CountBooks = %d, %v; want 3, nil", n, err)
	}
}

func TestFindBookByID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Example", SortTitle: "Example", Format: "epub"}, []string{"Jane Doe"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	got, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatalf("FindBookByID: %v", err)
	}
	if got == nil || got.ID != id || got.Title != "Example" {
		t.Errorf("FindBookByID(%d) = %+v, want the created book", id, got)
	}

	unknown, err := db.FindBookByID(ctx, id+1000)
	if err != nil {
		t.Fatalf("FindBookByID unknown id: %v", err)
	}
	if unknown != nil {
		t.Errorf("FindBookByID(unknown) = %+v, want nil", unknown)
	}
}

func TestListAuthorsForBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	multiID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Multi", SortTitle: "Multi"}, []string{"Zed Zorro", "Ann Alpha"})
	if err != nil {
		t.Fatalf("CreateBook multi: %v", err)
	}
	noneID, err := db.CreateBook(ctx, Book{ContentHash: "hash-2", Title: "None", SortTitle: "None"}, nil)
	if err != nil {
		t.Fatalf("CreateBook none: %v", err)
	}

	names, err := db.ListAuthorsForBook(ctx, multiID)
	if err != nil {
		t.Fatalf("ListAuthorsForBook multi: %v", err)
	}
	if want := []string{"Zed Zorro", "Ann Alpha"}; !slices.Equal(names, want) {
		t.Errorf("ListAuthorsForBook(multi) = %v, want %v (source order, not alphabetical)", names, want)
	}

	names, err = db.ListAuthorsForBook(ctx, noneID)
	if err != nil {
		t.Fatalf("ListAuthorsForBook none: %v", err)
	}
	if names == nil || len(names) != 0 {
		t.Errorf("ListAuthorsForBook(authorless) = %v (nil=%v), want empty non-nil slice", names, names == nil)
	}
}

func TestListBookFiles(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookAID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-a", Title: "Book A", SortTitle: "Book A"}, nil, "b/second.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile A: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, bookAID, "a/first.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile A second location: %v", err)
	}
	bookBID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-b", Title: "Book B", SortTitle: "Book B"}, nil, "other.epub", 200, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile B: %v", err)
	}

	files, err := db.ListBookFiles(ctx, bookAID)
	if err != nil {
		t.Fatalf("ListBookFiles A: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("ListBookFiles(A) returned %d files, want 2", len(files))
	}
	if files[0].FilePath != "a/first.epub" || files[1].FilePath != "b/second.epub" {
		t.Errorf("ListBookFiles(A) paths = [%q, %q], want ordered by path", files[0].FilePath, files[1].FilePath)
	}
	for _, f := range files {
		if f.BookID != bookAID {
			t.Errorf("ListBookFiles(A) leaked a file from another book: %+v", f)
		}
	}

	filesB, err := db.ListBookFiles(ctx, bookBID)
	if err != nil {
		t.Fatalf("ListBookFiles B: %v", err)
	}
	if len(filesB) != 1 || filesB[0].FilePath != "other.epub" {
		t.Errorf("ListBookFiles(B) = %+v, want exactly [other.epub]", filesB)
	}

	if err := db.SetFilesMissing(ctx, []int64{filesB[0].ID}, mtime); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}
	filesB, err = db.ListBookFiles(ctx, bookBID)
	if err != nil {
		t.Fatalf("ListBookFiles B after marking missing: %v", err)
	}
	if !filesB[0].MissingSince.Valid {
		t.Error("ListBookFiles(B) after SetFilesMissing: MissingSince not surfaced")
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
	if got := authors[multiID]; len(got) != 2 || got[0] != "Jane Doe" || got[1] != "John Roe" {
		t.Errorf("authors[multi] = %v, want [Jane Doe John Roe] in that order", got)
	}
	if got := authors[noneID]; got != nil {
		t.Errorf("authors[none] = %v, want no entry", got)
	}
}

// The regression this step exists for. author_id order is first-sight-in-
// the-library order: Terry Pratchett is created via book one, so under the
// old author_id-ordered query this returned [Terry Pratchett, Neil Gaiman]
// for book two — the wrong lead author — no matter what book two's own
// file credited them as. This fails on master today; it must not.
func TestListBookAuthorsUsesEachBooksOwnOrderNotAuthorID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Book One"}, []string{"Terry Pratchett"}); err != nil {
		t.Fatalf("CreateBook 1: %v", err)
	}
	book2ID, err := db.CreateBook(ctx, Book{ContentHash: "hash-2", Title: "Book Two"}, []string{"Neil Gaiman", "Terry Pratchett"})
	if err != nil {
		t.Fatalf("CreateBook 2: %v", err)
	}

	authors, err := db.ListBookAuthors(ctx)
	if err != nil {
		t.Fatalf("ListBookAuthors: %v", err)
	}
	if got := authors[book2ID]; len(got) != 2 || got[0] != "Neil Gaiman" || got[1] != "Terry Pratchett" {
		t.Errorf("authors[book2] = %v, want [Neil Gaiman Terry Pratchett] — book two's own credited order, not author_id order", got)
	}
}

func TestCountFilesByBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

	oneID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-one", Title: "One Location", Format: "epub"}, nil, "/one.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile one: %v", err)
	}

	twoID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-two", Title: "Two Locations", Format: "epub"}, nil, "/two-a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile two: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, twoID, "/two-b.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile two-b: %v", err)
	}

	threeID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-three", Title: "Three Locations", Format: "epub"}, nil, "/three-a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile three: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, threeID, "/three-b.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile three-b: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, threeID, "/three-c.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile three-c: %v", err)
	}

	counts, err := db.CountFilesByBook(ctx)
	if err != nil {
		t.Fatalf("CountFilesByBook: %v", err)
	}
	if counts[oneID] != 1 {
		t.Errorf("counts[one] = %d, want 1", counts[oneID])
	}
	if counts[twoID] != 2 {
		t.Errorf("counts[two] = %d, want 2", counts[twoID])
	}
	if counts[threeID] != 3 {
		t.Errorf("counts[three] = %d, want 3", counts[threeID])
	}
}

// The decision CLAUDE.md and the plan pin: a missing location still counts.
// A book_files row stays until it has been missing past MISSING_GRACE, and
// the detail page lists it (annotated) for that whole window, so the grid's
// count must agree rather than dropping the marker early.
func TestCountFilesByBookCountsMissingLocations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "/x.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	f, err := db.FindFileByPath(ctx, "/x.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, mtime); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}

	counts, err := db.CountFilesByBook(ctx)
	if err != nil {
		t.Fatalf("CountFilesByBook: %v", err)
	}
	if counts[bookID] != 1 {
		t.Errorf("counts[book] = %d, want 1 (a missing row is still a row)", counts[bookID])
	}
}

func TestCountFilesByBookOmitsDeletedBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "/x.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, bookID)
		return err
	}); err != nil {
		t.Fatalf("delete book: %v", err)
	}

	counts, err := db.CountFilesByBook(ctx)
	if err != nil {
		t.Fatalf("CountFilesByBook: %v", err)
	}
	if _, ok := counts[bookID]; ok {
		t.Errorf("counts[book] present after delete = %d, want no entry", counts[bookID])
	}
}

func TestCountFilesByBookEmptyLibrary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	counts, err := db.CountFilesByBook(ctx)
	if err != nil {
		t.Fatalf("CountFilesByBook: %v", err)
	}
	if counts == nil {
		t.Error("counts = nil, want empty non-nil map")
	}
	if len(counts) != 0 {
		t.Errorf("counts = %v, want empty", counts)
	}
}

func TestBookAuthorsPositionIsZeroBasedAndContiguous(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Three Authors"}, []string{"Author A", "Author B", "Author C"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	rows, err := db.Read().QueryContext(ctx, `SELECT position FROM book_authors WHERE book_id = ? ORDER BY position`, bookID)
	if err != nil {
		t.Fatalf("query positions: %v", err)
	}
	defer rows.Close()
	var positions []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		positions = append(positions, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if want := []int{0, 1, 2}; !slices.Equal(positions, want) {
		t.Errorf("positions = %v, want %v", positions, want)
	}
}

// The second defect found while validating this step: a name credited
// twice in one file used to fail the whole CreateBook transaction —
// book_authors' primary key is (book_id, author_id), so the second insert
// for the same author violated it — and silently drop the book from the
// library with nothing in the UI to say why. It must now link once, at its
// first position, not error and not lose the book.
func TestCreateBookDedupesRepeatedAuthorName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Repeated Credit"}, []string{"Adam Author", "Adam Author"})
	if err != nil {
		t.Fatalf("CreateBook with a repeated author name: %v", err)
	}

	books, err := db.ListBooks(ctx)
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("ListBooks returned %d books, want 1", len(books))
	}

	authors, err := db.ListBookAuthors(ctx)
	if err != nil {
		t.Fatalf("ListBookAuthors: %v", err)
	}
	if got := authors[bookID]; len(got) != 1 || got[0] != "Adam Author" {
		t.Errorf("authors[book] = %v, want exactly one link to Adam Author", got)
	}
}

// ListBooks orders by sort_title, which is declared COLLATE NOCASE so that a
// lowercase title files among its own letter rather than after every
// capitalised one. Under the default BINARY collation this returns
// [Anna Karenina, Zebra Book, apple book], because 'Z' (0x5A) < 'a' (0x61).
func TestListBooksOrdersCaseInsensitively(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Inserted in an order that matches neither the wanted nor the binary one.
	for i, sortTitle := range []string{"zebra book", "anna karenina", "Apple Book"} {
		if _, err := db.CreateBook(ctx, Book{
			ContentHash: fmt.Sprintf("hash-%d", i),
			Title:       sortTitle,
			SortTitle:   sortTitle,
		}, nil); err != nil {
			t.Fatalf("CreateBook %q: %v", sortTitle, err)
		}
	}

	books, err := db.ListBooks(ctx)
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}

	var got []string
	for _, b := range books {
		got = append(got, b.SortTitle)
	}
	want := []string{"anna karenina", "Apple Book", "zebra book"}
	if !slices.Equal(got, want) {
		t.Errorf("ListBooks order = %v, want %v", got, want)
	}
}

// modified_at used to be bound as a raw time.Time, which the driver rendered
// as "2026-08-31 12:34:56.123456789 +0200 CEST" — a shape SQLite's own date
// functions can't parse. This asserts the raw stored text, not just the
// round-trip: the round-trip passes even with the broken format, which is
// why it went unnoticed.
func TestBookFileTimestampsAreSQLiteReadable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Timestamp Book", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	mtime := time.Date(2026, 8, 31, 12, 34, 56, 123456789, time.FixedZone("CEST", 2*60*60))
	fileID, err := db.UpsertBookFile(ctx, bookID, "/path.epub", 100, mtime)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	f, err := db.FindFileByPath(ctx, "/path.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	if !f.ModifiedAt.Equal(mtime) {
		t.Errorf("ModifiedAt round-trip = %v, want %v", f.ModifiedAt, mtime)
	}

	var raw string
	if err := db.Read().QueryRowContext(ctx, `SELECT CAST(modified_at AS TEXT) FROM book_files WHERE id = ?`, fileID).Scan(&raw); err != nil {
		t.Fatalf("read raw modified_at: %v", err)
	}
	if want := formatTime(mtime); raw != want {
		t.Errorf("stored modified_at = %q, want %q (sqliteTimeLayout, in UTC)", raw, want)
	}

	var modifiedReadable, addedReadable sql.NullString
	err = db.Read().QueryRowContext(ctx,
		`SELECT datetime(modified_at), datetime(added_at) FROM book_files WHERE id = ?`, fileID).
		Scan(&modifiedReadable, &addedReadable)
	if err != nil {
		t.Fatalf("datetime() query: %v", err)
	}
	if !modifiedReadable.Valid {
		t.Error("datetime(modified_at) is NULL, want a value")
	}
	if !addedReadable.Valid {
		t.Error("datetime(added_at) is NULL, want a value (book_files.added_at default)")
	}
}

func TestDeletingBookCascadesFilesAndAuthorLinkButKeepsAuthor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bookID, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Cascade Book", Format: "epub"}, []string{"Jane Doe"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, bookID, "/path.epub", 100, time.Now()); err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	// No DeleteBook method exists yet (out of scope for this step); raw SQL
	// is the only way to exercise the cascade the schema now provides.
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, bookID)
		return err
	}); err != nil {
		t.Fatalf("delete book: %v", err)
	}

	var fileCount, joinCount, authorCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM book_files WHERE book_id = ?`, bookID).Scan(&fileCount); err != nil {
		t.Fatalf("count book_files: %v", err)
	}
	if fileCount != 0 {
		t.Errorf("book_files rows after delete = %d, want 0", fileCount)
	}

	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM book_authors WHERE book_id = ?`, bookID).Scan(&joinCount); err != nil {
		t.Fatalf("count book_authors: %v", err)
	}
	if joinCount != 0 {
		t.Errorf("book_authors rows after delete = %d, want 0", joinCount)
	}

	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM authors WHERE name = ?`, "Jane Doe").Scan(&authorCount); err != nil {
		t.Fatalf("count authors: %v", err)
	}
	if authorCount != 1 {
		t.Errorf("authors row for %q after book delete = %d, want 1 (the author itself must survive)", "Jane Doe", authorCount)
	}
}

func TestAuthorNameIsUnique(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateBook(ctx, Book{ContentHash: "hash-1", Title: "Book One"}, []string{"Shared Author"}); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	// Bypass findOrCreateAuthor's own SELECT-then-INSERT dedup, to prove the
	// database-level constraint is what actually stops a duplicate.
	err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO authors (name) VALUES (?)`, "Shared Author")
		return err
	})
	if err == nil {
		t.Fatal("inserting a duplicate author name: want an error, got nil")
	}
}

// The …Tx helpers exist so a caller can compose more than one write inside
// a single DB.Write, atomically. This is what composing two of them,
// successfully, actually looks like.
func TestComposedWritesCommitTogether(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	var bookID int64
	err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err := createBookTx(ctx, tx, Book{ContentHash: "hash-1", Title: "Composed Book", Format: "epub"}, nil)
		if err != nil {
			return err
		}
		bookID = id
		_, err = upsertBookFileTx(ctx, tx, id, "/path.epub", 100, mtime)
		return err
	})
	if err != nil {
		t.Fatalf("db.Write: %v", err)
	}

	book, err := db.FindBookByContentHash(ctx, "hash-1")
	if err != nil || book == nil || book.ID != bookID {
		t.Fatalf("FindBookByContentHash = %+v, %v; want book %d", book, err, bookID)
	}
	file, err := db.FindFileByPath(ctx, "/path.epub")
	if err != nil || file == nil || file.BookID != bookID {
		t.Fatalf("FindFileByPath = %+v, %v; want book %d", file, err, bookID)
	}
}

// The property the whole step is for: a later failure in the same callback
// must undo an earlier successful …Tx call too, not just its own statement.
// Before this step there was no way for two writes to even share a
// transaction, so this case could not previously exist, let alone be tested.
func TestComposedWritesRollBackTogether(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	sentinel := errors.New("boom")
	err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err := createBookTx(ctx, tx, Book{ContentHash: "hash-1", Title: "Rolled Back Book", Format: "epub"}, nil)
		if err != nil {
			return err
		}
		if _, err := upsertBookFileTx(ctx, tx, id, "/path.epub", 100, mtime); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("db.Write error = %v, want %v", err, sentinel)
	}

	book, err := db.FindBookByContentHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindBookByContentHash: %v", err)
	}
	if book != nil {
		t.Errorf("FindBookByContentHash = %+v, want nil (rolled back)", book)
	}
	file, err := db.FindFileByPath(ctx, "/path.epub")
	if err != nil {
		t.Fatalf("FindFileByPath: %v", err)
	}
	if file != nil {
		t.Errorf("FindFileByPath = %+v, want nil (rolled back)", file)
	}
}

func TestCreateBookWithFile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Atomic Book", Format: "epub"},
		[]string{"Jane Doe"}, "/path.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	book, err := db.FindBookByContentHash(ctx, "hash-1")
	if err != nil || book == nil || book.ID != bookID {
		t.Fatalf("FindBookByContentHash = %+v, %v; want book %d", book, err, bookID)
	}
	file, err := db.FindFileByPath(ctx, "/path.epub")
	if err != nil || file == nil || file.BookID != bookID {
		t.Fatalf("FindFileByPath = %+v, %v; want book %d", file, err, bookID)
	}
}

// A failure anywhere inside CreateBookWithFile's single transaction must
// leave no books row behind, not just return an error. book_files' only
// real per-row constraint (file_path uniqueness) is absorbed by its own
// ON CONFLICT DO UPDATE, so it can never be the thing that fails; a
// duplicate author name is: both occurrences resolve to the same author
// id, so the second book_authors insert hits the (book_id, author_id)
// primary key. That happens inside createBookTx, before the file insert
// even runs — which is itself part of what this proves: the whole
// operation is one transaction, not "insert the book, then maybe insert
// the file."
// A duplicate author name in authorNames is deduplicated, not an error (see
// TestCreateBookDedupesRepeatedAuthorName) — so this test's failure
// trigger is a duplicate content_hash instead, which still fails the
// transaction's very first statement (the UNIQUE index on
// books.content_hash) and proves the same thing: a failure anywhere in
// CreateBookWithFile's transaction leaves no partial book row behind.
func TestCreateBookWithFileIsAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "First", Format: "epub"},
		nil, "/first.epub", 100, mtime); err != nil {
		t.Fatalf("CreateBookWithFile first: %v", err)
	}

	_, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Should Not Exist", Format: "epub"},
		nil, "/second.epub", 100, mtime)
	if err == nil {
		t.Fatal("CreateBookWithFile with a duplicate content_hash: want an error, got nil")
	}

	var count int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE title = ?`, "Should Not Exist").Scan(&count); err != nil {
		t.Fatalf("count books: %v", err)
	}
	if count != 0 {
		t.Errorf("books row survived a failed CreateBookWithFile call; want 0 rows, got %d", count)
	}
}

func TestReassignFileAndPruneOrphanDeletesSingleLocationOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	oldID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-old", Title: "Old Content", Format: "epub"},
		[]string{"Jane Doe"}, "/x.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile old: %v", err)
	}
	newID, err := db.CreateBook(ctx, Book{ContentHash: "hash-new", Title: "New Content", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook new: %v", err)
	}

	fileID, orphanedID, orphanedTitle, err := db.ReassignFileAndPruneOrphan(ctx, newID, "/x.epub", 200, mtime)
	if err != nil {
		t.Fatalf("ReassignFileAndPruneOrphan: %v", err)
	}
	if fileID == 0 {
		t.Error("fileID = 0, want a nonzero book_files id")
	}
	if orphanedID != oldID {
		t.Errorf("orphanedID = %d, want %d", orphanedID, oldID)
	}
	if orphanedTitle != "Old Content" {
		t.Errorf("orphanedTitle = %q, want %q", orphanedTitle, "Old Content")
	}

	f, err := db.FindFileByPath(ctx, "/x.epub")
	if err != nil || f == nil || f.BookID != newID {
		t.Errorf("FindFileByPath after reassign = %+v, %v; want book %d", f, err, newID)
	}

	var oldCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE id = ?`, oldID).Scan(&oldCount); err != nil {
		t.Fatalf("count old book: %v", err)
	}
	if oldCount != 0 {
		t.Errorf("old book row survived orphaning; want 0, got %d", oldCount)
	}
}

// The case a before-upsert count gets wrong: a book with two locations
// loses one to a reassignment and must survive, still owning the other.
func TestReassignFileAndPruneOrphanKeepsMultiLocationOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	oldID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-old", Title: "Duplicated Content", Format: "epub"}, nil,
		"/a/copy.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, oldID, "/b/copy.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile second location: %v", err)
	}
	newID, err := db.CreateBook(ctx, Book{ContentHash: "hash-new", Title: "New Content", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook new: %v", err)
	}

	_, orphanedID, _, err := db.ReassignFileAndPruneOrphan(ctx, newID, "/a/copy.epub", 200, mtime)
	if err != nil {
		t.Fatalf("ReassignFileAndPruneOrphan: %v", err)
	}
	if orphanedID != 0 {
		t.Errorf("orphanedID = %d, want 0 (book still owns /b/copy.epub)", orphanedID)
	}

	var oldCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE id = ?`, oldID).Scan(&oldCount); err != nil {
		t.Fatalf("count old book: %v", err)
	}
	if oldCount != 1 {
		t.Errorf("old book row = %d rows, want 1 (must survive)", oldCount)
	}

	remaining, err := db.FindFileByPath(ctx, "/b/copy.epub")
	if err != nil || remaining == nil || remaining.BookID != oldID {
		t.Errorf("FindFileByPath /b/copy.epub = %+v, %v; want it still owned by %d", remaining, err, oldID)
	}
}

func TestReassignFileAndPruneOrphanNoopOnUnchangedOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Touched Book", Format: "epub"}, nil,
		"/path.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	newMtime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, orphanedID, _, err := db.ReassignFileAndPruneOrphan(ctx, bookID, "/path.epub", 150, newMtime)
	if err != nil {
		t.Fatalf("ReassignFileAndPruneOrphan: %v", err)
	}
	if orphanedID != 0 {
		t.Errorf("orphanedID = %d, want 0 (same owner, just a stat refresh)", orphanedID)
	}

	var count int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE id = ?`, bookID).Scan(&count); err != nil {
		t.Fatalf("count book: %v", err)
	}
	if count != 1 {
		t.Errorf("book row = %d rows, want 1", count)
	}
}

func TestReassignFileAndPruneOrphanCascadesAuthorLinkButKeepsAuthor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	oldID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-old", Title: "Old Content", Format: "epub"},
		[]string{"Jane Doe"}, "/x.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	newID, err := db.CreateBook(ctx, Book{ContentHash: "hash-new", Title: "New Content", Format: "epub"}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	if _, _, _, err := db.ReassignFileAndPruneOrphan(ctx, newID, "/x.epub", 200, mtime); err != nil {
		t.Fatalf("ReassignFileAndPruneOrphan: %v", err)
	}

	var joinCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM book_authors WHERE book_id = ?`, oldID).Scan(&joinCount); err != nil {
		t.Fatalf("count book_authors: %v", err)
	}
	if joinCount != 0 {
		t.Errorf("book_authors rows for orphaned book = %d, want 0", joinCount)
	}

	var authorCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM authors WHERE name = ?`, "Jane Doe").Scan(&authorCount); err != nil {
		t.Fatalf("count authors: %v", err)
	}
	if authorCount != 1 {
		t.Errorf("authors row for %q after orphaning = %d, want 1 (the author itself must survive)", "Jane Doe", authorCount)
	}
}

func TestSetFilesMissingDoesNotRestartAnAlreadyMissingRowsTimer(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "/x.epub", 100, mtime); err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	f, err := db.FindFileByPath(ctx, "/x.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}

	first := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, first); err != nil {
		t.Fatalf("SetFilesMissing first: %v", err)
	}

	later := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, later); err != nil {
		t.Fatalf("SetFilesMissing second: %v", err)
	}

	updated, err := db.FindFileByPath(ctx, "/x.epub")
	if err != nil || updated == nil {
		t.Fatalf("FindFileByPath: %+v, %v", updated, err)
	}
	if !updated.MissingSince.Valid {
		t.Fatal("MissingSince = invalid, want set")
	}
	if !updated.MissingSince.Time.Equal(first) {
		t.Errorf("MissingSince = %v, want the first mark %v, not the later one", updated.MissingSince.Time, first)
	}
}

func TestClearFilesMissing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "/x.epub", 100, mtime); err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	f, err := db.FindFileByPath(ctx, "/x.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}

	if err := db.SetFilesMissing(ctx, []int64{f.ID}, time.Now()); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}
	if err := db.ClearFilesMissing(ctx, []int64{f.ID}); err != nil {
		t.Fatalf("ClearFilesMissing: %v", err)
	}

	cleared, err := db.FindFileByPath(ctx, "/x.epub")
	if err != nil || cleared == nil {
		t.Fatalf("FindFileByPath: %+v, %v", cleared, err)
	}
	if cleared.MissingSince.Valid {
		t.Errorf("MissingSince = %v, want cleared", cleared.MissingSince)
	}
}

func TestListFilesUnder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-root", Title: "Root Book", Format: "epub"}, nil, "root.epub", 100, mtime); err != nil {
		t.Fatalf("CreateBookWithFile root: %v", err)
	}
	if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-sub", Title: "Sub Book", Format: "epub"}, nil, "sub/book.epub", 100, mtime); err != nil {
		t.Fatalf("CreateBookWithFile sub: %v", err)
	}
	if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-sibling", Title: "Sibling Book", Format: "epub"}, nil, "subsequent.epub", 100, mtime); err != nil {
		t.Fatalf("CreateBookWithFile sibling: %v", err)
	}

	all, err := db.ListFilesUnder(ctx, "")
	if err != nil {
		t.Fatalf("ListFilesUnder(\"\"): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListFilesUnder(\"\") = %d rows, want 3", len(all))
	}

	underSub, err := db.ListFilesUnder(ctx, "sub")
	if err != nil {
		t.Fatalf("ListFilesUnder(sub): %v", err)
	}
	if len(underSub) != 1 || underSub[0].FilePath != "sub/book.epub" {
		t.Errorf("ListFilesUnder(sub) = %+v, want just sub/book.epub (not the sibling %q which merely shares a string prefix)", underSub, "subsequent.epub")
	}
}

// PruneMissingFiles does no filtering of its own — the caller (reconcile-
// Missing, in internal/scanner) is the one that decides what's eligible,
// via age and a current Lstat reconfirmation. This test only pins the
// mechanical contract: it deletes exactly the rows named, and nothing else,
// regardless of how long other rows have been marked.
func TestPruneMissingFilesDeletesOnlyTheGivenIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	setup := func(path, hash string) int64 {
		if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: hash, Title: path, Format: "epub"}, nil, path, 100, mtime); err != nil {
			t.Fatalf("CreateBookWithFile %s: %v", path, err)
		}
		f, err := db.FindFileByPath(ctx, path)
		if err != nil || f == nil {
			t.Fatalf("FindFileByPath %s: %+v, %v", path, f, err)
		}
		return f.ID
	}

	targetID := setup("target.epub", "hash-target")
	untouchedID := setup("untouched.epub", "hash-untouched")

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := db.SetFilesMissing(ctx, []int64{targetID, untouchedID}, old); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}

	files, books, err := db.PruneMissingFiles(ctx, []int64{targetID})
	if err != nil {
		t.Fatalf("PruneMissingFiles: %v", err)
	}
	if files != 1 || books != 1 {
		t.Errorf("PruneMissingFiles = files=%d books=%d, want files=1 books=1 (only target.epub)", files, books)
	}

	if f, err := db.FindFileByPath(ctx, "target.epub"); err != nil || f != nil {
		t.Errorf("target.epub survived pruning: %+v, %v", f, err)
	}
	// untouched.epub is just as overdue as target.epub was, but wasn't
	// named — PruneMissingFiles must not decide on its own that it also
	// qualifies.
	if f, err := db.FindFileByPath(ctx, "untouched.epub"); err != nil || f == nil {
		t.Errorf("untouched.epub was pruned despite not being in the given ID list: %+v, %v", f, err)
	}
}

func TestPruneMissingFilesDeletesBookOnlyWhenLastLocationGoes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Two Locations", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if _, err := db.UpsertBookFile(ctx, bookID, "b.epub", 100, mtime); err != nil {
		t.Fatalf("UpsertBookFile second location: %v", err)
	}

	fileA, err := db.FindFileByPath(ctx, "a.epub")
	if err != nil || fileA == nil {
		t.Fatalf("FindFileByPath a.epub: %+v, %v", fileA, err)
	}
	fileB, err := db.FindFileByPath(ctx, "b.epub")
	if err != nil || fileB == nil {
		t.Fatalf("FindFileByPath b.epub: %+v, %v", fileB, err)
	}

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Losing one of two locations must not delete the book.
	if err := db.SetFilesMissing(ctx, []int64{fileA.ID}, old); err != nil {
		t.Fatalf("SetFilesMissing a: %v", err)
	}
	files, books, err := db.PruneMissingFiles(ctx, []int64{fileA.ID})
	if err != nil {
		t.Fatalf("PruneMissingFiles (one of two): %v", err)
	}
	if files != 1 || books != 0 {
		t.Errorf("PruneMissingFiles (one of two) = files=%d books=%d, want files=1 books=0", files, books)
	}
	var bookCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE id = ?`, bookID).Scan(&bookCount); err != nil {
		t.Fatalf("count book: %v", err)
	}
	if bookCount != 1 {
		t.Errorf("book survived losing one of two locations = %d rows, want 1", bookCount)
	}

	// Losing the last remaining location must delete the book too.
	if err := db.SetFilesMissing(ctx, []int64{fileB.ID}, old); err != nil {
		t.Fatalf("SetFilesMissing b: %v", err)
	}
	files, books, err = db.PruneMissingFiles(ctx, []int64{fileB.ID})
	if err != nil {
		t.Fatalf("PruneMissingFiles (last location): %v", err)
	}
	if files != 1 || books != 1 {
		t.Errorf("PruneMissingFiles (last location) = files=%d books=%d, want files=1 books=1", files, books)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE id = ?`, bookID).Scan(&bookCount); err != nil {
		t.Fatalf("count book: %v", err)
	}
	if bookCount != 0 {
		t.Errorf("book survived losing its last location = %d rows, want 0", bookCount)
	}
}

// A single DELETE ... IN (...) statement can't take an unbounded number of
// bound parameters (SQLite's SQLITE_MAX_VARIABLE_NUMBER), so a large overdue
// batch must be chunked internally rather than handed to SQL as one list.
// This exercises well past pruneMissingFilesChunkSize to prove chunking
// doesn't drop or double-count rows at a chunk boundary.
func TestPruneMissingFilesHandlesMoreIDsThanOneSQLChunk(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const total = pruneMissingFilesChunkSize + 200
	var fileIDs []int64
	for i := 0; i < total; i++ {
		path := fmt.Sprintf("book-%d.epub", i)
		if _, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: fmt.Sprintf("hash-%d", i), Title: path, Format: "epub"}, nil, path, 100, mtime); err != nil {
			t.Fatalf("CreateBookWithFile %s: %v", path, err)
		}
		f, err := db.FindFileByPath(ctx, path)
		if err != nil || f == nil {
			t.Fatalf("FindFileByPath %s: %+v, %v", path, f, err)
		}
		fileIDs = append(fileIDs, f.ID)
	}
	if err := db.SetFilesMissing(ctx, fileIDs, old); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}

	files, books, err := db.PruneMissingFiles(ctx, fileIDs)
	if err != nil {
		t.Fatalf("PruneMissingFiles: %v", err)
	}
	if files != total || books != total {
		t.Errorf("PruneMissingFiles = files=%d books=%d, want files=%d books=%d", files, books, total, total)
	}

	var remaining int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM book_files`).Scan(&remaining); err != nil {
		t.Fatalf("count book_files: %v", err)
	}
	if remaining != 0 {
		t.Errorf("book_files rows remaining = %d, want 0", remaining)
	}
}
