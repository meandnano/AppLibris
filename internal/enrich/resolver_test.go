package enrich

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"library/internal/storage"
)

// fakeProvider is a Provider with no HTTP: byISBN/search decide the answer
// per call (nil means "no answer, no error" — Metadata{}), and every call
// is counted so tests can assert a provider was, or was not, asked.
type fakeProvider struct {
	name   string
	byISBN func(ctx context.Context, isbn string) (Metadata, error)
	search func(ctx context.Context, title string, authors []string) (Metadata, error)
	calls  int
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) ByISBN(ctx context.Context, isbn string) (Metadata, error) {
	f.calls++
	if f.byISBN == nil {
		return Metadata{}, nil
	}
	return f.byISBN(ctx, isbn)
}

func (f *fakeProvider) Search(ctx context.Context, title string, authors []string) (Metadata, error) {
	f.calls++
	if f.search == nil {
		return Metadata{}, nil
	}
	return f.search(ctx, title, authors)
}

// TestIsMissing is Decision 1's rule in isolation, one case per branch —
// dropping either half of isMissing breaks exactly one of these.
func TestIsMissing(t *testing.T) {
	cases := []struct {
		name          string
		value, source string
		want          bool
	}{
		{"empty, no source recorded", "", "", true},
		{"empty, embedded", "", "embedded", true},
		{"empty, manual — deliberately cleared", "", "manual", false},
		{"non-empty, embedded", "x", "embedded", false},
		{"non-empty, manual", "x", "manual", false},
		{"non-empty, no source recorded", "x", "", false},
	}
	for _, c := range cases {
		if got := isMissing(c.value, c.source); got != c.want {
			t.Errorf("%s: isMissing(%q, %q) = %v, want %v", c.name, c.value, c.source, got, c.want)
		}
	}
}

func TestResolveAsksForEmptyEmbeddedField(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{storage.FieldTitle: "embedded"}
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books"}, nil
	}}

	values, sourceName, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}
	if values[storage.FieldPublisher] != "Ace Books" {
		t.Errorf("values[publisher] = %q, want %q", values[storage.FieldPublisher], "Ace Books")
	}
	if sourceName[storage.FieldPublisher] != "fake" {
		t.Errorf("sourceName[publisher] = %q, want fake", sourceName[storage.FieldPublisher])
	}
}

// The regression this whole step exists to prevent: a field a person
// deliberately cleared must not be refilled, even when a provider call —
// triggered by some other, genuinely missing field — happens to answer it
// too.
func TestResolveDoesNotAskForManuallyClearedField(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", Publisher: ""}
	sources := map[storage.MetadataField]string{storage.FieldPublisher: "manual"}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books", Description: "A description"}, nil
	}}

	values, sourceName, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (description was still missing)", p.calls)
	}
	if _, ok := values[storage.FieldPublisher]; ok {
		t.Errorf("values contains publisher = %q, want it absent — the field was manually cleared", values[storage.FieldPublisher])
	}
	if _, ok := sourceName[storage.FieldPublisher]; ok {
		t.Errorf("sourceName contains publisher, want it absent")
	}
	if values[storage.FieldDescription] != "A description" {
		t.Errorf("values[description] = %q, want %q (this field genuinely was missing)", values[storage.FieldDescription], "A description")
	}
}

// A field with a value is never asked for, whatever its source — even
// "embedded", which enrichment must never treat as worth improving on.
func TestResolveNeverOverwritesAPresentValue(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", Publisher: "Original Press", Description: ""}
	sources := map[storage.MetadataField]string{storage.FieldPublisher: "embedded"}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Publisher: "A Different Press", Description: "New description"}, nil
	}}

	values, _, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values[storage.FieldPublisher]; ok {
		t.Errorf("values contains publisher = %q, want it absent — publisher already had a value", values[storage.FieldPublisher])
	}
	if values[storage.FieldDescription] != "New description" {
		t.Errorf("values[description] = %q, want %q", values[storage.FieldDescription], "New description")
	}
}

// A book with nothing missing must not call a provider at all — the API
// call this step's whole field-level design exists to save.
func TestResolveCallsNoProviderWhenNothingIsMissing(t *testing.T) {
	book := storage.Book{
		ID: 1, Title: "Book", Publisher: "Press", PublishedDate: "2020",
		Language: "en", ISBN: "9780000000001", Description: "Text",
		CoverPath: "covers/existing.jpg",
	}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake"}

	values, sourceName, _, _, err := Resolve(context.Background(), book, []string{"An Author"}, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", p.calls)
	}
	if len(values) != 0 || len(sourceName) != 0 {
		t.Errorf("values = %v, sourceName = %v, want both empty", values, sourceName)
	}
}

// Field-level merge: two providers each answer a different missing field,
// and each field's source names the provider that actually answered it.
func TestResolveMergesFieldsAcrossProviders(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", Publisher: "", Description: "", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{}

	a := &fakeProvider{name: "provider-a", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books"}, nil
	}}
	b := &fakeProvider{name: "provider-b", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Description: "A description"}, nil
	}}

	values, sourceName, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls: a=%d, b=%d, want 1 each", a.calls, b.calls)
	}
	if values[storage.FieldPublisher] != "Ace Books" || sourceName[storage.FieldPublisher] != "provider-a" {
		t.Errorf("publisher = %q from %q, want Ace Books from provider-a", values[storage.FieldPublisher], sourceName[storage.FieldPublisher])
	}
	if values[storage.FieldDescription] != "A description" || sourceName[storage.FieldDescription] != "provider-b" {
		t.Errorf("description = %q from %q, want A description from provider-b", values[storage.FieldDescription], sourceName[storage.FieldDescription])
	}
}

// Once the first provider has answered everything missing, the chain
// stops early — the second provider must never be called.
func TestResolveStopsEarlyOnceNothingIsMissing(t *testing.T) {
	// Every field but publisher and description is already present, so
	// provider-a's answer to those two truly empties the missing set —
	// otherwise a leftover missing field (say, an unset language) would
	// call provider-b for an unrelated reason and this test wouldn't be
	// exercising the early-stop path at all.
	book := storage.Book{
		ID: 1, Title: "Book", Publisher: "", PublishedDate: "2020",
		Language: "en", ISBN: "9780000000001", Description: "",
		CoverPath: "covers/existing.jpg",
	}
	sources := map[storage.MetadataField]string{}
	authors := []string{"An Author"}

	a := &fakeProvider{name: "provider-a", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books", Description: "A description"}, nil
	}}
	b := &fakeProvider{name: "provider-b", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		t.Error("provider-b was called; provider-a already answered everything missing")
		return Metadata{}, nil
	}}

	values, _, _, _, err := Resolve(context.Background(), book, authors, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 {
		t.Fatalf("provider-a calls = %d, want 1", a.calls)
	}
	if b.calls != 0 {
		t.Fatalf("provider-b calls = %d, want 0", b.calls)
	}
	if values[storage.FieldPublisher] != "Ace Books" || values[storage.FieldDescription] != "A description" {
		t.Errorf("values = %v, want both fields from provider-a", values)
	}
}

// A provider error is skipped, not fatal: the chain continues to the next
// provider, and the job still resolves whatever that next one answers.
func TestResolveSkipsAProviderThatErrors(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", Publisher: "", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{}

	a := &fakeProvider{name: "provider-a", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("network unreachable")
	}}
	b := &fakeProvider{name: "provider-b", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Ace Books"}, nil
	}}

	values, sourceName, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{a, b})
	if err != nil {
		t.Fatalf("Resolve returned an error for a per-provider failure: %v", err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls: a=%d, b=%d, want 1 each — the error must not block the next provider", a.calls, b.calls)
	}
	if values[storage.FieldPublisher] != "Ace Books" || sourceName[storage.FieldPublisher] != "provider-b" {
		t.Errorf("publisher = %q from %q, want Ace Books from provider-b", values[storage.FieldPublisher], sourceName[storage.FieldPublisher])
	}
}

// A provider must not be able to widen its own mandate: an answer for a
// field that was not missing is discarded even though the provider was
// legitimately asked (for a different, genuinely missing field).
func TestResolveDiscardsAnswersForFieldsNotMissing(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", Publisher: "Original Press", Description: "", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{storage.FieldPublisher: "embedded"}
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Uninvited Press", Description: "A description"}, nil
	}}

	values, _, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values[storage.FieldPublisher]; ok {
		t.Errorf("values contains publisher = %q, want it discarded", values[storage.FieldPublisher])
	}
	if values[storage.FieldDescription] != "A description" {
		t.Errorf("values[description] = %q, want %q", values[storage.FieldDescription], "A description")
	}
}

// Authors round-trip through the newline-joined string ApplyEnrichedFields
// expects, the same as every other field's missing/resolved value.
func TestResolveHandlesAuthorsAsAMissingField(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Authors: []string{"First Author", "Second Author"}}, nil
	}}

	values, sourceName, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	want := "First Author\nSecond Author"
	if values[storage.FieldAuthors] != want {
		t.Errorf("values[authors] = %q, want %q", values[storage.FieldAuthors], want)
	}
	if sourceName[storage.FieldAuthors] != "fake" {
		t.Errorf("sourceName[authors] = %q, want fake", sourceName[storage.FieldAuthors])
	}
}

func TestResolveDoesNotAskForPresentAuthors(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Authors: []string{"Someone Else"}}, nil
	}}

	values, _, _, _, err := Resolve(context.Background(), book, []string{"Existing Author"}, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values[storage.FieldAuthors]; ok {
		t.Errorf("values contains authors = %q, want it absent — the book already has authors", values[storage.FieldAuthors])
	}
}

// A cover is missing exactly like any other empty field, and a provider's
// answer for it comes back through Resolve's separate coverURL/coverSource
// return values rather than the values map — see Resolve's doc comment for
// why.
func TestResolveAsksForCoverWhenMissing(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{}
	const wantCover = "https://covers.example/1-L.jpg"
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: wantCover}, nil
	}}

	values, _, coverURL, coverSource, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if coverURL != wantCover {
		t.Errorf("coverURL = %q, want %q", coverURL, wantCover)
	}
	if coverSource != "fake" {
		t.Errorf("coverSource = %q, want fake", coverSource)
	}
	// The URL must never travel in values, which goes straight into
	// columns — cover_path holds a stored file's path, never a remote URL.
	if got, ok := values[storage.FieldCover]; ok {
		t.Errorf("values[cover] = %q, want it absent — only the worker may put a path there", got)
	}
}

// The regression this test guards: a book that already has a cover must
// never be handed a provider's cover answer, even though the provider was
// asked anyway (for some other, genuinely missing field) and happened to
// include one.
func TestResolveDoesNotAskForCoverWhenPresent(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", Description: "", ISBN: "9780000000001", CoverPath: "covers/existing.jpg"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: "https://covers.example/new.jpg", Description: "A description"}, nil
	}}

	values, _, coverURL, coverSource, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	// Empty here is what stops the worker downloading an image the book has
	// no use for — the whole reason Metadata carries a URL, not bytes.
	if coverURL != "" {
		t.Errorf("coverURL = %q, want empty — the book already has a cover", coverURL)
	}
	if coverSource != "" {
		t.Errorf("coverSource = %q, want empty", coverSource)
	}
	if values[storage.FieldDescription] != "A description" {
		t.Errorf("values[description] = %q, want %q (this field genuinely was missing)", values[storage.FieldDescription], "A description")
	}
}

// Once one provider has answered the cover, an earlier provider's answer
// wins and a later one's is discarded — the same first-answer-wins rule
// every other field gets. Every field but the cover is already present, so
// provider-a's cover answer truly empties the missing set — the same
// isolation TestResolveStopsEarlyOnceNothingIsMissing applies to text
// fields.
func TestResolveKeepsFirstProvidersCoverAnswer(t *testing.T) {
	book := storage.Book{
		ID: 1, Title: "Book", Publisher: "Press", PublishedDate: "2020",
		Language: "en", ISBN: "9780000000001", Description: "Text",
	}
	sources := map[storage.MetadataField]string{}
	authors := []string{"An Author"}

	a := &fakeProvider{name: "provider-a", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: "https://covers.example/from-a.jpg"}, nil
	}}
	b := &fakeProvider{name: "provider-b", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		t.Error("provider-b was called; provider-a already answered the only missing field (cover)")
		return Metadata{}, nil
	}}

	_, _, coverURL, coverSource, err := Resolve(context.Background(), book, authors, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if coverURL != "https://covers.example/from-a.jpg" {
		t.Errorf("coverURL = %q, want provider-a's", coverURL)
	}
	if coverSource != "provider-a" {
		t.Errorf("coverSource = %q, want provider-a", coverSource)
	}
}

// A provider's answer never passes through internal/service's
// normalizeField, so the resolver is the only thing bounding what a remote
// source can put in a column. A line break in any single-line field breaks
// its rendering everywhere downstream; an unbounded description is a remote
// party choosing this database's row size.
func TestResolveSanitizesProviderValues(t *testing.T) {
	book := storage.Book{ID: 1, Title: "", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{}
	longDescription := strings.Repeat("x", maxEnrichedDescriptionBytes+500)

	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{
			Title:       "  A Title\nwith a line break  ",
			Publisher:   "Press\r\nInc",
			Authors:     []string{"  First Author  ", "Second\nAuthor", "   "},
			Description: longDescription,
		}, nil
	}}

	values, _, _, _, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if got := values[storage.FieldTitle]; got != "A Title with a line break" {
		t.Errorf("title = %q, want the line break collapsed and the value trimmed", got)
	}
	if got := values[storage.FieldPublisher]; got != "Press Inc" {
		t.Errorf("publisher = %q, want %q", got, "Press Inc")
	}
	// Each name is sanitised on its own: authorsJoin is itself a newline,
	// so sanitising the joined string would collapse the list into one name.
	if got := values[storage.FieldAuthors]; got != "First Author\nSecond Author" {
		t.Errorf("authors = %q, want two names with the interior break collapsed and the blank dropped", got)
	}
	if got := len(values[storage.FieldDescription]); got > maxEnrichedDescriptionBytes {
		t.Errorf("description is %d bytes, want at most %d", got, maxEnrichedDescriptionBytes)
	}
}

// Truncation must not leave a half-written rune behind: the column is text,
// and invalid UTF-8 in it renders as a replacement character forever.
func TestSanitizeValueTruncatesOnARuneBoundary(t *testing.T) {
	value := strings.Repeat("é", maxEnrichedScalarBytes)
	got := sanitizeValue(storage.FieldPublisher, value)
	if len(got) > maxEnrichedScalarBytes {
		t.Errorf("length = %d, want at most %d", len(got), maxEnrichedScalarBytes)
	}
	if !utf8.ValidString(got) {
		t.Error("truncated value is not valid UTF-8")
	}
}

// internal/service refuses a title over its own limit and a list over 100
// authors. A provider answer this package writes but that one would reject
// leaves the field un-editable through the app: opening the editor and
// pressing Save unchanged fails on a value nobody typed.
func TestResolveCapsMatchTheEditableLimits(t *testing.T) {
	names := make([]string, maxEnrichedAuthors+20)
	for i := range names {
		names[i] = fmt.Sprintf("Author %d", i)
	}

	book := storage.Book{ID: 1, ISBN: "9780000000001"}
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{
			Title:   strings.Repeat("t", maxEnrichedTitleBytes+500),
			Authors: names,
		}, nil
	}}

	values, _, _, _, err := Resolve(context.Background(), book, nil, map[storage.MetadataField]string{}, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(values[storage.FieldTitle]); got != maxEnrichedTitleBytes {
		t.Errorf("title is %d bytes, want it truncated to %d", got, maxEnrichedTitleBytes)
	}
	got := strings.Split(values[storage.FieldAuthors], authorsJoin)
	if len(got) != maxEnrichedAuthors {
		t.Errorf("authors = %d names, want the list cut at %d", len(got), maxEnrichedAuthors)
	}
	if got[0] != "Author 0" {
		t.Errorf("first author = %q, want the list cut from the end, not the front", got[0])
	}
}

// An over-long single name is capped on its own, not by the joined list's
// length — the same reason each name is sanitised separately.
func TestSanitizeValueCapsOneAuthorName(t *testing.T) {
	got := sanitizeValue(storage.FieldAuthors, strings.Repeat("a", maxEnrichedAuthorNameBytes+10))
	if len(got) != maxEnrichedAuthorNameBytes {
		t.Errorf("length = %d, want %d", len(got), maxEnrichedAuthorNameBytes)
	}
}
