package googlebooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"library/internal/enrich"
)

// The fixtures under testdata are shaped after the Google Books Volumes
// API's documented response format
// (https://developers.google.com/books/docs/v1/using) rather than a live
// capture: this sandbox has no outbound network access, so a real request
// can't be made from here. Field names and nesting match the API's stable,
// publicly documented shape. volumes_match.json omits imageLinks
// deliberately — a real one is an absolute URL at a live Google host, and
// this sandbox has no outbound network access, so any test that exercises
// a cover fetch builds its own response inline with imageLinks.thumbnail
// pointing at a local httptest.Server instead of reading it from a static
// file — a fixture can't bake in a server address chosen at test run time.

func testClient(t *testing.T, apiKey string, handler http.HandlerFunc) (*Client, *int) {
	t.Helper()
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return &Client{
		baseURL:    server.URL,
		apiKey:     apiKey,
		httpClient: server.Client(),
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
	c := New("")
	if c.httpClient.Timeout != Timeout {
		t.Errorf("httpClient.Timeout = %v, want Timeout (%v); http.DefaultClient has none at all", c.httpClient.Timeout, Timeout)
	}
}

func TestName(t *testing.T) {
	if got := New("").Name(); got != "googlebooks" {
		t.Errorf("Name() = %q, want %q", got, "googlebooks")
	}
}

func TestByISBNMatchParsesFixture(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_match.json"))
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
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q", got.Language, "en")
	}
	if got.ISBN != "9780262011532" {
		t.Errorf("ISBN = %q, want %q (the 13-digit form, normalised)", got.ISBN, "9780262011532")
	}
	// The Volumes API documents description as HTML-formatted, and nothing
	// downstream renders markup — html/template escapes the detail page's
	// description, so a tag left in shows up literally.
	wantDescription := "Structure and Interpretation of Computer Programs has had a dramatic " +
		"impact on computer science curricula over the past decade.\n\n" +
		"It is used at MIT & elsewhere."
	if got.Description != wantDescription {
		t.Errorf("Description = %q, want %q", got.Description, wantDescription)
	}
}

func TestPlainText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain text is untouched", "A plain synopsis.", "A plain synopsis."},
		{"inline tags are dropped", "A <b>bold</b> and <i>italic</i> claim.", "A bold and italic claim."},
		{"br becomes a line break", "First line.<br>Second line.", "First line.\nSecond line."},
		{"self-closing br becomes a line break", "First.<br/>Second.", "First.\nSecond."},
		{"paragraphs are separated by a blank line", "<p>One.</p><p>Two.</p>", "One.\n\nTwo."},
		{"entities are unescaped", "Salt &amp; pepper &mdash; a pair.", "Salt & pepper — a pair."},
		{
			"escaped markup survives as text",
			"Use &lt;b&gt; for bold.",
			"Use <b> for bold.",
		},
		{"a bare less-than is not a tag", "a < b and c > d", "a < b and c > d"},
		{"an unterminated tag is not stripped", "ends with <p", "ends with <p"},
		{"attributes are dropped with their tag", `<a href="http://x/">link</a>`, "link"},
		{"surrounding whitespace is trimmed", "<p>  Trimmed.  </p>", "Trimmed."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plainText(tt.raw); got != tt.want {
				t.Errorf("plainText(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestByISBNNormalisesRequestISBN(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "isbn:9780262011532" {
			t.Errorf("request q = %q, want %q", got, "isbn:9780262011532")
		}
		w.Write(readFixture(t, "volumes_no_match.json"))
	})

	if _, err := client.ByISBN(context.Background(), "978-0-262-01153-2"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
}

func TestByISBNNoMatchIsNotAnError(t *testing.T) {
	client, hits := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_no_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "0000000000")
	if err != nil {
		t.Fatalf("ByISBN: want nil error for a 200 with no items (the ordinary case), got %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
	if *hits != 1 {
		t.Errorf("server hits = %d, want 1", *hits)
	}
}

func TestByISBN404IsNotAnError(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
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
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := client.ByISBN(context.Background(), "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on 429, got nil")
	}
}

func TestByISBN5xxIsAnError(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := client.ByISBN(context.Background(), "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on 503, got nil")
	}
}

func TestByISBNTransportErrorIsAnError(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
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
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not valid json"))
	})

	_, err := client.ByISBN(context.Background(), "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on malformed body, got nil")
	}
}

func TestByISBNEmptyISBNNeverCallsServer(t *testing.T) {
	client, hits := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_no_match.json"))
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
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, `intitle:"Structure and Interpretation of Computer Programs"`) {
			t.Errorf("q = %q, want it to contain intitle:...", q)
		}
		if !strings.Contains(q, `inauthor:"Harold Abelson"`) {
			t.Errorf("q = %q, want it to contain inauthor:...", q)
		}
		w.Write(readFixture(t, "volumes_match.json"))
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
	client, hits := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_no_match.json"))
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
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_no_match.json"))
	})

	got, err := client.Search(context.Background(), "Some Obscure Title Nobody Wrote About", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !isZeroMetadata(got) {
		t.Errorf("Metadata = %+v, want zero value", got)
	}
}

func TestRequestCarriesKeyWhenConfigured(t *testing.T) {
	client, _ := testClient(t, "test-api-key", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "test-api-key" {
			t.Errorf("request key = %q, want %q", got, "test-api-key")
		}
		w.Write(readFixture(t, "volumes_no_match.json"))
	})

	if _, err := client.ByISBN(context.Background(), "9780262011532"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
}

func TestRequestStillMadeWithoutKey(t *testing.T) {
	client, hits := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "" {
			t.Errorf("request key = %q, want empty (no key configured)", got)
		}
		w.Write(readFixture(t, "volumes_no_match.json"))
	})

	if _, err := client.ByISBN(context.Background(), "9780262011532"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if *hits != 1 {
		t.Errorf("server hits = %d, want 1 — a missing key must not stop the request", *hits)
	}
}

func TestAPIKeyNeverAppearsInErrorText(t *testing.T) {
	client, _ := testClient(t, "super-secret-key", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("upstream error, key=super-secret-key rejected"))
	})

	_, err := client.ByISBN(context.Background(), "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on 503, got nil")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

func TestAPIKeyNeverAppearsInTransportErrorText(t *testing.T) {
	client, _ := testClient(t, "super-secret-key", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := client.ByISBN(ctx, "9780262011532")
	if err == nil {
		t.Fatal("ByISBN: want error on a timed-out request, got nil")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

func TestBestISBNPrefersISBN13Type(t *testing.T) {
	got := bestISBN([]industryIdentifier{
		{Type: "ISBN_10", Identifier: "0262011530"},
		{Type: "ISBN_13", Identifier: "9780262011532"},
	})
	if got != "9780262011532" {
		t.Errorf("bestISBN = %q, want %q", got, "9780262011532")
	}
}

func TestBestISBNFallsBackToISBN10Type(t *testing.T) {
	got := bestISBN([]industryIdentifier{
		{Type: "ISBN_10", Identifier: "0262011530"},
	})
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

// The provider names the cover's URL and downloads nothing: the fetch is
// internal/enrich's Worker's, so it only happens for a book that actually
// needs a cover.
func TestByISBNNamesCoverURLWithoutFetchingIt(t *testing.T) {
	coverHits := 0
	coverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coverHits++
	}))
	t.Cleanup(coverServer.Close)

	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf(`{"totalItems":1,"items":[{"volumeInfo":{
			"title":"Structure and Interpretation of Computer Programs",
			"imageLinks":{"thumbnail":%q}
		}}]}`, coverServer.URL+"/cover.jpg")
		w.Write([]byte(body))
	})

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	// best() upgrades http to https; the test server only speaks http, so
	// the expectation is the upgraded form rather than the URL as served.
	want := strings.Replace(coverServer.URL+"/cover.jpg", "http://", "https://", 1)
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want %q", got.CoverURL, want)
	}
	if coverHits != 0 {
		t.Errorf("cover server hits = %d, want 0 — a lookup must never download an image", coverHits)
	}
}

func TestByISBNNoImageLinksLeavesCoverURLEmpty(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780262011532")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if got.CoverURL != "" {
		t.Errorf("CoverURL = %q, want empty — volumes_match.json carries no imageLinks", got.CoverURL)
	}
}

// thumbnail is ~128px wide, under internal/cover's 400px target, which
// never upscales — so a larger link wins when the volume offers one. The
// http URLs Google answers with are upgraded to https, since these bytes
// end up served from /covers/.
func TestCoverURLPrefersTheLargestLinkAndUpgradesToHTTPS(t *testing.T) {
	cases := []struct {
		name  string
		links imageLinks
		want  string
	}{
		{
			name:  "largest wins",
			links: imageLinks{Large: "https://books.example/large", Medium: "https://books.example/medium", Thumbnail: "https://books.example/thumb"},
			want:  "https://books.example/large",
		},
		{
			name:  "thumbnail is the fallback",
			links: imageLinks{Thumbnail: "https://books.example/thumb"},
			want:  "https://books.example/thumb",
		},
		{
			name:  "http is upgraded",
			links: imageLinks{Thumbnail: "http://books.google.com/books/content?id=1"},
			want:  "https://books.google.com/books/content?id=1",
		},
		{name: "no links at all", links: imageLinks{}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.links.best(); got != c.want {
				t.Errorf("best() = %q, want %q", got, c.want)
			}
		})
	}
}

// The Volumes API binds intitle: to the single token after it, so an
// unquoted multi-word title constrains only its first word and lets the
// rest drift into free-text terms — matching some other book whose
// metadata is then written under this one's provenance.
func TestSearchQuotesItsQualifiers(t *testing.T) {
	var gotQuery string
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Write(readFixture(t, "volumes_no_match.json"))
	})

	if _, err := client.Search(context.Background(), `Structure and "Interpretation"`, []string{"Harold Abelson"}); err != nil {
		t.Fatal(err)
	}
	want := `intitle:"Structure and Interpretation" inauthor:"Harold Abelson"`
	if gotQuery != want {
		t.Errorf("q = %q, want %q", gotQuery, want)
	}
}

// A 429 or 5xx is worth another attempt; a 403 (Google's over-quota and
// rejected-key answer) and a 400 will be rejected identically however many
// times they are asked, so WithRetry must be able to tell them apart
// through enrich.ErrRetryable.
func TestErrorsAreClassifiedRetryableOrNot(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusBadRequest, false},
		{http.StatusForbidden, false},
	}
	for _, c := range cases {
		client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
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

// Redaction must not cost the error chain: without Unwrap, whether
// context.Canceled were detectable on a transport failure would depend on
// whether an API key happened to be configured.
func TestRedactedTransportErrorStillUnwraps(t *testing.T) {
	client, _ := testClient(t, "secret-key", func(w http.ResponseWriter, r *http.Request) {})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.ByISBN(ctx, "9780262011532")
	if err == nil {
		t.Fatal("want an error from a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false for %v", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

func TestRequestsCarryAUserAgent(t *testing.T) {
	var got string
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write(readFixture(t, "volumes_no_match.json"))
	})
	if _, err := client.ByISBN(context.Background(), "9780262011532"); err != nil {
		t.Fatal(err)
	}
	if got != userAgent {
		t.Errorf("User-Agent = %q, want %q", got, userAgent)
	}
}
