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

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}
	if res.Values[storage.FieldPublisher] != "Ace Books" {
		t.Errorf("values[publisher] = %q, want %q", res.Values[storage.FieldPublisher], "Ace Books")
	}
	if res.SourceName[storage.FieldPublisher] != "fake" {
		t.Errorf("sourceName[publisher] = %q, want fake", res.SourceName[storage.FieldPublisher])
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
		return Metadata{Title: "Book", Publisher: "Ace Books", Description: "A description"}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (description was still missing)", p.calls)
	}
	if _, ok := res.Values[storage.FieldPublisher]; ok {
		t.Errorf("values contains publisher = %q, want it absent — the field was manually cleared", res.Values[storage.FieldPublisher])
	}
	if _, ok := res.SourceName[storage.FieldPublisher]; ok {
		t.Errorf("sourceName contains publisher, want it absent")
	}
	if res.Values[storage.FieldDescription] != "A description" {
		t.Errorf("values[description] = %q, want %q (this field genuinely was missing)", res.Values[storage.FieldDescription], "A description")
	}
}

// A field with a value is never asked for, whatever its source — even
// "embedded", which enrichment must never treat as worth improving on.
func TestResolveNeverOverwritesAPresentValue(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", Publisher: "Original Press", Description: ""}
	sources := map[storage.MetadataField]string{storage.FieldPublisher: "embedded"}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Title: "Book", Publisher: "A Different Press", Description: "New description"}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Values[storage.FieldPublisher]; ok {
		t.Errorf("values contains publisher = %q, want it absent — publisher already had a value", res.Values[storage.FieldPublisher])
	}
	if res.Values[storage.FieldDescription] != "New description" {
		t.Errorf("values[description] = %q, want %q", res.Values[storage.FieldDescription], "New description")
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

	res, err := Resolve(context.Background(), book, []string{"An Author"}, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", p.calls)
	}
	if len(res.Values) != 0 || len(res.SourceName) != 0 {
		t.Errorf("values = %v, sourceName = %v, want both empty", res.Values, res.SourceName)
	}
	// Zero asked, not zero failed-of-some-asked: this run is an honest
	// success and the worker's Asked > 0 clause is what keeps it one.
	if res.Asked != 0 || res.Failed != 0 {
		t.Errorf("asked = %d, failed = %d, want 0 and 0", res.Asked, res.Failed)
	}
}

// Every provider failing is what the worker turns into a failed job, so
// Resolve has to make it distinguishable: same empty Values as an honest
// no-match, but Failed == Asked rather than zero. Resolve itself still
// returns no error — a per-provider failure is not its own.
func TestResolveReportsEveryProviderFailing(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{}

	a := &fakeProvider{name: "provider-a", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("429 too many requests")
	}}
	b := &fakeProvider{name: "provider-b", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("503 service unavailable")
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{a, b})
	if err != nil {
		t.Fatalf("Resolve returned an error for per-provider failures: %v", err)
	}
	if res.Asked != 2 || res.Failed != 2 {
		t.Errorf("asked = %d, failed = %d, want 2 and 2", res.Asked, res.Failed)
	}
	if len(res.Values) != 0 {
		t.Errorf("values = %v, want empty", res.Values)
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

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls: a=%d, b=%d, want 1 each", a.calls, b.calls)
	}
	if res.Asked != 2 || res.Failed != 0 {
		t.Errorf("asked = %d, failed = %d, want 2 and 0", res.Asked, res.Failed)
	}
	if res.Values[storage.FieldPublisher] != "Ace Books" || res.SourceName[storage.FieldPublisher] != "provider-a" {
		t.Errorf("publisher = %q from %q, want Ace Books from provider-a", res.Values[storage.FieldPublisher], res.SourceName[storage.FieldPublisher])
	}
	if res.Values[storage.FieldDescription] != "A description" || res.SourceName[storage.FieldDescription] != "provider-b" {
		t.Errorf("description = %q from %q, want A description from provider-b", res.Values[storage.FieldDescription], res.SourceName[storage.FieldDescription])
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

	res, err := Resolve(context.Background(), book, authors, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 {
		t.Fatalf("provider-a calls = %d, want 1", a.calls)
	}
	if b.calls != 0 {
		t.Fatalf("provider-b calls = %d, want 0", b.calls)
	}
	// Asked counts providers actually called, not providers configured.
	// The caller's failure rule is Failed == Asked, so counting the
	// provider the early stop skipped would make a one-provider run look
	// like a two-provider one that half-failed.
	if res.Asked != 1 || res.Failed != 0 {
		t.Errorf("asked = %d, failed = %d, want 1 and 0 — provider-b was never called", res.Asked, res.Failed)
	}
	if res.Values[storage.FieldPublisher] != "Ace Books" || res.Values[storage.FieldDescription] != "A description" {
		t.Errorf("values = %v, want both fields from provider-a", res.Values)
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

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{a, b})
	if err != nil {
		t.Fatalf("Resolve returned an error for a per-provider failure: %v", err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls: a=%d, b=%d, want 1 each — the error must not block the next provider", a.calls, b.calls)
	}
	if res.Asked != 2 || res.Failed != 1 {
		t.Errorf("asked = %d, failed = %d, want 2 and 1", res.Asked, res.Failed)
	}
	if res.Values[storage.FieldPublisher] != "Ace Books" || res.SourceName[storage.FieldPublisher] != "provider-b" {
		t.Errorf("publisher = %q from %q, want Ace Books from provider-b", res.Values[storage.FieldPublisher], res.SourceName[storage.FieldPublisher])
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

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Values[storage.FieldPublisher]; ok {
		t.Errorf("values contains publisher = %q, want it discarded", res.Values[storage.FieldPublisher])
	}
	if res.Values[storage.FieldDescription] != "A description" {
		t.Errorf("values[description] = %q, want %q", res.Values[storage.FieldDescription], "A description")
	}
}

// Authors round-trip through the newline-joined string ApplyEnrichedFields
// expects, the same as every other field's missing/resolved value.
func TestResolveHandlesAuthorsAsAMissingField(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Title: "Book", Authors: []string{"First Author", "Second Author"}}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	want := "First Author\nSecond Author"
	if res.Values[storage.FieldAuthors] != want {
		t.Errorf("values[authors] = %q, want %q", res.Values[storage.FieldAuthors], want)
	}
	if res.SourceName[storage.FieldAuthors] != "fake" {
		t.Errorf("sourceName[authors] = %q, want fake", res.SourceName[storage.FieldAuthors])
	}
}

func TestResolveDoesNotAskForPresentAuthors(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Authors: []string{"Someone Else"}}, nil
	}}

	res, err := Resolve(context.Background(), book, []string{"Existing Author"}, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Values[storage.FieldAuthors]; ok {
		t.Errorf("values contains authors = %q, want it absent — the book already has authors", res.Values[storage.FieldAuthors])
	}
}

// A cover is missing exactly like any other empty field, and a provider's
// answer for it comes back through Resolution's separate CoverURL and
// CoverSource rather than the values map — see Resolve's doc comment for
// why.
func TestResolveAsksForCoverWhenMissing(t *testing.T) {
	book := storage.Book{ID: 1, Title: "Book", ISBN: "9780000000001"}
	sources := map[storage.MetadataField]string{}
	const wantCover = "https://covers.example/1-L.jpg"
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{CoverURL: wantCover}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if res.CoverURL != wantCover {
		t.Errorf("coverURL = %q, want %q", res.CoverURL, wantCover)
	}
	if res.CoverSource != "fake" {
		t.Errorf("coverSource = %q, want fake", res.CoverSource)
	}
	// The URL must never travel in values, which goes straight into
	// columns — cover_path holds a stored file's path, never a remote URL.
	if got, ok := res.Values[storage.FieldCover]; ok {
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

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	// Empty here is what stops the worker downloading an image the book has
	// no use for — the whole reason Metadata carries a URL, not bytes.
	if res.CoverURL != "" {
		t.Errorf("coverURL = %q, want empty — the book already has a cover", res.CoverURL)
	}
	if res.CoverSource != "" {
		t.Errorf("coverSource = %q, want empty", res.CoverSource)
	}
	if res.Values[storage.FieldDescription] != "A description" {
		t.Errorf("values[description] = %q, want %q (this field genuinely was missing)", res.Values[storage.FieldDescription], "A description")
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

	res, err := Resolve(context.Background(), book, authors, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if res.CoverURL != "https://covers.example/from-a.jpg" {
		t.Errorf("coverURL = %q, want provider-a's", res.CoverURL)
	}
	if res.CoverSource != "provider-a" {
		t.Errorf("coverSource = %q, want provider-a", res.CoverSource)
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

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Values[storage.FieldTitle]; got != "A Title with a line break" {
		t.Errorf("title = %q, want the line break collapsed and the value trimmed", got)
	}
	if got := res.Values[storage.FieldPublisher]; got != "Press Inc" {
		t.Errorf("publisher = %q, want %q", got, "Press Inc")
	}
	// Each name is sanitised on its own: authorsJoin is itself a newline,
	// so sanitising the joined string would collapse the list into one name.
	if got := res.Values[storage.FieldAuthors]; got != "First Author\nSecond Author" {
		t.Errorf("authors = %q, want two names with the interior break collapsed and the blank dropped", got)
	}
	if got := len(res.Values[storage.FieldDescription]); got > maxEnrichedDescriptionBytes {
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

	res, err := Resolve(context.Background(), book, nil, map[storage.MetadataField]string{}, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(res.Values[storage.FieldTitle]); got != maxEnrichedTitleBytes {
		t.Errorf("title is %d bytes, want it truncated to %d", got, maxEnrichedTitleBytes)
	}
	got := strings.Split(res.Values[storage.FieldAuthors], authorsJoin)
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

// A search answer that fails the gate is treated as no match: nothing is
// merged, the missing set is untouched, and the chain carries on to the
// next provider with the full set still to fill.
func TestResolveRejectsAnImplausibleSearchAnswer(t *testing.T) {
	book := storage.Book{ID: 1, Title: "01 - Fellowship"}
	sources := map[storage.MetadataField]string{}

	a := &fakeProvider{name: "provider-a", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Title: "The Fellowship of the Ring", Publisher: "Allen & Unwin", Description: "A different book"}, nil
	}}
	var sawMissing []storage.MetadataField
	b := &fakeProvider{name: "provider-b", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		sawMissing = append(sawMissing, storage.FieldPublisher)
		return Metadata{}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Values) != 0 {
		t.Errorf("values = %v, want empty — the answer was about a different book", res.Values)
	}
	if b.calls != 1 || len(sawMissing) != 1 {
		t.Errorf("provider-b calls = %d, want 1 — a rejected answer must not end the chain", b.calls)
	}
	// A rejection is not a provider failure: it is an answer this book
	// cannot use, which is the four-case contract's no-match case.
	if res.Failed != 0 {
		t.Errorf("failed = %d, want 0 — a rejected answer is not a provider error", res.Failed)
	}
}

func TestResolveAcceptsAPlausibleSearchAnswer(t *testing.T) {
	book := storage.Book{ID: 1, Title: "The Hobbit"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Title: "The Hobbit: 75th Anniversary Edition", Publisher: "Houghton Mifflin"}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[storage.FieldPublisher] != "Houghton Mifflin" {
		t.Errorf("values[publisher] = %q, want Houghton Mifflin", res.Values[storage.FieldPublisher])
	}
}

// Decision 3, and the test a later "why is this field being dropped?"
// cleanup deletes: a search answer never supplies an ISBN, even for a book
// that has none and even when the answer clears the gate.
func TestResolveNeverWritesISBNFromASearchAnswer(t *testing.T) {
	book := storage.Book{ID: 1, Title: "The Hobbit"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Title: "The Hobbit", ISBN: "9780261102217", Publisher: "Allen & Unwin"}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := res.Values[storage.FieldISBN]; ok {
		t.Errorf("values[isbn] = %q, want it absent — a ranking is not an identification", got)
	}
	if res.Values[storage.FieldPublisher] != "Allen & Unwin" {
		t.Errorf("values[publisher] = %q, want it kept — only isbn is withheld", res.Values[storage.FieldPublisher])
	}
}

// The withholding is scoped to the search path, not to the field — which
// shows up as a ByISBN answer never being gated at all. The titles here
// disagree completely and the answer still merges, because an ISBN names
// one edition: the answer is about this book by construction, and applying
// a title-similarity test to it would reject correct data on the strength
// of a provider's differing title string.
func TestResolveNeverGatesAnISBNAnswer(t *testing.T) {
	book := storage.Book{ID: 1, Title: "The Hobbit", ISBN: "9780261102217"}
	sources := map[storage.MetadataField]string{}
	p := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Title: "Something Else Entirely", Publisher: "Allen & Unwin"}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[storage.FieldPublisher] != "Allen & Unwin" {
		t.Errorf("values[publisher] = %q, want Allen & Unwin — a ByISBN answer is never gated", res.Values[storage.FieldPublisher])
	}
}

// Taken together with Decision 3, enrichment can no longer write isbn at
// all, and that is the intended consequence rather than an oversight: the
// field is only ever missing for a book with no ISBN, and such a book can
// only reach a provider through Search, where the value is withheld. A book
// that has one does not need it. Pinned because it reads as a bug to
// anyone who meets the skip without this reasoning.
func TestResolveNeverWritesISBNByAnyRoute(t *testing.T) {
	sources := map[storage.MetadataField]string{}
	answer := Metadata{Title: "The Hobbit", ISBN: "9780261102217", Publisher: "Allen & Unwin"}

	for _, book := range []storage.Book{
		{ID: 1, Title: "The Hobbit"},                        // no ISBN: search path
		{ID: 2, Title: "The Hobbit", ISBN: "9780000000001"}, // has one: ByISBN, then the fallback
	} {
		p := &fakeProvider{
			name:   "fake",
			byISBN: func(ctx context.Context, isbn string) (Metadata, error) { return Metadata{}, nil },
			search: func(ctx context.Context, title string, authors []string) (Metadata, error) { return answer, nil },
		}
		res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := res.Values[storage.FieldISBN]; ok {
			t.Errorf("book %d: values[isbn] = %q, want it absent by every route", book.ID, got)
		}
	}
}

// Decision 4: a clean no-match by ISBN falls back to a title search on the
// same provider, before the chain moves on.
func TestResolveFallsBackToSearchOnACleanISBNNoMatch(t *testing.T) {
	book := storage.Book{ID: 1, Title: "The Hobbit", ISBN: "9780261102217"}
	sources := map[storage.MetadataField]string{}

	var searchTitle string
	var searchAuthors []string
	p := &fakeProvider{
		name:   "fake",
		byISBN: func(ctx context.Context, isbn string) (Metadata, error) { return Metadata{}, nil },
		search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
			searchTitle, searchAuthors = title, authors
			return Metadata{Title: "The Hobbit", Publisher: "Allen & Unwin"}, nil
		},
	}

	res, err := Resolve(context.Background(), book, []string{"J.R.R. Tolkien"}, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if searchTitle != "The Hobbit" || len(searchAuthors) != 1 {
		t.Errorf("search called with (%q, %v), want the book's own title and authors", searchTitle, searchAuthors)
	}
	if res.Values[storage.FieldPublisher] != "Allen & Unwin" {
		t.Errorf("values[publisher] = %q, want Allen & Unwin", res.Values[storage.FieldPublisher])
	}
	// One provider, two calls — Asked counts providers, not calls.
	if res.Asked != 1 || res.Failed != 0 {
		t.Errorf("asked = %d, failed = %d, want 1 and 0", res.Asked, res.Failed)
	}
}

// The negative half, which matters more than the positive one: an ISBN
// lookup that *errors* must not fall back. A transient 5xx says nothing
// about whether the ISBN is right, and searching on it would accept a
// fuzzy answer because a host was briefly unreachable.
func TestResolveDoesNotFallBackWhenTheISBNLookupErrors(t *testing.T) {
	book := storage.Book{ID: 1, Title: "The Hobbit", ISBN: "9780261102217"}
	sources := map[storage.MetadataField]string{}

	searched := false
	a := &fakeProvider{
		name: "provider-a",
		byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
			return Metadata{}, errors.New("503 service unavailable")
		},
		search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
			searched = true
			return Metadata{Title: "The Hobbit", Publisher: "Wrong Press"}, nil
		},
	}
	b := &fakeProvider{name: "provider-b", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Publisher: "Allen & Unwin"}, nil
	}}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if searched {
		t.Error("provider-a's search was called after its ISBN lookup errored")
	}
	if res.Values[storage.FieldPublisher] != "Allen & Unwin" {
		t.Errorf("values[publisher] = %q, want provider-b's answer", res.Values[storage.FieldPublisher])
	}
	if res.Asked != 2 || res.Failed != 1 {
		t.Errorf("asked = %d, failed = %d, want 2 and 1", res.Asked, res.Failed)
	}
}

// An ISBN lookup that answers is never followed by a search, however
// partial the answer.
func TestResolveDoesNotSearchWhenTheISBNLookupAnswers(t *testing.T) {
	book := storage.Book{ID: 1, Title: "The Hobbit", ISBN: "9780261102217"}
	sources := map[storage.MetadataField]string{}

	searched := false
	p := &fakeProvider{
		name: "fake",
		byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
			return Metadata{Publisher: "Allen & Unwin"}, nil
		},
		search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
			searched = true
			return Metadata{}, nil
		},
	}

	if _, err := Resolve(context.Background(), book, nil, sources, []Provider{p}); err != nil {
		t.Fatal(err)
	}
	if searched {
		t.Error("search was called even though the ISBN lookup answered")
	}
}

// A cover-only reply is an answer, not a no-match — Metadata.IsEmpty says
// so — and must not provoke a fallback search.
func TestResolveDoesNotSearchPastACoverOnlyISBNAnswer(t *testing.T) {
	book := storage.Book{ID: 1, Title: "The Hobbit", ISBN: "9780261102217"}
	sources := map[storage.MetadataField]string{}

	searched := false
	p := &fakeProvider{
		name: "fake",
		byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
			return Metadata{CoverURL: "https://covers.example/1.jpg"}, nil
		},
		search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
			searched = true
			return Metadata{}, nil
		},
	}

	res, err := Resolve(context.Background(), book, nil, sources, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if searched {
		t.Error("search was called past a cover-only answer")
	}
	if res.CoverURL != "https://covers.example/1.jpg" {
		t.Errorf("coverURL = %q, want the answer's", res.CoverURL)
	}
}

// A book with no title has nothing to search on, so neither branch calls
// Search — saving a round trip and a rate-limit token for an answer the
// gate would reject anyway.
func TestResolveDoesNotSearchWithoutATitle(t *testing.T) {
	sources := map[storage.MetadataField]string{}

	for _, book := range []storage.Book{
		{ID: 1, Title: ""},
		{ID: 2, Title: "", ISBN: "9780261102217"},
	} {
		searched := false
		p := &fakeProvider{
			name:   "fake",
			byISBN: func(ctx context.Context, isbn string) (Metadata, error) { return Metadata{}, nil },
			search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
				searched = true
				return Metadata{}, nil
			},
		}
		if _, err := Resolve(context.Background(), book, nil, sources, []Provider{p}); err != nil {
			t.Fatal(err)
		}
		if searched {
			t.Errorf("book %d: search was called with an empty title", book.ID)
		}
	}
}
