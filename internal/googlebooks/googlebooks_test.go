package googlebooks

import (
	"context"
	"encoding/json"
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

// Every fixture under testdata is a **live capture** of the Volumes API,
// taken with a real GOOGLE_BOOKS_API_KEY on 2026-09-06 (see
// docs/plans/completed/ for the verification that took them). None is
// hand-shaped from the documentation, and none is hand-edited to fit a
// change: a fixture adjusted until the code passes tests the parser
// against its author's expectations instead of against the API, which is
// how internal/openlibrary shipped a Bulgarian language for an English
// book with every test green.
//
//   - volumes_match.json — GET /volumes?q=isbn:9780547928227&maxResults=1
//   - volumes_search_match.json — the intitle:/inauthor: fallback's shape
//   - volumes_no_match.json — an ISBN the API knows nothing about
//   - volumes_detail.json — GET /volumes/{id}, the *other* endpoint, kept
//     as the evidence that the larger imageLinks sizes live only there
//   - volumes_regional_language.json — a pt-BR volume, the language tag
//     Google really answers with
//   - volumes_wrong_edition.json — a Portuguese-titled volume Google
//     labels "en"
//   - error_400_bad_key.json, error_429_per_day.json,
//     error_429_per_minute.json — the three failures the live check
//     actually provoked, which is what settled the retry classification
//
// A cover fetch still builds its response inline with imageLinks.thumbnail
// pointing at a local httptest.Server: a fixture cannot bake in a server
// address chosen at test run time.

// testClient serves handler at the list endpoint (/volumes) and 404s the
// single-volume one (/volumes/{id}), so a test written about a search can
// never be handed the detail request enrichVolume makes for a matched
// volume — which would fail its query assertions and inflate its hit
// count. internal/openlibrary's own testClient isolates its cover host for
// the same reason. Tests about the detail request use detailClient below.
//
// hits counts list requests only, for the same reason: a test asserting
// "one request" means one lookup.
func testClient(t *testing.T, apiKey string, handler http.HandlerFunc) (*Client, *int) {
	t.Helper()
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isVolumeDetailPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
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

// detailClient serves list at /volumes and detail at /volumes/{id},
// counting the detail requests — the split testClient refuses, for tests
// that are about enrichVolume itself.
func detailClient(t *testing.T, list, detail http.HandlerFunc) (*Client, *int) {
	t.Helper()
	detailHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isVolumeDetailPath(r.URL.Path) {
			detailHits++
			detail(w, r)
			return
		}
		list(w, r)
	}))
	t.Cleanup(server.Close)

	return &Client{
		baseURL:    server.URL,
		apiKey:     "",
		httpClient: server.Client(),
	}, &detailHits
}

// isVolumeDetailPath reports whether p addresses one volume rather than the
// list endpoint — "/volumes/LLSpngEACAAJ" rather than "/volumes".
func isVolumeDetailPath(p string) bool {
	return strings.HasPrefix(p, "/volumes/")
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

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}

	if got.Title != "The Hobbit, Or, There and Back Again" {
		t.Errorf("Title = %q", got.Title)
	}
	wantAuthors := []string{"J. R. R. Tolkien"}
	if len(got.Authors) != len(wantAuthors) || got.Authors[0] != wantAuthors[0] {
		t.Errorf("Authors = %v, want %v", got.Authors, wantAuthors)
	}
	if got.Publisher != "Mariner Books" {
		t.Errorf("Publisher = %q, want %q", got.Publisher, "Mariner Books")
	}
	// The edition's year, not the work's. Open Library's search endpoint
	// answered 1937 for this same ISBN, which is what
	// docs/plans/completed/2026090401 moved it off search.json to fix; the
	// Volumes API returns one volume per edition, so it does not have that
	// failure mode.
	if got.PublishedDate != "2012" {
		t.Errorf("PublishedDate = %q, want %q — the 2012 Mariner edition, not the work's 1937", got.PublishedDate, "2012")
	}
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q", got.Language, "en")
	}
	if got.ISBN != "9780547928227" {
		t.Errorf("ISBN = %q, want %q (the 13-digit form, normalised)", got.ISBN, "9780547928227")
	}
	// The list endpoint's description arrives with markup already
	// stripped — see TestListEndpointDescriptionCarriesNoMarkup.
	wantDescription := "Celebrating 75 years of one of the world's most treasured classics with an " +
		"all new trade paperback edition. Repackaged with new cover art. 500,000 first printing."
	if got.Description != wantDescription {
		t.Errorf("Description = %q, want %q", got.Description, wantDescription)
	}
}

func TestSearchMatchParsesFixture(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_search_match.json"))
	})

	got, err := client.Search(context.Background(), "Pride and Prejudice", []string{"Jane Austen"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if got.Title != "Pride and Prejudice" {
		t.Errorf("Title = %q", got.Title)
	}
	// Unlike Open Library's /search.json, which describes a work and so
	// cannot answer either of these for one edition, the Volumes API
	// answers the search path with a volume — one edition — so language
	// and published date are the edition's on this path too.
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q", got.Language, "en")
	}
	if got.PublishedDate == "" {
		t.Error("PublishedDate is empty — the search path answers an edition, so it has one")
	}
	if got.ISBN == "" {
		t.Error("ISBN is empty — the search path answers an edition, so it has one")
	}
}

// The capture is what the client's own request shape (GET /volumes?q=…)
// really answers, and it carries only the two smallest sizes. small
// through extraLarge exist only on the single-volume endpoint
// (volumes_detail.json, the same volume, five sizes) — which is the whole
// reason enrichVolume makes a second request, and the reason it must not
// be "optimised" away.
func TestListEndpointOffersOnlyTheThumbnailSizes(t *testing.T) {
	var parsed volumesResponse
	if err := json.Unmarshal(readFixture(t, "volumes_match.json"), &parsed); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	links := parsed.Items[0].VolumeInfo.ImageLinks
	if links.Thumbnail == "" {
		t.Fatal("the capture carries no thumbnail")
	}
	if links.Large != "" || links.Medium != "" || links.Small != "" {
		t.Errorf("the list endpoint answered a size above thumbnail: %+v", links)
	}

	var detail volume
	if err := json.Unmarshal(readFixture(t, "volumes_detail.json"), &detail); err != nil {
		t.Fatalf("unmarshal detail fixture: %v", err)
	}
	if detail.VolumeInfo.ImageLinks.Large == "" {
		t.Error("volumes_detail.json carries no large link — the capture no longer shows the contrast it exists for")
	}
}

// The Volumes API documents volumeInfo.description as HTML-formatted, and
// plainText renders it — but that documentation describes the
// single-volume endpoint. The list endpoint this client calls strips the
// markup itself (and truncates), so plainText is a no-op on every real
// answer this client receives. Both captures of the same volume are here
// to show it, since it is the kind of claim that reads as settled and is
// not.
func TestListEndpointDescriptionCarriesNoMarkup(t *testing.T) {
	var list volumesResponse
	if err := json.Unmarshal(readFixture(t, "volumes_match.json"), &list); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if strings.ContainsAny(list.Items[0].VolumeInfo.Description, "<>") {
		t.Errorf("the list endpoint answered markup: %q", list.Items[0].VolumeInfo.Description)
	}

	var detail volume
	if err := json.Unmarshal(readFixture(t, "volumes_detail.json"), &detail); err != nil {
		t.Fatalf("unmarshal detail fixture: %v", err)
	}
	if !strings.Contains(detail.VolumeInfo.Description, "<p>") {
		t.Error("volumes_detail.json carries no markup — the capture no longer shows the contrast it exists for")
	}
}

// totalItems is an estimate, not a count: the same isbn: query answers 300
// at maxResults=1 and 1 at maxResults=5. Nothing reads it, and this pins
// that — a no-match test written against totalItems would pass on a
// response that has items.
func TestNoMatchIsDecidedByItemsNotTotalItems(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"kind":"books#volumes","totalItems":0,"items":[{"volumeInfo":{"title":"Present anyway"}}]}`))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if got.Title != "Present anyway" {
		t.Errorf("Title = %q — an item present under totalItems 0 is still a match", got.Title)
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

func TestNoImageLinksLeavesCoverURLEmpty(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"totalItems":1,"items":[{"volumeInfo":{"title":"No cover here"}}]}`))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if got.CoverURL != "" {
		t.Errorf("CoverURL = %q, want empty — the volume carries no imageLinks", got.CoverURL)
	}
}

// The capture's own cover URL, end to end: http as Google answers it,
// upgraded, and the zoom=1 thumbnail — 128x192 when fetched, which
// internal/cover stores as-is since it never upscales past its 400px
// target.
func TestByISBNCoverURLFromCaptureIsTheUpgradedThumbnail(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_match.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	want := "https://books.google.com/books/content?id=LLSpngEACAAJ&printsec=frontcover&img=1&zoom=1&source=gbs_api"
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want %q", got.CoverURL, want)
	}
}

// thumbnail is ~195px on the long edge, under internal/cover's 400px
// target, which never upscales — so a larger link wins when the volume
// offers one. But *not* the largest available: medium and large both clear
// the target, so the choice between them only decides how many pixels get
// thrown away, and extraLarge (~2670px, up to 800 KB) is past
// enrich.MaxCoverBytes, where the cover is refused outright rather than
// downsized. The http URLs Google answers with are upgraded to https,
// since these bytes end up served from /covers/.
func TestCoverURLPrefersTheSmallestLinkOverTheTargetAndUpgradesToHTTPS(t *testing.T) {
	cases := []struct {
		name  string
		links imageLinks
		want  string
	}{
		{
			name:  "medium beats large: both clear the 400px target, medium costs fewer bytes",
			links: imageLinks{Large: "https://books.example/large", Medium: "https://books.example/medium", Small: "https://books.example/small", Thumbnail: "https://books.example/thumb"},
			want:  "https://books.example/medium",
		},
		{
			name:  "large when there is no medium",
			links: imageLinks{Large: "https://books.example/large", Small: "https://books.example/small", Thumbnail: "https://books.example/thumb"},
			want:  "https://books.example/large",
		},
		{
			name:  "small beats thumbnail below the target",
			links: imageLinks{Small: "https://books.example/small", Thumbnail: "https://books.example/thumb"},
			want:  "https://books.example/small",
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

// extraLarge is not in imageLinks at all, so a response carrying one is
// ignored rather than preferred. It is the only observed size past
// enrich.MaxCoverBytes (512 KiB), and past that a cover is refused
// outright — the failure mode this ordering exists to avoid is a *better*
// cover becoming no cover.
func TestExtraLargeIsIgnored(t *testing.T) {
	body := `{"totalItems":1,"items":[{"id":"abc","volumeInfo":{"title":"T","imageLinks":{
		"extraLarge":"https://books.example/xl","medium":"https://books.example/medium"}}}]}`

	client, _ := detailClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) },
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"id":"abc","volumeInfo":{}}`)) })

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if got.CoverURL != "https://books.example/medium" {
		t.Errorf("CoverURL = %q, want the medium link — extraLarge must not win", got.CoverURL)
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

// The three failures the live check actually provoked, replayed with their
// real bodies. They are here because the classification they decide was
// written from the documentation and got its premise backwards: the
// package comment reasoned that "a 403 is Google's over-quota and
// rejected-key answer", and neither is. A rejected key is 400 and
// over-quota is 429, on both the per-day and the per-minute limit; no
// probe produced a 403 at all.
func TestLiveErrorBodiesAreClassified(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		fixture     string
		retryable   bool
		bodyExcerpt string
	}{
		{
			name:        "a rejected key is 400 and is not worth asking twice",
			status:      http.StatusBadRequest,
			fixture:     "error_400_bad_key.json",
			retryable:   false,
			bodyExcerpt: "API key not valid",
		},
		{
			name:        "an exhausted per-day quota is 429",
			status:      http.StatusTooManyRequests,
			fixture:     "error_429_per_day.json",
			retryable:   true,
			bodyExcerpt: "Queries per day",
		},
		{
			name:        "ordinary per-minute throttling is 429 too",
			status:      http.StatusTooManyRequests,
			fixture:     "error_429_per_minute.json",
			retryable:   true,
			bodyExcerpt: "Queries per minute per user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write(readFixture(t, tt.fixture))
			})

			_, err := client.ByISBN(context.Background(), "9780547928227")
			if err == nil {
				t.Fatalf("ByISBN: want an error on %d", tt.status)
			}
			if got := errors.Is(err, enrich.ErrRetryable); got != tt.retryable {
				t.Errorf("errors.Is(err, ErrRetryable) = %v, want %v: %v", got, tt.retryable, err)
			}
			// The body reaches the error text: Google's message names
			// which limit was hit, which is the only place that
			// distinction exists — there is no Retry-After header on
			// either 429.
			if !strings.Contains(err.Error(), tt.bodyExcerpt) {
				t.Errorf("error text does not carry %q: %v", tt.bodyExcerpt, err)
			}
		})
	}
}

// maxErrorBodyBytes truncates, and Google's real 429 body is longer than
// it — so the part of the message that names the limit has to survive the
// cut for the error to be worth reading at all.
func TestLiveQuotaBodyNamesItsLimitWithinTheErrorBodyCap(t *testing.T) {
	for _, fixture := range []string{"error_429_per_day.json", "error_429_per_minute.json"} {
		body := readFixture(t, fixture)
		if len(body) <= maxErrorBodyBytes {
			t.Errorf("%s is %d bytes, no longer over the %d-byte cap this test exists for", fixture, len(body), maxErrorBodyBytes)
		}
		if !strings.Contains(string(body[:maxErrorBodyBytes]), "Quota exceeded for quota metric") {
			t.Errorf("%s: the quota message falls outside the first %d bytes", fixture, maxErrorBodyBytes)
		}
	}
}

// volumeInfo.language is a BCP-47 tag, not an ISO 639-1 code: a scan of
// 188 live volumes returned pt-BR 50 times and zh-CN 32, alongside plain
// en/ru/ja/sv. Left as answered, books.language would hold "pt" for a book
// internal/openlibrary filled and "pt-BR" for the next one this provider
// did — the one-column-two-vocabularies split marcToISO639 exists to
// prevent.
func TestRegionalLanguageTagIsReducedToItsBaseSubtag(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_regional_language.json"))
	})

	got, err := client.Search(context.Background(), "O Hobbit", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.Language != "pt" {
		t.Errorf("Language = %q, want %q — the capture answers pt-BR", got.Language, "pt")
	}
}

func TestBaseLanguage(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pt-BR", "pt"},
		{"zh-CN", "zh"},
		{"en", "en"},
		{"", ""},
		{"PT-br", "pt"},
		{"  en  ", "en"},
		// A three-letter primary subtag is passed through rather than
		// guessed at, the same choice marcToISO639 makes for a code it
		// does not know: no observed value is one, and a wrong code reads
		// as answered where an unfamiliar one reads as unfamiliar.
		{"haw", "haw"},
	}
	for _, c := range cases {
		if got := baseLanguage(c.in); got != c.want {
			t.Errorf("baseLanguage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A volume Google has correctly identified and mislabelled: titled in
// Portuguese, credited to Paulo Coelho, language "en". A thin
// OCLC-derived stub — one OCLC identifier, no publisher, no description.
//
// enrich.plausibleMatch compares title and author, so it accepts this and
// "en" reaches books.language. That is a limit of any title/author gate
// rather than a fault in one: the gate refuses the cross-language
// mismatches it can see (a transliterated title is a title mismatch), and
// withholding language from search answers would cost four correct values
// to avoid this one. Recorded in
// docs/backlog/2026090609-provider-language-can-be-wrong.md and
// deliberately not fixed.
//
// The capture lives here because this package's lookup produced it.
func TestSearchCanAnswerAMislabelledLanguage(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_wrong_edition.json"))
	})

	got, err := client.Search(context.Background(), "O Alquimista", []string{"Paulo Coelho"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.Title != "O Alquimista" || len(got.Authors) != 1 || got.Authors[0] != "Paulo Coelho" {
		t.Fatalf("the capture no longer shows an exact title and author match: %q / %v", got.Title, got.Authors)
	}
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q — the capture's own value, and the point of the capture", got.Language, "en")
	}
}

// listThenDetail serves volumes_match.json at the list endpoint and
// volumes_detail.json at the single-volume one — the two captures of
// different volumes, which is fine and deliberate: what these tests are
// about is that the second response's larger cover and HTML description
// replace the first's, not that the two describe one book.
func listThenDetail(t *testing.T, detail http.HandlerFunc) (*Client, *int) {
	t.Helper()
	return detailClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write(readFixture(t, "volumes_match.json")) },
		detail)
}

// The whole point of the second request: the list endpoint's thumbnail is
// 128x192, under internal/cover's 400px target, and the sizes worth having
// live only on /volumes/{id}.
func TestMatchedVolumePrefersTheDetailEndpointsLargerCover(t *testing.T) {
	client, detailHits := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/volumes/LLSpngEACAAJ" {
			t.Errorf("detail path = %q, want the matched volume's id", r.URL.Path)
		}
		w.Write(readFixture(t, "volumes_detail.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if *detailHits != 1 {
		t.Fatalf("detail requests = %d, want 1", *detailHits)
	}

	var detail volume
	if err := json.Unmarshal(readFixture(t, "volumes_detail.json"), &detail); err != nil {
		t.Fatalf("unmarshal detail fixture: %v", err)
	}
	// medium, not large: both clear cover.Store's 400px target, so the
	// smaller one is chosen. The https upgrade applies to the detail
	// endpoint's links too — the capture answers them as plain http,
	// exactly as the list one does.
	if !strings.HasPrefix(detail.VolumeInfo.ImageLinks.Medium, "http://") {
		t.Error("volumes_detail.json no longer answers http, so this test no longer checks the upgrade")
	}
	want := strings.Replace(detail.VolumeInfo.ImageLinks.Medium, "http://", "https://", 1)
	if want == "" {
		t.Fatal("volumes_detail.json carries no medium link")
	}
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want the detail endpoint's medium link %q", got.CoverURL, want)
	}
	if strings.Contains(got.CoverURL, "zoom=1") {
		t.Error("CoverURL is still the 128px list thumbnail")
	}
}

// The detail endpoint's description is the documented HTML one, so
// plainText renders it and the paragraph breaks the list endpoint flattens
// to spaces come back.
func TestMatchedVolumePrefersTheDetailEndpointsDescription(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_detail.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if !strings.Contains(got.Description, "\n\n") {
		t.Errorf("Description carries no paragraph break, so the detail endpoint's markup was not used: %q", got.Description)
	}
	if strings.ContainsAny(got.Description, "<>") {
		t.Errorf("Description still carries markup: %q", got.Description)
	}
	if strings.Contains(got.Description, "Celebrating 75 years") {
		t.Error("Description is still the list endpoint's")
	}
}

// A bigger cover and a paragraph break are niceties. Six good text fields
// are the answer, and no failure of the second request may cost them.
func TestDetailRequestFailureKeepsTheListAnswer(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) { time.Sleep(50 * time.Millisecond) }
	tests := []struct {
		name   string
		detail http.HandlerFunc
		ctx    func(t *testing.T) context.Context
	}{
		{
			name:   "server error",
			detail: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		},
		{
			name:   "not found",
			detail: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) },
		},
		{
			name:   "malformed body",
			detail: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{not valid json")) },
		},
		{
			name:   "timeout",
			detail: slow,
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := listThenDetail(t, tt.detail)
			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx(t)
			}

			got, err := client.ByISBN(ctx, "9780547928227")
			if err != nil {
				t.Fatalf("ByISBN: want nil error — a lost cover must not fail a lookup that has its text fields; got %v", err)
			}
			if got.Title != "The Hobbit, Or, There and Back Again" {
				t.Errorf("Title = %q, want the list answer intact", got.Title)
			}
			if got.Publisher != "Mariner Books" || got.ISBN != "9780547928227" || got.Language != "en" {
				t.Errorf("the list answer's fields did not survive: %+v", got)
			}
			want := "https://books.google.com/books/content?id=LLSpngEACAAJ&printsec=frontcover&img=1&zoom=1&source=gbs_api"
			if got.CoverURL != want {
				t.Errorf("CoverURL = %q, want the list thumbnail %q", got.CoverURL, want)
			}
			if !strings.Contains(got.Description, "Celebrating 75 years") {
				t.Errorf("Description = %q, want the list answer's", got.Description)
			}
		})
	}
}

// The two conditions that make the second request pointless before it is
// made: nothing to address, or nothing to improve on.
func TestDetailRequestIsSkippedWhenItCouldNotHelp(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "no volume id to address",
			body: `{"totalItems":1,"items":[{"volumeInfo":{"title":"No id here","imageLinks":{"thumbnail":"http://books.example/t.jpg"}}}]}`,
		},
		{
			// A volume with no cover at all has no larger one either.
			name: "no cover to improve on",
			body: `{"totalItems":1,"items":[{"id":"abc123","volumeInfo":{"title":"No cover here"}}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, detailHits := detailClient(t,
				func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tt.body)) },
				func(w http.ResponseWriter, r *http.Request) {
					t.Error("the detail endpoint was called")
					w.Write(readFixture(t, "volumes_detail.json"))
				})

			if _, err := client.ByISBN(context.Background(), "9780547928227"); err != nil {
				t.Fatalf("ByISBN: %v", err)
			}
			if *detailHits != 0 {
				t.Errorf("detail requests = %d, want 0", *detailHits)
			}
		})
	}
}

// A no-match costs one request, not two: there is no volume to ask about.
func TestNoMatchMakesNoDetailRequest(t *testing.T) {
	client, detailHits := detailClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write(readFixture(t, "volumes_no_match.json")) },
		func(w http.ResponseWriter, r *http.Request) { t.Error("the detail endpoint was called for a no-match") })

	if _, err := client.ByISBN(context.Background(), "9999999999999"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if *detailHits != 0 {
		t.Errorf("detail requests = %d, want 0", *detailHits)
	}
}

// An empty detail description leaves the list one alone — the detail
// endpoint answers "" for plenty of volumes, and replacing a real
// description with nothing would be a regression bought with a request.
func TestEmptyDetailDescriptionKeepsTheListOne(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"LLSpngEACAAJ","volumeInfo":{"imageLinks":{"large":"https://books.example/large.jpg"}}}`))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if !strings.Contains(got.Description, "Celebrating 75 years") {
		t.Errorf("Description = %q, want the list answer's", got.Description)
	}
	if got.CoverURL != "https://books.example/large.jpg" {
		t.Errorf("CoverURL = %q, want the detail endpoint's only link", got.CoverURL)
	}
}

// The key travels on the detail request too — it is the same API and the
// same quota, and an unkeyed one would answer 429 for everyone.
func TestDetailRequestCarriesTheKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "test-api-key" {
			t.Errorf("%s: key = %q, want %q", r.URL.Path, got, "test-api-key")
		}
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("%s: User-Agent = %q, want %q", r.URL.Path, got, userAgent)
		}
		if isVolumeDetailPath(r.URL.Path) {
			w.Write(readFixture(t, "volumes_detail.json"))
			return
		}
		w.Write(readFixture(t, "volumes_match.json"))
	}))
	t.Cleanup(server.Close)

	client := &Client{baseURL: server.URL, apiKey: "test-api-key", httpClient: server.Client()}
	if _, err := client.ByISBN(context.Background(), "9780547928227"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
}
