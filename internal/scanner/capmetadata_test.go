package scanner

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"library/internal/service"
	"library/internal/storage"
)

// straddling builds a valid UTF-8 value of at least n bytes positioned so
// that a cut at any of the Max* limits lands *inside* a rune.
//
// Getting this wrong is the easy way to write a test that proves nothing:
// every limit here is even, so a filler of two-byte runes is cut on a rune
// boundary by arithmetic alone and `strings.ToValidUTF8` could be deleted
// from capValue with every assertion still green. Three-byte runes behind a
// two-byte lead-in straddle all three limits, since each is 1 mod 3 and the
// lead-in shifts that to 2
func straddling(n int) string {
	return "aa" + strings.Repeat("€", n/3+1)
}

// overlongEPUB writes an EPUB whose embedded metadata is past every limit a
// person's edit meets. The description has to fit inside internal/epub's
// 4 MiB OPF bound while still exceeding 64 KiB, so it is built here rather
// than taken from a fixture
func overlongEPUB(t *testing.T, path string) {
	t.Helper()

	var creators strings.Builder
	for i := range 150 {
		fmt.Fprintf(&creators, "<dc:creator>Author %03d</dc:creator>\n    ", i)
	}
	// One name past the per-name cap, so the author list is cut two ways
	fmt.Fprintf(&creators, "<dc:creator>%s</dc:creator>", straddling(storage.MaxAuthorNameBytes+500))

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
		straddling(storage.MaxTitleBytes+500),
		creators.String(),
		straddling(storage.MaxDescriptionBytes*2),
		straddling(storage.MaxScalarBytes+500))

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
		// straddling puts a rune across every limit, so the surviving
		// value must stop short of it. Landing exactly on the limit means
		// the partial rune was kept
		if len(tt.value) != tt.limit-2 {
			t.Errorf("%s is %d bytes, want %d — the last whole rune before the %d-byte limit",
				tt.field, len(tt.value), tt.limit-2, tt.limit)
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
	// The per-name cap is a separate rule from the list cap, and the
	// over-long name sits past the list cut, so it has to be checked on
	// its own rather than through the stored list
	longName := capValue("verbose.epub", storage.FieldAuthors,
		straddling(storage.MaxAuthorNameBytes+500), storage.MaxAuthorNameBytes)
	if len(longName) != storage.MaxAuthorNameBytes-2 {
		t.Errorf("a capped author name is %d bytes, want %d", len(longName), storage.MaxAuthorNameBytes-2)
	}
	if !utf8.ValidString(longName) {
		t.Error("a capped author name was cut mid-rune")
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
// 10 MB description and pressing Save fails on a value nobody typed
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

	authors, err := db.ListAuthorsForBook(ctx, book.ID)
	if err != nil {
		t.Fatalf("ListAuthorsForBook: %v", err)
	}

	svc := service.New(db)
	for _, tt := range []struct {
		field string
		value string
	}{
		{"title", book.Title},
		{"description", book.Description},
		{"publisher", book.Publisher},
		// The editor submits the list as newline-separated lines, which is
		// the form both caps have to survive together: too many names and
		// one name too long are separate refusals in normalizeAuthors
		{"authors", strings.Join(authors, "\n")},
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

	authorsAfter, err := db.ListAuthorsForBook(ctx, book.ID)
	if err != nil {
		t.Fatalf("ListAuthorsForBook after the edits: %v", err)
	}
	if !slices.Equal(authorsAfter, authors) {
		t.Errorf("authors round-tripped to %d names, want the %d they went in as", len(authorsAfter), len(authors))
	}
}

// wrappedEPUB writes an EPUB whose metadata elements wrap across lines in
// the source XML, which is legal and is what a generator that pretty-prints
// its output produces. TrimSpace removes only what sits at either end, so
// every break here is interior and reaches capValue
func wrappedEPUB(t *testing.T, path string) {
	t.Helper()

	opfXML := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Violence
      and Its Discontents</dc:title>
    <dc:creator>Jean
      Baptiste Roe</dc:creator>
    <dc:publisher>Penguin
      Publishing Group</dc:publisher>
    <dc:description>One paragraph.
Another paragraph.</dc:description>
  </metadata>
  <manifest></manifest>
</package>`

	writeTestEPUBWithOPF(t, path, opfXML)
}

// The other half of the property above: a value the editor refuses is never
// stored either. normalizeField rejects a line break in every field but
// description, so a wrapped one reaches the editor rather than the
// validation error — an <input type="text"> drops it on submit and rewrites
// the field behind the person's back, and a wrapped author name in the
// <textarea> is split into two authors by normalizeAuthors
func TestCreateBookWrappedValuesAreEditable(t *testing.T) {
	libDir := t.TempDir()
	coversDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	wrappedEPUB(t, filepath.Join(libDir, "wrapped.epub"))

	if _, err := Scan(ctx, db, libDir, coversDir, testMissingGrace); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	file, err := db.FindFileByPath(ctx, "wrapped.epub")
	if err != nil || file == nil {
		t.Fatalf("FindFileByPath = %v, %v", file, err)
	}
	book, err := db.FindBookByID(ctx, file.BookID)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID = %v, %v", book, err)
	}

	if book.Title != "Violence and Its Discontents" {
		t.Errorf("Title = %q, want it collapsed onto one line", book.Title)
	}
	if book.Publisher != "Penguin Publishing Group" {
		t.Errorf("Publisher = %q, want it collapsed onto one line", book.Publisher)
	}
	// Description is the one field that keeps its breaks, and the one
	// normalizeField accepts them in
	if !strings.Contains(book.Description, "\n") {
		t.Errorf("Description = %q, want its paragraph break kept", book.Description)
	}

	authors, err := db.ListAuthorsForBook(ctx, book.ID)
	if err != nil {
		t.Fatalf("ListAuthorsForBook: %v", err)
	}
	if len(authors) != 1 || authors[0] != "Jean Baptiste Roe" {
		t.Fatalf("Authors = %v, want one name on one line", authors)
	}

	svc := service.New(db)
	for _, tt := range []struct {
		field string
		value string
	}{
		{"title", book.Title},
		{"publisher", book.Publisher},
		{"description", book.Description},
		{"authors", strings.Join(authors, "\n")},
	} {
		if _, err := svc.UpdateBookMetadata(ctx, book.ID, service.MetadataUpdate{Field: tt.field, Value: tt.value}); err != nil {
			t.Fatalf("UpdateBookMetadata %s: %v", tt.field, err)
		}
	}

	authorsAfter, err := db.ListAuthorsForBook(ctx, book.ID)
	if err != nil {
		t.Fatalf("ListAuthorsForBook after the edits: %v", err)
	}
	if !slices.Equal(authorsAfter, authors) {
		t.Errorf("authors round-tripped to %v, want the %v they went in as", authorsAfter, authors)
	}

	after, err := db.FindBookByID(ctx, book.ID)
	if err != nil || after == nil {
		t.Fatalf("FindBookByID after the edits = %v, %v", after, err)
	}
	if after.Title != book.Title || after.Publisher != book.Publisher || after.Description != book.Description {
		t.Errorf("a field changed across a Save of what was shown: %q/%q/%q",
			after.Title, after.Publisher, after.Description)
	}
}
