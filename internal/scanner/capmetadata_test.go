package scanner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"library/internal/service"
	"library/internal/storage"
)

// overlongEPUB writes an EPUB whose embedded metadata is past every limit a
// person's edit meets. The description has to fit inside internal/epub's
// 4 MiB OPF bound while still exceeding 64 KiB, so it is built here rather
// than taken from a fixture.
func overlongEPUB(t *testing.T, path string) {
	t.Helper()

	var creators strings.Builder
	for i := range 150 {
		fmt.Fprintf(&creators, "<dc:creator>Author %03d</dc:creator>\n    ", i)
	}

	opfXML := fmt.Sprintf(`<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    %s
    <dc:description>%s</dc:description>
    <dc:publisher>%s</dc:publisher>
  </metadata>
  <manifest></manifest>
</package>`,
		// Multi-byte throughout, so a cut on a byte boundary would leave
		// an invalid rune behind and the assertions would see it
		strings.Repeat("é", 1000),
		creators.String(),
		strings.Repeat("ü", storage.MaxDescriptionBytes),
		strings.Repeat("ß", storage.MaxScalarBytes))

	writeTestEPUBWithOPF(t, path, opfXML)
}

func TestCreateBookCapsEmbeddedMetadata(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	overlongEPUB(t, filepath.Join(libDir, "verbose.epub"))

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	file, err := db.FindFileByPath(ctx, "verbose.epub")
	if err != nil || file == nil {
		t.Fatalf("FindFileByPath = %v, %v", file, err)
	}
	book, err := db.FindBookByID(ctx, file.BookID)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID = %v, %v", book, err)
	}

	for _, tt := range []struct {
		field storage.MetadataField
		value string
		limit int
	}{
		{storage.FieldTitle, book.Title, storage.MaxTitleBytes},
		{storage.FieldDescription, book.Description, storage.MaxDescriptionBytes},
		{storage.FieldPublisher, book.Publisher, storage.MaxScalarBytes},
	} {
		if len(tt.value) > tt.limit {
			t.Errorf("%s is %d bytes, want at most %d", tt.field, len(tt.value), tt.limit)
		}
		if tt.value == "" {
			t.Errorf("%s is empty: an over-long value is truncated, never dropped", tt.field)
		}
		if !utf8.ValidString(tt.value) {
			t.Errorf("%s was cut mid-rune", tt.field)
		}
	}

	authors, err := db.ListAuthorsForBook(ctx, book.ID)
	if err != nil {
		t.Fatalf("ListAuthorsForBook: %v", err)
	}
	if len(authors) != storage.MaxAuthors {
		t.Fatalf("authors = %d names, want the list cut at %d", len(authors), storage.MaxAuthors)
	}
	// Source order, so the cut takes the tail rather than an arbitrary
	// hundred: the first credited author is the one the grid shows
	if authors[0] != "Author 000" || authors[storage.MaxAuthors-1] != fmt.Sprintf("Author %03d", storage.MaxAuthors-1) {
		t.Errorf("authors = %v…%v, want the first %d in source order", authors[0], authors[len(authors)-1], storage.MaxAuthors)
	}

	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		t.Fatalf("FieldSourcesForBook: %v", err)
	}
	// Truncation says nothing about where a value came from
	for _, field := range []storage.MetadataField{
		storage.FieldTitle, storage.FieldDescription, storage.FieldPublisher, storage.FieldAuthors,
	} {
		if sources[field] != "embedded" {
			t.Errorf("%s source = %q, want %q", field, sources[field], "embedded")
		}
	}
}

// The property the caps exist for: what the scanner stored is a value the
// editor hands back unchanged. Without them, opening the editor on a
// 10 MB description and pressing Save fails on a value nobody typed.
func TestCreateBookCappedValuesAreEditable(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	overlongEPUB(t, filepath.Join(libDir, "verbose.epub"))

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	file, err := db.FindFileByPath(ctx, "verbose.epub")
	if err != nil || file == nil {
		t.Fatalf("FindFileByPath = %v, %v", file, err)
	}
	book, err := db.FindBookByID(ctx, file.BookID)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID = %v, %v", book, err)
	}

	svc := service.New(db)
	for _, tt := range []struct {
		field string
		value string
	}{
		{"title", book.Title},
		{"description", book.Description},
		{"publisher", book.Publisher},
	} {
		detail, err := svc.UpdateBookMetadata(ctx, book.ID, service.MetadataUpdate{Field: tt.field, Value: tt.value})
		if err != nil {
			t.Fatalf("UpdateBookMetadata %s: %v", tt.field, err)
		}
		if detail == nil {
			t.Fatalf("UpdateBookMetadata %s: book not found", tt.field)
		}
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID after the edits = %v, %v", after, err)
	}
	if after.Title != book.Title {
		t.Errorf("title round-tripped to %d bytes, want the %d it went in as", len(after.Title), len(book.Title))
	}
	if after.Description != book.Description {
		t.Errorf("description round-tripped to %d bytes, want the %d it went in as", len(after.Description), len(book.Description))
	}
	if after.Publisher != book.Publisher {
		t.Errorf("publisher round-tripped to %d bytes, want the %d it went in as", len(after.Publisher), len(book.Publisher))
	}
}
