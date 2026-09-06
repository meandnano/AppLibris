package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateBookRecordsEmbeddedFieldSources(t *testing.T) {
	db := openTestDB(t)
	id, err := db.CreateBook(context.Background(), Book{
		ContentHash: "metadata-source", Title: "The Book", Publisher: "Press", Description: "Text",
	}, []string{"First Author"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	rows, err := db.Read().Query(`SELECT field, source FROM field_sources WHERE book_id = ? ORDER BY field`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var field, source string
		if err := rows.Scan(&field, &source); err != nil {
			t.Fatal(err)
		}
		got[field] = source
	}
	for _, field := range []string{"title", "publisher", "description", "authors"} {
		if got[field] != "embedded" {
			t.Errorf("source for %s = %q, want embedded", field, got[field])
		}
	}
	if _, exists := got["isbn"]; exists {
		t.Error("empty ISBN received provenance")
	}
}

func TestUpdateBookMetadataIsAtomicWithFTSAndProvenance(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "editable", Title: "The Old Title", SortTitle: "old title", Description: "old description"}, []string{"Old Author"})
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if exists, err := db.UpdateBookField(ctx, id, FieldTitle, "An Updated Title", when); err != nil || !exists {
		t.Fatalf("UpdateBookField = %v, %v", exists, err)
	}
	if exists, err := db.UpdateBookAuthors(ctx, id, []string{"New Author", "Second Author"}, when); err != nil || !exists {
		t.Fatalf("UpdateBookAuthors = %v, %v", exists, err)
	}
	if exists, err := db.UpdateBookField(ctx, id, FieldDescription, "", when); err != nil || !exists {
		t.Fatalf("clear description = %v, %v", exists, err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Title != "An Updated Title" || book.SortTitle != "updated title" || book.Description != "" {
		t.Errorf("updated book = %#v", book)
	}
	if !book.ModifiedAt.Equal(when) {
		t.Errorf("ModifiedAt = %v, want %v — an edit has to stamp the book it changed", book.ModifiedAt, when)
	}
	authors, err := db.ListAuthorsForBook(ctx, id)
	if err != nil || len(authors) != 2 || authors[0] != "New Author" || authors[1] != "Second Author" {
		t.Errorf("authors = %v, %v", authors, err)
	}
	for _, field := range []MetadataField{FieldTitle, FieldAuthors, FieldDescription} {
		var source string
		if err := db.Read().QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, field).Scan(&source); err != nil || source != "manual" {
			t.Errorf("source for %s = %q, %v", field, source, err)
		}
	}
	books, err := db.SearchBooks(ctx, `"Updated"*`, BookPage{})
	if err != nil || len(books) != 1 {
		t.Errorf("search updated title = %v, %v", books, err)
	}
	books, err = db.SearchBooks(ctx, `"Old"*`, BookPage{})
	if err != nil || len(books) != 0 {
		t.Errorf("search old metadata = %v, %v", books, err)
	}
}

// A field the user deliberately cleared is still manually claimed. This is
// the case a future provider pass must not misread: an empty value is not
// missing metadata if someone chose to empty it, and provenance is the
// only thing that can tell the two apart.
func TestClearedFieldKeepsManualProvenance(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{
		ContentHash: "cleared", Title: "Book", SortTitle: "book", Publisher: "Ace Books",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var source string
	if err := db.Read().QueryRow(
		`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldPublisher).Scan(&source); err != nil {
		t.Fatalf("publisher provenance after creation: %v", err)
	}
	if source != "embedded" {
		t.Errorf("source after creation = %q, want embedded", source)
	}

	if exists, err := db.UpdateBookField(ctx, id, FieldPublisher, "", time.Now()); err != nil || !exists {
		t.Fatalf("clear publisher = %v, %v", exists, err)
	}

	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Publisher != "" {
		t.Errorf("Publisher = %q, want empty", book.Publisher)
	}
	if err := db.Read().QueryRow(
		`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, id, FieldPublisher).Scan(&source); err != nil {
		t.Fatalf("publisher provenance after clearing: %v", err)
	}
	if source != "manual" {
		t.Errorf("source after clearing = %q, want manual — a blank manual value is a claim, not an absence", source)
	}
}

func TestUpdateBookFieldRejectsAuthorsAndUnknownBooks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "guards", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Authors live in a join table, so they cannot be written by the
	// scalar path — UpdateBookAuthors is the only way in.
	if _, err := db.UpdateBookField(ctx, id, FieldAuthors, "Someone", time.Now()); !errors.Is(err, ErrInvalidMetadataField) {
		t.Errorf("UpdateBookField(authors) error = %v, want ErrInvalidMetadataField", err)
	}

	// cover_path holds a path internal/cover.Store produced, not text a
	// person types — only ApplyEnrichedFields may write it.
	if _, err := db.UpdateBookField(ctx, id, FieldCover, "covers/x.jpg", time.Now()); !errors.Is(err, ErrInvalidMetadataField) {
		t.Errorf("UpdateBookField(cover) error = %v, want ErrInvalidMetadataField", err)
	}

	// An unknown book is absent, not an error — the caller turns that into
	// a 404, the same contract the finders use.
	for _, name := range []string{"field", "authors"} {
		var exists bool
		var err error
		if name == "field" {
			exists, err = db.UpdateBookField(ctx, 99999, FieldTitle, "x", time.Now())
		} else {
			exists, err = db.UpdateBookAuthors(ctx, 99999, []string{"x"}, time.Now())
		}
		if err != nil || exists {
			t.Errorf("update %s for an unknown book = %v, %v; want false, nil", name, exists, err)
		}
	}
}

// book_authors is keyed on (book_id, author_id), so a duplicate name would
// violate the primary key and roll the whole update back. createBookTx
// drops repeats at first occurrence; this public operation has to match,
// or a direct caller can fail a write with input the creation path accepts.
func TestUpdateBookAuthorsDropsRepeatsAtFirstOccurrence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.CreateBook(ctx, Book{ContentHash: "dupes", Title: "Book", SortTitle: "book"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	exists, err := db.UpdateBookAuthors(ctx, id, []string{"First", "Second", "First", "Third"}, time.Now())
	if err != nil {
		t.Fatalf("UpdateBookAuthors with a repeated name: %v", err)
	}
	if !exists {
		t.Fatal("exists = false")
	}

	authors, err := db.ListAuthorsForBook(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"First", "Second", "Third"}
	if len(authors) != len(want) {
		t.Fatalf("authors = %v, want %v", authors, want)
	}
	for i := range want {
		if authors[i] != want[i] {
			t.Errorf("authors = %v, want %v — the repeat keeps its first position", authors, want)
		}
	}

	// Positions stay contiguous: they are the order the book lists its
	// authors in, not the index of the submitted line.
	rows, err := db.Read().Query(`SELECT position FROM book_authors WHERE book_id = ? ORDER BY position`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var positions []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		positions = append(positions, p)
	}
	for i, p := range positions {
		if p != i {
			t.Errorf("positions = %v, want 0..%d contiguous", positions, len(positions)-1)
			break
		}
	}
}

func TestClearProviderCover(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateBook(ctx, Book{
		ContentHash: "clear-cover", Title: "The Book", Publisher: "Press",
	}, []string{"An Author"})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if err := db.UpdateBookCoverPath(ctx, id, "covers/abc.jpg"); err != nil {
		t.Fatalf("UpdateBookCoverPath: %v", err)
	}
	// The state a provider-fetched cover leaves: a path, and a field_sources
	// row naming the provider — the only writer of a cover row.
	if _, _, err := db.ApplyEnrichedFields(ctx, id,
		map[MetadataField]string{FieldCover: "covers/abc.jpg"},
		map[MetadataField]string{FieldCover: "openlibrary"}, time.Now()); err != nil {
		t.Fatalf("ApplyEnrichedFields: %v", err)
	}
	if _, err := db.Read().Exec(`UPDATE books SET cover_retry = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	before, err := db.FindBookByID(ctx, id)
	if err != nil || before == nil {
		t.Fatalf("FindBookByID: %+v, %v", before, err)
	}

	at := time.Now().Add(time.Hour).Truncate(time.Second)
	exists, err := db.ClearProviderCover(ctx, id, at)
	if err != nil || !exists {
		t.Fatalf("ClearProviderCover: exists=%v, err=%v", exists, err)
	}

	after, err := db.FindBookByID(ctx, id)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID: %+v, %v", after, err)
	}
	if after.CoverPath != "" {
		t.Errorf("CoverPath = %q, want empty", after.CoverPath)
	}
	// cover_retry means "retry the extraction", and there is no embedded
	// original to extract — leaving it set sends the next sweep straight
	// back into the loop this exists to end.
	if after.CoverRetry {
		t.Error("CoverRetry still set")
	}
	if !after.ModifiedAt.After(before.ModifiedAt) {
		t.Errorf("modified_at = %v, want later than %v", after.ModifiedAt, before.ModifiedAt)
	}

	sources, err := db.FieldSourcesForBook(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if src, ok := sources[FieldCover]; ok {
		t.Errorf("cover source = %q, want the row gone", src)
	}
	// Row-scoped, not book-scoped: a DELETE without the field clause would
	// pass every assertion above and silently drop every other provenance
	// the book has.
	for _, f := range []MetadataField{FieldTitle, FieldPublisher, FieldAuthors} {
		if sources[f] == "" {
			t.Errorf("%s source was removed too; the delete must be row-scoped", f)
		}
	}
}

func TestClearProviderCoverUnknownBook(t *testing.T) {
	db := openTestDB(t)
	exists, err := db.ClearProviderCover(context.Background(), 9999, time.Now())
	if err != nil {
		t.Fatalf("ClearProviderCover: %v", err)
	}
	if exists {
		t.Error("exists = true for an unknown book")
	}
}
