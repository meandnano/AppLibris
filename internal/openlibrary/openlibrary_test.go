package openlibrary

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"library/internal/enrich"
)

// The fixtures under testdata are shaped after openlibrary.org/search.json's
// documented response format rather than a live capture: this sandbox has
// no outbound network access, so a real request can't be made from here.
// Field names and nesting match the API's stable, publicly documented
// shape.

// testClient wires coverBaseURL to its own isolated server that 404s by
// default, kept separate from the search server and its hits counter: a
// match fixture carrying a cover_i (search_match.json does) would otherwise
// make every test using it fire a second, uncounted request at the same
// handler search assertions (query params, hit counts) don't expect. Tests
// that care about the cover fetch itself override client.coverBaseURL
// after the fact.
func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *int) {
	t.Helper()
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	coverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(coverServer.Close)

	return &Client{
		baseURL:      server.URL,
		coverBaseURL: coverServer.URL,
		httpClient:   server.Client(),
	}, &hits
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestNewSetsTimeout(t *testing.T) {
	c := New()
	if c.httpClient.Timeout != Timeout {
		t.Errorf("httpClient.Timeout = %v, want Timeout (%v); http.DefaultClient has none at all", c.httpClient.Timeout, Timeout)
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "openlibrary" {
		t.Errorf("Name() = %q, want %q", got, "openlibrary")
	}
}

func TestByISBNMatchParsesFixture(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}

	if got.Title != "Structure and Interpretation of Computer Programs" {
		t.Errorf("Title = %q", got.Title)
	}
	wantAuthors := []string{"Harold Abelson", "Gerald Jay Sussman"}
	if len(got.Authors) != len(wantAuthors) || got.Authors[0] != wantAuthors[0] || got.Authors[1] != wantAuthors[1] {
		t.Errorf("Authors = %v, want %v", got.Authors, wantAuthors)
	}
	if got.Publisher != "MIT Press" {
		t.Errorf("Publisher = %q, want %q", got.Publisher, "MIT Press")
	}
	if got.PublishedDate != "1996" {
		t.Errorf("PublishedDate = %q, want %q", got.PublishedDate, "1996")
	}
	// The fixture answers MARC "eng"; the library stores ISO 639-1, the
	// same form internal/epub and internal/fb2 produce.
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q", got.Language, "en")
	}
	if got.ISBN != "9780262011532" {
		t.Errorf("ISBN = %q, want %q (the 13-digit form, normalised)", got.ISBN, "9780262011532")
	}
}

func TestByISBNNormalisesRequestISBN(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("isbn"); got != "9780262011532" {
			t.Errorf("request isbn = %q, want normalised %q", got, "9780262011532")
		}
		w.Write(readFixture(t, "search_no_match.json"))
	})

	if _, err := client.ByISBN(context.Background(), "978-0-262-01153-2"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
}

func TestByISBNNoMatchIsNotAnError(t *testing.T) {
	client, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_no_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "0000000000")
	if err != nil {
		t.Fatalf("ByISBN: want nil error for a 200 with no docs (the ordinary case), got %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
	if *hits != 1 {
		t.Errorf("server hits = %d, want 1", *hits)
	}
}

func TestByISBN404IsNotAnError(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: want nil error on 404 — a missing record is an answer, got %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
}

func TestByISBN429IsAnError(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := client.ByISBN(context.Background(), "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on 429, got nil")
	}
}

func TestByISBN5xxIsAnError(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := client.ByISBN(context.Background(), "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on 503, got nil")
	}
}

func TestByISBNTransportErrorIsAnError(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := client.ByISBN(ctx, "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on a timed-out request, got nil")
	}
}

func TestByISBNMalformedBodyIsAnError(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not valid json"))
	})

	_, err := client.ByISBN(context.Background(), "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on malformed body, got nil")
	}
}

func TestByISBNEmptyISBNNeverCallsServer(t *testing.T) {
	client, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_no_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
	if *hits != 0 {
		t.Errorf("server hits = %d, want 0 — an empty ISBN has nothing to look up", *hits)
	}
}

func TestSearchSendsTitleAndAuthor(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("title"); got != "Structure and Interpretation of Computer Programs" {
			t.Errorf("title = %q", got)
		}
		if got := q.Get("author"); got != "Harold Abelson" {
			t.Errorf("author = %q", got)
		}
		w.Write(readFixture(t, "search_match.json"))
	})

	got, err := client.Search(context.Background(), "Structure and Interpretation of Computer Programs", []string{"Harold Abelson"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.Title == "" {
		t.Error("Title is empty")
	}
}

func TestSearchEmptyTitleNeverCallsServer(t *testing.T) {
	client, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_no_match.json"))
	})

	got, err := client.Search(context.Background(), "", []string{"Someone"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
	if *hits != 0 {
		t.Errorf("server hits = %d, want 0 — an empty title has nothing to search on", *hits)
	}
}

func TestSearchNoMatchIsNotAnError(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_no_match.json"))
	})

	got, err := client.Search(context.Background(), "Some Obscure Title Nobody Wrote About", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
}

func TestBestISBNPrefers13Digit(t *testing.T) {
	got := bestISBN([]string{"0262011530", "9780262011532", "978-0-262-01153-2"})
	if got != "9780262011532" {
		t.Errorf("bestISBN = %q, want %q", got, "9780262011532")
	}
}

func TestBestISBNFallsBackTo10Digit(t *testing.T) {
	got := bestISBN([]string{"0262011530"})
	if got != "0262011530" {
		t.Errorf("bestISBN = %q, want %q", got, "0262011530")
	}
}

func TestBestISBNEmptyList(t *testing.T) {
	if got := bestISBN(nil); got != "" {
		t.Errorf("bestISBN(nil) = %q, want empty", got)
	}
}

func TestNormalizeISBN(t *testing.T) {
	cases := []struct{ in, want string }{
		{"978-0-262-01153-2", "9780262011532"},
		{"0 306 40615 x", "030640615X"},
		{"", ""},
		{"9780306406157", "9780306406157"},
	}
	for _, c := range cases {
		if got := normalizeISBN(c.in); got != c.want {
			t.Errorf("normalizeISBN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// isZeroMetadata reports whether m carries no answer at all — enrich.Metadata
// holds a slice field, so a plain == against a zero-value literal doesn't
// compile.
func isZeroMetadata(m enrich.Metadata) bool {
	return m.Title == "" && len(m.Authors) == 0 && m.Publisher == "" &&
		m.PublishedDate == "" && m.Language == "" && m.ISBN == "" && m.Description == "" &&
		m.CoverURL == ""
}

// The fixture's cover_i (240727) is what drives the URL this test expects —
// covers.openlibrary.org's documented id-based shape. The provider names
// the URL and downloads nothing: the fetch is internal/enrich's Worker's,
// so it only happens for a book that actually needs a cover.
func TestByISBNNamesCoverURLWithoutFetchingIt(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_match.json"))
	})

	coverHits := 0
	coverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coverHits++
	}))
	t.Cleanup(coverServer.Close)
	client.coverBaseURL = coverServer.URL

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	want := coverServer.URL + "/b/id/240727-L.jpg"
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want %q", got.CoverURL, want)
	}
	if coverHits != 0 {
		t.Errorf("cover server hits = %d, want 0 — a lookup must never download an image", coverHits)
	}
}

func TestByISBNNoCoverIDLeavesCoverURLEmpty(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_no_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "0000000000")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if got.CoverURL != "" {
		t.Errorf("CoverURL = %q, want empty — no match means no cover_i", got.CoverURL)
	}
}

// Open Library answers MARC three-letter codes; internal/epub, internal/fb2
// and internal/googlebooks all produce ISO 639-1. Without the mapping the
// language column holds "eng" for one book and "en" for the next.
func TestLanguageIsMappedToISO639(t *testing.T) {
	cases := []struct{ in, want string }{
		{"eng", "en"},
		{"RUS", "ru"},
		{"en", "en"},
		{"zzz", "zzz"},
	}
	for _, c := range cases {
		if got := isoLanguage(c.in); got != c.want {
			t.Errorf("isoLanguage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// publish_date wins when present; first_publish_year is the fallback for
// the many docs that carry only it, and neither leaves the field empty
// rather than inventing one.
func TestPublishedDatePrefersPublishDateThenFirstPublishYear(t *testing.T) {
	client := New()
	cases := []struct {
		name string
		doc  searchDoc
		want string
	}{
		{"publish_date wins", searchDoc{PublishDate: []string{"1996"}, FirstPublishYear: 1984}, "1996"},
		{"first_publish_year is the fallback", searchDoc{FirstPublishYear: 1984}, "1984"},
		{"neither invents nothing", searchDoc{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := client.toMetadata(c.doc).PublishedDate; got != c.want {
				t.Errorf("PublishedDate = %q, want %q", got, c.want)
			}
		})
	}
}

// A 429 or 5xx is worth another attempt; a 400 will be rejected identically
// however many times it is asked, so WithRetry must be able to tell them
// apart through enrich.ErrRetryable.
func TestErrorsAreClassifiedRetryableOrNot(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusBadRequest, false},
		{http.StatusForbidden, false},
	}
	for _, c := range cases {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		})
		_, err := client.ByISBN(context.Background(), "9780262011532")
		if err == nil {
			t.Fatalf("status %d: want an error", c.status)
		}
		if got := errors.Is(err, enrich.ErrRetryable); got != c.wantRetryable {
			t.Errorf("status %d: errors.Is(err, ErrRetryable) = %v, want %v", c.status, got, c.wantRetryable)
		}
	}
}

// Open Library's API terms ask for a descriptive User-Agent and throttle
// the generic Go default; a block would look like any other transient
// failure, so it would silently skip this provider for every book.
func TestRequestsCarryAUserAgent(t *testing.T) {
	var got string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write(readFixture(t, "search_no_match.json"))
	})
	if _, err := client.ByISBN(context.Background(), "9780262011532"); err != nil {
		t.Fatal(err)
	}
	if got != userAgent {
		t.Errorf("User-Agent = %q, want %q", got, userAgent)
	}
}
