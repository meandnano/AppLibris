package openlibrary

import (
	"context"
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
	if got.Language != "eng" {
		t.Errorf("Language = %q, want %q", got.Language, "eng")
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
		len(m.Cover) == 0
}

// The fixture's cover_i (240727) is what drives the URL this test's cover
// server expects — covers.openlibrary.org's documented id-based shape.
func TestByISBNFetchesCoverWhenPresent(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_match.json"))
	})

	wantCover := []byte("fake-jpeg-bytes")
	coverHits := 0
	coverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coverHits++
		if r.URL.Path != "/b/id/240727-L.jpg" {
			t.Errorf("cover request path = %q, want /b/id/240727-L.jpg", r.URL.Path)
		}
		w.Write(wantCover)
	}))
	t.Cleanup(coverServer.Close)
	client.coverBaseURL = coverServer.URL

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if string(got.Cover) != string(wantCover) {
		t.Errorf("Cover = %q, want %q", got.Cover, wantCover)
	}
	if coverHits != 1 {
		t.Errorf("cover server hits = %d, want 1", coverHits)
	}
}

// A cover fetch failure must not fail the whole lookup — the rest of the
// fixture's fields are still a good answer.
func TestByISBNCoverFetchFailureDoesNotFailTheLookup(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_match.json"))
	})
	// client.coverBaseURL already points at testClient's default 404
	// server, so no override is needed here.

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: want nil error when only the cover fetch fails, got %v", err)
	}
	if got.Title == "" {
		t.Error("Title is empty — a failed cover fetch must not lose the rest of the answer")
	}
	if got.Cover != nil {
		t.Errorf("Cover = %q, want nil", got.Cover)
	}
}

func TestByISBNNoCoverIDNeverCallsCoverServer(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "search_no_match.json"))
	})

	coverHits := 0
	coverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coverHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(coverServer.Close)
	client.coverBaseURL = coverServer.URL

	if _, err := client.ByISBN(context.Background(), "0000000000"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if coverHits != 0 {
		t.Errorf("cover server hits = %d, want 0 — no match means no cover_i to fetch", coverHits)
	}
}
