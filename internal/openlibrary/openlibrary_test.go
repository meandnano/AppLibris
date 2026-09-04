package openlibrary

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"library/internal/enrich"
)

// The fixtures under testdata have two different provenances, and the
// difference is worth knowing when reading a failure:
//
//   - edition_match.json and edition_no_match.json are **live captures** of
//     the Read API's answers (for ISBN 9780547928227 and for an ISBN it
//     knows nothing about). They are what turned up the work-versus-edition
//     defects this endpoint exists to fix, which no hand-written fixture
//     would have shown — including that a no-match is a bare `[]`.
//   - search_match.json and search_no_match.json are shaped after
//     /search.json's documented response format rather than captured, from
//     when this package was written without outbound network access. Field
//     names and nesting match the API's stable documented shape, and the
//     work-level values they carry (a language list, a first_publish_year)
//     match what the live endpoint really answers.
//
// Neither is hand-edited to fit a change: a fixture adjusted until the code
// passes tests the parser against its author's expectations instead of
// against the API.

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

	httpClient := server.Client()
	// The production redirect policy, not net/http's default: without it
	// the redirect tests below would assert the standard library's
	// behaviour rather than checkRedirect's.
	httpClient.CheckRedirect = checkRedirect

	return &Client{
		baseURL:      server.URL,
		coverBaseURL: coverServer.URL,
		httpClient:   httpClient,
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

// The regression test for this endpoint's whole existence. The fixture is
// the Read API's real answer for an English Houghton Mifflin printing of
// The Hobbit; /search.json answers the same ISBN with the *work*, whose
// language array starts "bul" and whose date is 1937, and carries neither a
// publisher nor a description at all.
func TestByISBNMatchParsesFixture(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "edition_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}

	if got.Title != "The Hobbit" {
		t.Errorf("Title = %q", got.Title)
	}
	// Authors come from the record's data block: this edition record lists
	// no authors of its own, since authorship belongs to the work.
	wantAuthors := []string{"J.R.R. Tolkien"}
	if len(got.Authors) != len(wantAuthors) || got.Authors[0] != wantAuthors[0] {
		t.Errorf("Authors = %v, want %v", got.Authors, wantAuthors)
	}
	if got.Publisher != "Mariner Books" {
		t.Errorf("Publisher = %q, want %q — /search.json answers this ISBN with no publisher at all", got.Publisher, "Mariner Books")
	}
	if got.PublishedDate != "2012" {
		t.Errorf("PublishedDate = %q, want %q — the edition's year, not the work's 1937", got.PublishedDate, "2012")
	}
	// "/languages/eng" -> MARC "eng" -> ISO 639-1, the same form
	// internal/epub and internal/fb2 produce.
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q — not the work's 31-language list", got.Language, "en")
	}
	if got.ISBN != "9780547928227" {
		t.Errorf("ISBN = %q, want %q (the 13-digit form, normalised)", got.ISBN, "9780547928227")
	}
	if !strings.Contains(got.Description, "Bilbo Baggins") {
		t.Errorf("Description = %q, want the edition's description — /search.json answers this ISBN with none", got.Description)
	}
}

// The ISBN belongs in the path here, not a query parameter: an edit that
// quietly moved this lookup back to /search.json would still return
// plausible-looking metadata, so the endpoint itself is asserted.
func TestByISBNUsesTheEditionEndpointWithANormalisedISBN(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/api/volumes/brief/isbn/9780262011532.json"; r.URL.Path != want {
			t.Errorf("request path = %q, want %q", r.URL.Path, want)
		}
		w.Write(readFixture(t, "edition_no_match.json"))
	})

	if _, err := client.ByISBN(context.Background(), "978-0-262-01153-2"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
}

// The Read API answers an ISBN it knows nothing about with a bare `[]` — an
// array where a match is an object — so this is the no-match case *and* a
// body that a struct decode rejects outright. Reporting it as a parse
// failure would make the ordinary case for an obscure book look like a
// broken provider.
func TestByISBNArrayBodyIsNoMatchNotAParseError(t *testing.T) {
	client, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "edition_no_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "9790000000001")
	if err != nil {
		t.Fatalf("ByISBN: want nil error for the empty-array answer (the ordinary case), got %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
	if *hits != 1 {
		t.Errorf("server hits = %d, want 1", *hits)
	}
}

// An empty records object is the same answer by a different shape.
func TestByISBNEmptyRecordsIsNoMatch(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"records": {}, "items": []}`))
	})

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
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
		w.Write(readFixture(t, "edition_no_match.json"))
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

// The fixture's covers[0] (12003329) is what drives the URL this test
// expects — covers.openlibrary.org's documented id-based shape. The provider
// names the URL and downloads nothing: the fetch is internal/enrich's
// Worker's, so it only happens for a book that actually needs a cover.
func TestByISBNNamesCoverURLWithoutFetchingIt(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "edition_match.json"))
	})

	coverHits := 0
	coverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coverHits++
	}))
	t.Cleanup(coverServer.Close)
	client.coverBaseURL = coverServer.URL

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	want := coverServer.URL + "/b/id/12003329-L.jpg"
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want %q", got.CoverURL, want)
	}
	if coverHits != 0 {
		t.Errorf("cover server hits = %d, want 0 — a lookup must never download an image", coverHits)
	}
}

func TestByISBNNoCoverIDLeavesCoverURLEmpty(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "edition_no_match.json"))
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

// /search.json answers about a work, so its language and date describe
// every edition at once — the fixture's own publish_date lists 1996 and
// 1985 across 12 editions, and its first_publish_year is 1985. Reporting
// either would fill books.language and books.published_date with a value
// that is not this book's, and a filled field is one enrich.Resolve never
// offers to another provider and never asks about again. Empty is the
// answer; the fields a work genuinely does determine still come through.
func TestSearchReportsNoLanguageOrPublishedDate(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_match.json"))
	})

	got, err := client.Search(context.Background(), "Structure and Interpretation of Computer Programs", []string{"Harold Abelson"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if got.Language != "" {
		t.Errorf("Language = %q, want empty — the fixture's \"eng\" is the work's, across 12 editions", got.Language)
	}
	if got.PublishedDate != "" {
		t.Errorf("PublishedDate = %q, want empty — the fixture offers only the work's 1985/1996", got.PublishedDate)
	}
	if got.Title != "Structure and Interpretation of Computer Programs" {
		t.Errorf("Title = %q — a work does determine its title", got.Title)
	}
	if len(got.Authors) != 2 {
		t.Errorf("Authors = %v, want both — a work does determine its authors", got.Authors)
	}
	if got.Publisher != "MIT Press" {
		t.Errorf("Publisher = %q, want %q", got.Publisher, "MIT Press")
	}
}

// Both shapes occur in Open Library's data model. A decode that expected
// only the object would drop every string-shaped description silently.
func TestDescriptionParsesFromBothShapes(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"object shape", `{"type": "/type/text", "value": " from an object "}`, "from an object"},
		{"bare string shape", `" from a string "`, "from a string"},
		{"absent", ``, ""},
		{"null", `null`, ""},
		{"unexpected shape", `12`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := textValue(json.RawMessage(c.raw)); got != c.want {
				t.Errorf("textValue(%s) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// Authorship belongs to the work, so an edition record usually lists author
// references or nothing — but some records do carry names, and that shape
// was observed live. It is the fallback, not the primary source.
func TestByISBNAuthorsFallBackToTheEditionRecord(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"records": {"/books/OL1M": {
			"data": {"title": "Retrospectiva"},
			"details": {"details": {
				"title": "Retrospectiva",
				"authors": [{"key": "/authors/OL293062A", "name": "Rómulo Macció"}]
			}}
		}}}`))
	})

	got, err := client.ByISBN(context.Background(), "9999999999999")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if len(got.Authors) != 1 || got.Authors[0] != "Rómulo Macció" {
		t.Errorf("Authors = %v, want the edition record's own", got.Authors)
	}
}

// The ISBN is an alias for the canonical edition key, so a redirect is a
// normal answer and has to be followed — but every hop is chosen by
// whatever host answered, not by this package, so each one's scheme is
// checked rather than only the first URL's.
func TestByISBNFollowsARedirect(t *testing.T) {
	client, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/volumes/brief/isbn/9780547928227.json" {
			http.Redirect(w, r, "/canonical.json", http.StatusFound)
			return
		}
		w.Write(readFixture(t, "edition_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if got.Title != "The Hobbit" {
		t.Errorf("Title = %q, want the redirect target's record", got.Title)
	}
	if *hits != 2 {
		t.Errorf("server hits = %d, want 2 (the alias and the target)", *hits)
	}
}

func TestByISBNRefusesANonHTTPRedirect(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})

	if _, err := client.ByISBN(context.Background(), "9780547928227"); err == nil {
		t.Fatal("ByISBN: want error on a redirect to a non-http(s) scheme, got nil")
	}
}

func TestByISBNStopsAnEndlessRedirectChain(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next.json", http.StatusFound)
	})

	if _, err := client.ByISBN(context.Background(), "9780547928227"); err == nil {
		t.Fatal("ByISBN: want error once the redirect budget is spent, got nil")
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
