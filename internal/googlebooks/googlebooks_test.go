package googlebooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
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
//   - volumes_pair_list.json and volumes_detail.json — one volume
//     (M1t9BgAAQBAJ) from both endpoints, which is what makes them a pair:
//     the evidence that the larger imageLinks sizes and the HTML
//     description live only on the single-volume one, and the only shape
//     enrichVolume's id check will merge
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
		httpClient: testHTTPClient(server),
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
		httpClient: testHTTPClient(server),
	}, &detailHits
}

// testHTTPClient is httptest's client with this package's redirect policy
// on it. httptest.Server.Client() does not carry one, so a helper handing
// back the bare client silently tests against net/http's default hop limit
// and no scheme check at all — which is how a CheckRedirect regression goes
// unnoticed even with a redirect test in the file.
func testHTTPClient(server *httptest.Server) *http.Client {
	c := server.Client()
	c.CheckRedirect = enrich.CheckLookupRedirect
	return c
}

// isVolumeDetailPath reports whether p addresses one volume rather than the
// list endpoint — "/volumes/{id}" rather than "/volumes".
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

func TestNewSetsRedirectPolicy(t *testing.T) {
	if New("").httpClient.CheckRedirect == nil {
		t.Error("httpClient.CheckRedirect is nil; net/http would then follow any hop to any scheme")
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
// storage.PlainDescription renders it — but that documentation describes the
// single-volume endpoint. The list endpoint this client calls strips the
// markup itself (and truncates), so the flattening is a no-op on every real
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

// The list response in isolation: http as Google answers it, upgraded, and
// the zoom=1 thumbnail, which is all that endpoint ever names.
//
// This is not what a lookup returns any more — enrichVolume replaces it
// from the single-volume endpoint — and it passes here only because
// testClient 404s that path. What it pins is the list half of the merge:
// that toMetadata still reads a cover out of the list response at all, so
// a book whose detail request fails has one to keep.
func TestListResponseCoverURLIsTheUpgradedThumbnail(t *testing.T) {
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
func TestCoverURLPrefersMediumAndUpgradesToHTTPS(t *testing.T) {
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
			name:  "small beats thumbnail when neither medium nor large is offered",
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
//
// A decode rather than a round trip: what is asserted is that the field is
// absent from the struct, which no amount of HTTP would say more clearly.
func TestExtraLargeIsIgnored(t *testing.T) {
	body := `{"imageLinks":{"extraLarge":"https://books.example/xl","medium":"https://books.example/medium"}}`

	var info volumeInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := info.ImageLinks.best(); got != "https://books.example/medium" {
		t.Errorf("best() = %q, want the medium link — extraLarge must not win", got)
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

// A refused redirect belongs with 400 and 403, not with a transport
// failure: checkRedirect is a pure function of URLs that do not change
// between attempts, so all a retry buys is the same refusal twice more.
// Each case is a different clause of the policy, since the sentinel has to
// be on every return rather than the one that was easiest to reach.
func TestRefusedRedirectIsNotRetryable(t *testing.T) {
	cases := []struct {
		name     string
		location func(base string) string
	}{
		{"off-host", func(string) string { return "https://elsewhere.example/volumes" }},
		{"non-http scheme", func(string) string { return "file:///etc/passwd" }},
		{"endless chain", func(base string) string { return base + "/volumes?q=again" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var base string
			client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, c.location(base), http.StatusFound)
			})
			base = client.baseURL

			_, err := client.ByISBN(context.Background(), "9780262011532")
			if err == nil {
				t.Fatal("want an error")
			}
			if errors.Is(err, enrich.ErrRetryable) {
				t.Errorf("errors.Is(err, ErrRetryable) = true, want false: %v", err)
			}
		})
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
// to avoid this one. Deliberately accepted; the reasoning is in
// docs/notes/enrichment.md, under the plausibility gate.
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

// pairVolumeID is the volume volumes_pair_list.json and volumes_detail.json
// both describe — captures of one book from both endpoints, which is what
// makes them a pair rather than two fixtures. enrichVolume refuses a detail
// body naming a different id, so mismatched captures would exercise that
// refusal instead of the merge these tests are about.
const pairVolumeID = "M1t9BgAAQBAJ"

// listThenDetail serves that pair: the list capture, then whatever the
// caller wants at the single-volume endpoint.
func listThenDetail(t *testing.T, detail http.HandlerFunc) (*Client, *int) {
	t.Helper()
	return detailClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write(readFixture(t, "volumes_pair_list.json")) },
		detail)
}

// The whole point of the second request: the list endpoint's thumbnail is
// 128x192, under internal/cover's 400px target, and the sizes worth having
// live only on /volumes/{id}.
func TestMatchedVolumePrefersTheDetailEndpointsLargerCover(t *testing.T) {
	client, detailHits := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/volumes/"+pairVolumeID {
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
	if !strings.Contains(got.Description, "\n\n") {
		t.Error("Description is still the list endpoint's flattened form")
	}
}

// assertListAnswerIntact checks that m is exactly what volumes_pair_list.json
// alone produces — every text field, the flattened description and the
// thumbnail cover — so a failing detail request is provably a no-op rather
// than merely not an error.
func assertListAnswerIntact(t *testing.T, m enrich.Metadata) {
	t.Helper()
	if m.Title != "The Collected Works of Samuel Taylor Coleridge, Volume 10" {
		t.Errorf("Title = %q, want the list answer intact", m.Title)
	}
	if !strings.HasPrefix(m.Description, "Based on a comparison of early editions,") {
		t.Errorf("Description = %q, want the list answer's", m.Description)
	}
	if strings.Contains(m.Description, "\n") {
		t.Errorf("Description gained a paragraph break, so the detail answer leaked in: %q", m.Description)
	}
	if m.CoverURL != "https://books.google.com/books/content?id=M1t9BgAAQBAJ&printsec=frontcover&img=1&zoom=1&edge=curl&source=gbs_api" {
		t.Errorf("CoverURL = %q, want the list thumbnail %q", m.CoverURL, "https://books.google.com/books/content?id=M1t9BgAAQBAJ&printsec=frontcover&img=1&zoom=1&edge=curl&source=gbs_api")
	}
}

// A bigger cover and a paragraph break are niceties. Six good text fields
// are the answer, and no failure of the second request may cost them.
func TestDetailRequestFailureKeepsTheListAnswer(t *testing.T) {
	tests := []struct {
		name   string
		detail http.HandlerFunc
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
			name:   "empty body",
			detail: func(w http.ResponseWriter, r *http.Request) {},
		},
		{
			name:   "json null",
			detail: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("null")) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := listThenDetail(t, tt.detail)

			got, err := client.ByISBN(context.Background(), "9780547928227")
			if err != nil {
				t.Fatalf("ByISBN: want nil error — a lost cover must not fail a lookup that has its text fields; got %v", err)
			}
			assertListAnswerIntact(t, got)
			// Every one of these might succeed next time, so the answer
			// must not be cached as though it were whole.
			if !got.Partial {
				t.Error("Partial = false, want the answer marked so WithCache declines to store it")
			}
		})
	}
}

// A body describing another volume is a failed detail request like any
// other: the two fields it would have filled are still unfilled, and the
// next attempt might get the right one.
func TestDetailBodyNamingAnotherVolumeMarksTheAnswerPartial(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"someOtherVolume","volumeInfo":{"description":"<p>Not this book.</p>"}}`))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	assertListAnswerIntact(t, got)
	if !got.Partial {
		t.Error("Partial = false, want a mismatched volume id treated as a failed detail request")
	}
}

// The other direction, so the mark cannot be set unconditionally: a whole
// answer must stay cacheable, or the cache stops working entirely.
func TestDetailRequestSuccessLeavesTheAnswerWhole(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "volumes_detail.json"))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if got.Partial {
		t.Error("Partial = true on a detail request that succeeded")
	}
}

// Cancellation is its own case because it must be provoked deterministically.
// A wall-clock deadline set before the call covers the *list* request too, so
// on a loaded machine that request is what times out and the test fails on a
// cause it is not about — measured at roughly 9% under -race. Cancelling from
// inside the detail handler cannot race: by then the list request has already
// returned.
func TestDetailRequestCancellationKeepsTheListAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, detailHits := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})

	got, err := client.ByISBN(ctx, "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: want nil error on a cancelled detail request; got %v", err)
	}
	if *detailHits != 1 {
		t.Fatalf("detail requests = %d, want 1 — the cancellation must happen inside the second request", *detailHits)
	}
	assertListAnswerIntact(t, got)
}

// The one condition that makes the second request pointless before it is
// made: no id to address it to.
func TestDetailRequestIsSkippedWithoutAVolumeID(t *testing.T) {
	body := `{"totalItems":1,"items":[{"volumeInfo":{"title":"No id here","imageLinks":{"thumbnail":"http://books.example/t.jpg"}}}]}`

	client, detailHits := detailClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) },
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("the detail endpoint was called")
		})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if *detailHits != 0 {
		t.Errorf("detail requests = %d, want 0", *detailHits)
	}
	// Not Partial: there is no detail endpoint to ask for a volume with no
	// id, so nothing a later attempt would do differently. Marking it
	// would make every id-less volume permanently uncacheable.
	if got.Partial {
		t.Error("Partial = true, but no request was skipped that a retry could make")
	}
}

// A volume with no cover art is still worth the request, for its
// description. Skipping one would be free on the cover half and would
// silently drop the other, which is the payoff for a book whose blurb is
// the thing worth having.
func TestVolumeWithNoCoverStillGetsItsDescription(t *testing.T) {
	client, detailHits := detailClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"totalItems":1,"items":[{"id":"abc123","volumeInfo":{"title":"Coverless"}}]}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"id":"abc123","volumeInfo":{"description":"<p>First.</p><p>Second.</p>"}}`))
		})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if *detailHits != 1 {
		t.Fatalf("detail requests = %d, want 1", *detailHits)
	}
	if got.Description != "First.\n\nSecond." {
		t.Errorf("Description = %q, want the detail endpoint's rendered markup", got.Description)
	}
	if got.CoverURL != "" {
		t.Errorf("CoverURL = %q, want empty", got.CoverURL)
	}
}

// A detail body naming a different volume is refused rather than merged.
// plausibleMatch gates the Title and Authors of the *list* response, so a
// substituted body's description and cover would reach books unchecked.
func TestDetailBodyNamingAnotherVolumeIsRefused(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"someOtherVolume","volumeInfo":{
			"description":"<p>A different book entirely.</p>",
			"imageLinks":{"large":"https://books.example/wrong.jpg"}}}`))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	assertListAnswerIntact(t, got)
}

// The description guard has its own test above; this is the cover's. A
// detail response with no imageLinks must not blank a cover the list
// response supplied.
func TestDetailResponseWithoutImageLinksKeepsTheListCover(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":%q,"volumeInfo":{"description":"<p>Only a blurb.</p>"}}`, pairVolumeID)
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	var list volumesResponse
	if err := json.Unmarshal(readFixture(t, "volumes_pair_list.json"), &list); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	want := strings.Replace(list.Items[0].VolumeInfo.ImageLinks.Thumbnail, "http://", "https://", 1)
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want the list thumbnail %q", got.CoverURL, want)
	}
	if got.Description != "Only a blurb." {
		t.Errorf("Description = %q, want the detail endpoint's", got.Description)
	}
}

// A whitespace-only detail description must not overwrite a real one. It
// reaches books as "" — enrich.sanitizeValue trims it and Resolve skips an
// empty value — so the field is simply lost, and with the default provider
// order Google is last, so nothing recovers it.
func TestWhitespaceOnlyDetailDescriptionKeepsTheListOne(t *testing.T) {
	blanks := []string{
		`"   "`, `"\n"`, `"\n\n  "`, `"<br>"`, `""`,
		`"\u00a0"`,       // a non-breaking space
		`"\u3000"`,       // the CJK ideographic space, which a cutset-based trim drops
		`"&#12288;"`,     // the same, escaped, which is how a description carries it
		`"\u2028"`,       // a line separator
		`"\u205f"`,       // a medium mathematical space
		`"&#8203;"`,      // a zero-width space, which unicode.IsSpace does not accept
		`"\ufeff"`,       // a byte-order mark arriving as content
		`"&#8203; \n"`,   // mixed with ordinary whitespace
		`"\u3000\u200b"`, // and both kinds together
	}
	for _, blank := range blanks {
		t.Run(blank, func(t *testing.T) {
			client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"id":%q,"volumeInfo":{"description":%s}}`, pairVolumeID, blank)
			})

			got, err := client.ByISBN(context.Background(), "9780547928227")
			if err != nil {
				t.Fatalf("ByISBN: %v", err)
			}
			assertListAnswerIntact(t, got)
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
		fmt.Fprintf(w, `{"id":%q,"volumeInfo":{"imageLinks":{"large":"https://books.example/large.jpg"}}}`, pairVolumeID)
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	if !strings.HasPrefix(got.Description, "Based on a comparison of early editions,") {
		t.Errorf("Description = %q, want the list answer's", got.Description)
	}
	if got.CoverURL != "https://books.example/large.jpg" {
		t.Errorf("CoverURL = %q, want the detail endpoint's only link", got.CoverURL)
	}
}

// The key travels on the detail request too — it is the same API and the
// same quota, and an unkeyed one would answer 429 for everyone.
func TestDetailRequestCarriesTheKey(t *testing.T) {
	detailHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "test-api-key" {
			t.Errorf("%s: key = %q, want %q", r.URL.Path, got, "test-api-key")
		}
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("%s: User-Agent = %q, want %q", r.URL.Path, got, userAgent)
		}
		if isVolumeDetailPath(r.URL.Path) {
			detailHits++
			w.Write(readFixture(t, "volumes_detail.json"))
			return
		}
		w.Write(readFixture(t, "volumes_pair_list.json"))
	}))
	t.Cleanup(server.Close)

	client := &Client{baseURL: server.URL, apiKey: "test-api-key", httpClient: server.Client()}
	if _, err := client.ByISBN(context.Background(), "9780547928227"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	// Without this the assertions above are vacuous: they live inside the
	// handler, so a client that never made the second request passes.
	if detailHits != 1 {
		t.Errorf("detail requests = %d, want 1", detailHits)
	}
}

// The detail path is the one where a leaked key is certain rather than
// possible. Its error never reaches a caller — enrichVolume swallows it —
// so slog.Debug is the only place it goes, and CLAUDE.md's invariant is
// that the key must never reach a log line. The list path's two redaction
// tests do not cover this one: all three redaction calls could be deleted
// from volumeByID and the suite stayed green.
func TestAPIKeyNeverAppearsInTheDetailPathsLogLine(t *testing.T) {
	const key = "super-secret-key"

	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isVolumeDetailPath(r.URL.Path) {
			w.WriteHeader(http.StatusInternalServerError)
			// Google's own error bodies do not echo the key, but a
			// misbehaving upstream or a proxy may, and the body reaches
			// the error text verbatim.
			fmt.Fprintf(w, "upstream rejected key=%s", key)
			return
		}
		w.Write(readFixture(t, "volumes_pair_list.json"))
	}))
	t.Cleanup(server.Close)

	client := &Client{baseURL: server.URL, apiKey: key, httpClient: server.Client()}
	if _, err := client.ByISBN(context.Background(), "9780547928227"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}

	out := logged.String()
	if !strings.Contains(out, "volume detail lookup failed") {
		t.Fatalf("the failure was not logged at all, so this test proves nothing: %q", out)
	}
	if strings.Contains(out, key) {
		t.Errorf("the log line leaks the API key: %q", out)
	}
}

// The same, for a transport failure — the shape that embeds the whole
// request URL, key and all, rather than a body this package chose to read.
func TestAPIKeyNeverAppearsInTheDetailPathsTransportErrorLog(t *testing.T) {
	const key = "super-secret-key"

	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isVolumeDetailPath(r.URL.Path) {
			cancel()
			<-r.Context().Done()
			return
		}
		w.Write(readFixture(t, "volumes_pair_list.json"))
	}))
	t.Cleanup(server.Close)

	client := &Client{baseURL: server.URL, apiKey: key, httpClient: server.Client()}
	if _, err := client.ByISBN(ctx, "9780547928227"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}

	out := logged.String()
	if !strings.Contains(out, "volume detail lookup failed") {
		t.Fatalf("the failure was not logged at all, so this test proves nothing: %q", out)
	}
	if strings.Contains(out, key) {
		t.Errorf("the log line leaks the API key: %q", out)
	}
}

// A redirect off Google is followed by default, and on the detail path the
// whole answer — the cover URL and the description — would come from
// whichever host replied, with plausibleMatch none the wiser since it gates
// the *list* response. The policy is what stops the chain; the id check
// stops a body that arrives anyway.
func TestDetailRequestDoesNotFollowARedirectToAnotherScheme(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	assertListAnswerIntact(t, got)
}

// A redirect loop is bounded rather than followed until the client's own
// timeout, which is eight seconds this lookup does not have to spend.
//
// The handler stops redirecting at a hard ceiling of its own rather than
// looping forever: without one, a policy that has lost its hop limit makes
// this test hang instead of fail, and a hang is a far worse signal than a
// red line.
func TestDetailRequestBoundsARedirectLoop(t *testing.T) {
	const ceiling = enrich.MaxLookupRedirects * 4

	hops := 0
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		hops++
		if hops > ceiling {
			w.Write([]byte(`{"id":"someOtherVolume","volumeInfo":{}}`))
			return
		}
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	// MaxLookupRedirects hops after the first request, so +1 in all.
	if hops > enrich.MaxLookupRedirects+1 {
		t.Errorf("followed %d hops, want at most %d", hops, enrich.MaxLookupRedirects+1)
	}
	assertListAnswerIntact(t, got)
}

// The volume id comes out of a remote response and goes straight into a
// URL path, so it is escaped rather than concatenated. Unescaped, an id of
// "../../../etc/passwd?key=leak&x=" turns its "?" into a real query
// separator — appending parameters of the id's choosing after the
// configured key — and its slashes into real path separators.
//
// The assertion is on the *wire* form (EscapedPath), not r.URL.Path, which
// net/url hands back decoded: the escaping is what stops a server routing
// the request somewhere else, and a decoded Path showing ".." is expected
// even when it worked.
func TestDetailRequestEscapesTheVolumeID(t *testing.T) {
	const hostile = `../../../etc/passwd?key=leak&x=`

	var gotEscapedPath, gotRawQuery string
	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/volumes" && r.URL.Query().Get("q") != "" {
			fmt.Fprintf(w, `{"totalItems":1,"items":[{"id":%q,"volumeInfo":{"title":"T"}}]}`, hostile)
			return
		}
		reached = true
		gotEscapedPath, gotRawQuery = r.URL.EscapedPath(), r.URL.RawQuery
		w.Write([]byte(`{"volumeInfo":{}}`))
	}))
	t.Cleanup(server.Close)

	client := &Client{baseURL: server.URL, apiKey: "configured-key", httpClient: testHTTPClient(server)}
	if _, err := client.ByISBN(context.Background(), "9780547928227"); err != nil {
		t.Fatalf("ByISBN: %v", err)
	}

	if !reached {
		t.Fatal("the detail endpoint was never reached, so nothing was escaped")
	}
	// One path segment under /volumes/, whatever the id contained.
	if strings.Count(strings.TrimPrefix(gotEscapedPath, "/volumes/"), "/") != 0 {
		t.Errorf("escaped path %q has more than one segment under /volumes/", gotEscapedPath)
	}
	// The "?" in the id must not have started a query.
	q, err := url.ParseQuery(gotRawQuery)
	if err != nil {
		t.Fatalf("parse query %q: %v", gotRawQuery, err)
	}
	if got := q["key"]; len(got) != 1 || got[0] != "configured-key" {
		t.Errorf("key = %v, want exactly the configured one — the id smuggled a parameter", got)
	}
	if len(q) != 1 {
		t.Errorf("query = %v, want only the key — the id added parameters of its own", q)
	}
}

// net/http sets Referer on every redirect hop from the previous request's
// full URL, and this client's URL carries "?key=…". It suppresses that only
// on https→http, so an ordinary https→https redirect hands the credential
// to whichever host answered — in a header, to a third party, which is
// worse than the log-line leak redactKey exists to prevent. The host check
// is what stops it, and this asserts the leak by watching for it rather
// than by trusting the policy's own unit test.
func TestARedirectOffTheAPIHostNeverCarriesTheKey(t *testing.T) {
	const key = "SUPERSECRETKEY"

	var mu sync.Mutex
	var foreignSaw []string
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		foreignSaw = append(foreignSaw, r.Header.Get("Referer")+"|"+r.URL.RawQuery)
		mu.Unlock()
		w.Write([]byte(`{"totalItems":1,"items":[{"id":"x","volumeInfo":{"title":"Foreign","description":"A different book entirely."}}]}`))
	}))
	t.Cleanup(foreign.Close)

	home := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(home.Close)

	httpClient := foreign.Client()
	httpClient.CheckRedirect = enrich.CheckLookupRedirect
	client := &Client{baseURL: home.URL, apiKey: key, httpClient: httpClient}

	got, err := client.ByISBN(context.Background(), "9780547928227")

	// What the foreign host saw is reported first and with Errorf, so a
	// regression prints the evidence rather than stopping at the verdict:
	// a Fatal here would make the one observation this test exists to make
	// unreachable on both paths.
	mu.Lock()
	saw := append([]string(nil), foreignSaw...)
	mu.Unlock()
	for _, s := range saw {
		t.Errorf("the foreign host was reached, and saw Referer|query %q", s)
	}
	if err == nil {
		t.Error("ByISBN: want an error when the lookup is redirected off its host")
	} else if strings.Contains(err.Error(), key) {
		t.Errorf("the error text leaks the API key: %v", err)
	}
	// And nothing the foreign host said was adopted.
	if got.Title != "" || got.Description != "" {
		t.Errorf("adopted the foreign host's answer: %+v", got)
	}
}

// A detail body with no id at all must be refused like one naming the wrong
// volume: treating "" as a pass lets the party being checked opt out of the
// check. Every capture under testdata carries an id, so nothing legitimate
// is turned away.
func TestDetailBodyWithoutAnIDIsRefused(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"volumeInfo":{
			"description":"<p>A different book entirely.</p>",
			"imageLinks":{"large":"https://elsewhere.example/wrong.jpg"}}}`))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	assertListAnswerIntact(t, got)
}

// The 4 MiB cap is the only bound on how much a misbehaving or hijacked
// upstream can make this process allocate on the detail path.
func TestDetailResponseIsBounded(t *testing.T) {
	client, _ := listThenDetail(t, func(w http.ResponseWriter, r *http.Request) {
		// Valid JSON far past the cap, so a missing LimitReader parses
		// happily and only the bound can refuse it.
		fmt.Fprintf(w, `{"id":%q,"volumeInfo":{"description":%q}}`,
			pairVolumeID, strings.Repeat("x", maxResponseBytes+1024))
	})

	got, err := client.ByISBN(context.Background(), "9780547928227")
	if err != nil {
		t.Fatalf("ByISBN: %v", err)
	}
	assertListAnswerIntact(t, got)
}

// The same bound on the list path. Pinned alongside the detail one rather
// than left to symmetry: an untested cap is one an edit can drop silently,
// and this is the response a hijacked or misbehaving upstream controls.
func TestListResponseIsBounded(t *testing.T) {
	client, _ := testClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"totalItems":1,"items":[{"id":"abc","volumeInfo":{"description":%q}}]}`,
			strings.Repeat("x", maxResponseBytes+1024))
	})

	if _, err := client.ByISBN(context.Background(), "9780547928227"); err == nil {
		t.Fatal("ByISBN: want an error for a response past maxResponseBytes")
	}
}

// The key reaches an error's text through the request URL, where
// url.Values.Encode has percent-escaped it — so a literal substring match
// alone misses any key containing a character that needs escaping. Today's
// Google keys are "AIza" plus URL-safe characters, which is a property of
// the key format rather than of redactKey, and not one to rest a
// credential on.
func TestAPIKeyIsRedactedInItsEncodedForm(t *testing.T) {
	const key = "weird/key+with spaces"

	client, _ := testClient(t, key, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("upstream said no"))
	})

	_, err := client.ByISBN(context.Background(), "9780547928227")
	if err == nil {
		t.Fatal("ByISBN: want an error on 503")
	}
	for _, form := range []string{key, url.QueryEscape(key), url.PathEscape(key)} {
		if strings.Contains(err.Error(), form) {
			t.Errorf("error text leaks the key as %q: %v", form, err)
		}
	}
}

// The same on the transport path, which is where the full request URL —
// and therefore the encoded key — actually ends up.
func TestEncodedAPIKeyIsRedactedInATransportError(t *testing.T) {
	const key = "weird/key+with spaces"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, _ := testClient(t, key, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})

	_, err := client.ByISBN(ctx, "9780547928227")
	if err == nil {
		t.Fatal("ByISBN: want an error on a cancelled request")
	}
	for _, form := range []string{key, url.QueryEscape(key)} {
		if strings.Contains(err.Error(), form) {
			t.Errorf("error text leaks the key as %q: %v", form, err)
		}
	}
}
