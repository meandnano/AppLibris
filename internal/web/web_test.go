package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"library/internal/service"
	"library/internal/storage"
)

func TestLibraryHandlerRendersScannedBooks(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.CreateBook(context.Background(), storage.Book{
		ContentHash: "hash-1",
		Title:       "The Test Book",
		SortTitle:   "Test Book",
		Format:      "epub",
	}, []string{"Jane Doe"}); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	svc := service.New(db)
	handler := Routes(svc, t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "The Test Book") {
		t.Errorf("GET / body = %q, want it to contain %q", body, "The Test Book")
	}
	if !strings.Contains(body, "Jane Doe") {
		t.Errorf("GET / body = %q, want it to contain the author %q", body, "Jane Doe")
	}
}

func TestLibraryHandlerRendersEmptyState(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "No books yet") {
		t.Errorf("GET / body = %q, want the empty state", body)
	}
}

func newTestHandlerWithBook(t *testing.T, title string, authors []string) http.Handler {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.CreateBook(context.Background(), storage.Book{
		ContentHash: "hash-1",
		Title:       title,
		SortTitle:   title,
		Format:      "epub",
	}, authors); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	return Routes(service.New(db), t.TempDir(), false, false)
}

func TestSearchFullPageRendersFilteredGridWithEchoedQuery(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/?q=<script>alert(1)</script>", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=... status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<html") {
		t.Errorf("GET /?q=... without HX-Request body does not look like a full page: %q", body)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("GET /?q=... body contains an unescaped <script> tag from the query: %q", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("GET /?q=... body does not contain the HTML-escaped query echoed back: %q", body)
	}
	if !strings.Contains(body, "Nothing matches") {
		t.Errorf("GET /?q=... body = %q, want the no-results state (nothing titled <script>...)", body)
	}
}

// The masthead count must stay the library's total size on a full-page
// search render, not the filtered result count — otherwise a shared or
// bookmarked ?q= link, or a plain page reload, reports a misleadingly
// small library size. It also must agree with what the swapped fragment
// shows once a live search settles: a stale masthead frozen at some other
// number would be just as misleading as a wrong one.
func TestSearchFullPageMastheadCountStaysLibraryTotal(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for i, title := range []string{"Piranesi", "Flights", "One Hundred Years of Solitude"} {
		if _, err := db.CreateBook(context.Background(), storage.Book{
			ContentHash: fmt.Sprintf("hash-%d", i),
			Title:       title,
			SortTitle:   title,
			Format:      "epub",
		}, nil); err != nil {
			t.Fatalf("CreateBook %q: %v", title, err)
		}
	}

	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=Piranesi status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "3 books") {
		t.Errorf("GET /?q=Piranesi masthead count missing/wrong in body: %q, want it to contain %q", body, "3 books")
	}
	if strings.Contains(body, "1 book<") {
		t.Errorf("GET /?q=Piranesi masthead shows the filtered count (1) instead of the library total (3): %q", body)
	}
}

func TestSearchFragmentOmitsFullPageChromeAndSetsVary(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=Piranesi (HX-Request) status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "<head") {
		t.Errorf("HX-Request response contains full-page chrome: %q", body)
	}
	if !strings.Contains(body, "Piranesi") {
		t.Errorf("HX-Request response = %q, want it to contain the matching book", body)
	}
	if got := rec.Header().Get("Vary"); got != wantVary {
		t.Errorf("GET /?q=Piranesi Vary = %q, want %q", got, wantVary)
	}
}

// wantVary is every request header that changes this URL's body: HX-Request
// picks the fragment, and HX-History-Restore-Request takes it back off a
// request that carries both.
const wantVary = "HX-Request, HX-History-Restore-Request"

// Vary: HX-Request has to be on both halves of the contract to mean
// anything: it exists so a cache keys the full page and the fragment
// separately, which only works if a cache that stored the full page first
// (never having seen HX-Request) still knows to treat a later
// HX-Request-bearing request for the same URL as a different response.
// Pinning it on only one branch would pass even if the header were moved
// inside the other.
func TestVarySetOnBothFullPageAndFragmentResponses(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	full := httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	fullRec := httptest.NewRecorder()
	handler.ServeHTTP(fullRec, full)
	if got := fullRec.Header().Get("Vary"); got != wantVary {
		t.Errorf("full-page GET /?q=Piranesi Vary = %q, want %q", got, wantVary)
	}

	fragment := httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	fragment.Header.Set("HX-Request", "true")
	fragmentRec := httptest.NewRecorder()
	handler.ServeHTTP(fragmentRec, fragment)
	if got := fragmentRec.Header().Get("Vary"); got != wantVary {
		t.Errorf("fragment GET /?q=Piranesi (HX-Request) Vary = %q, want %q", got, wantVary)
	}
}

// newTestHandlerWithLocations sets up one book with n file locations
// ("/loc-0.epub", "/loc-1.epub", ...) for the multi-location badge tests.
func newTestHandlerWithLocations(t *testing.T, title string, n int) http.Handler {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	mtime := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	bookID, _, _, _, err := db.CreateBookWithFile(ctx, storage.Book{
		ContentHash: "hash-1",
		Title:       title,
		SortTitle:   title,
		Format:      "epub",
	}, nil, "/loc-0.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	for i := 1; i < n; i++ {
		if _, err := db.UpsertBookFile(ctx, bookID, fmt.Sprintf("/loc-%d.epub", i), 100, mtime); err != nil {
			t.Fatalf("UpsertBookFile %d: %v", i, err)
		}
	}

	return Routes(service.New(db), t.TempDir(), false, false)
}

// The mutation this guards against: dropping the PathsLabel assignment
// entirely would still pass every storage and service test, since those
// only check the Locations count reaches BookSummary — this is the one test
// that would catch the marker never reaching the rendered page.
func TestMultiLocationBookRendersPathsMarker(t *testing.T) {
	handler := newTestHandlerWithLocations(t, "Duplicated Book", 2)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="card__paths"`) {
		t.Errorf("GET / body missing the paths marker: %q", body)
	}
	if !strings.Contains(body, "2 paths") {
		t.Errorf("GET / body = %q, want it to contain %q", body, "2 paths")
	}
}

// A single-location book must render no marker at all, not an empty span:
// asserting the class is absent (not just the text) catches a threshold bug
// that renders the marker with empty content.
func TestSingleLocationBookRendersNoPathsMarker(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Solo Book", nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `class="card__paths"`) {
		t.Errorf("GET / body for a single-location book contains a paths marker: %q", body)
	}
}

// The count is the real number, not a hardcoded 2.
func TestThreeLocationBookRendersThreePaths(t *testing.T) {
	handler := newTestHandlerWithLocations(t, "Triplicated Book", 3)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "3 paths") {
		t.Errorf("GET / body = %q, want it to contain %q", body, "3 paths")
	}
}

// The marker has to survive into the book-grid fragment a live search
// request gets, not just the full page.
func TestMultiLocationMarkerSurvivesIntoSearchFragment(t *testing.T) {
	handler := newTestHandlerWithLocations(t, "Piranesi", 2)

	req := httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=Piranesi (HX-Request) status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "2 paths") {
		t.Errorf("HX-Request fragment body = %q, want it to contain %q", body, "2 paths")
	}
}

// A change to the search input's htmx attributes (a typo'd trigger, a
// dropped hx-target) would leave every other test in this file green,
// since they all drive libraryHandler directly rather than checking what
// actually reaches the browser. This pins the rendered markup itself: the
// script that makes any of it work, and the attributes search-as-you-type
// depends on — debounce, partial target, whole-element swap, URL
// tracking, and the request-in-flight indicator.
func TestSearchBarHTMXWiringContract(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/?q=Pira", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<script src="/static/js/htmx.min.js" defer></script>`) {
		t.Error("body missing the htmx script tag — none of the hx-* attributes below do anything without it")
	}

	for _, want := range []string{
		`hx-get="/"`,
		`hx-trigger="input changed delay:300ms, search"`,
		`hx-target="#book-grid"`,
		`hx-swap="outerHTML"`,
		`hx-push-url="true"`,
		`hx-indicator="closest form"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search input missing %s — body = %q", want, body)
		}
	}

	// The input's own value attribute specifically, not just the query
	// appearing somewhere on the page — it's also echoed, escaped, in the
	// no-results heading when a query matches nothing, so a substring
	// check against the whole body can't tell the two apart.
	if !strings.Contains(body, `value="Pira"`) {
		t.Errorf("search input's value attribute is not %q; body = %q", "Pira", body)
	}
}

func TestSearchNoResultsIsDistinctFromEmptyLibrary(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/?q=nonexistentbook", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "search__empty") {
		t.Errorf("no-results body = %q, want the search__empty block", body)
	}
	if strings.Contains(body, "No books yet") {
		t.Errorf("no-results body = %q, want the no-results block, not the empty-library block", body)
	}
}

func TestSearchBlankQueryIsIdleNotSearching(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	for _, path := range []string{"/", "/?q=", "/?q=%20%20"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, "Piranesi") {
			t.Errorf("GET %s body = %q, want the full unfiltered grid", path, body)
		}
		if strings.Contains(body, "search__count") {
			t.Errorf("GET %s body = %q, want no result count on the idle/unfiltered grid", path, body)
		}
	}
}

// A raw NUL byte in the query string (curl --data-urlencode 'q=%00', or
// anything else that can put %00 in a URL) must not 500 the search route:
// SanitizeFTSQuery strips control characters before they can reach FTS5's
// MATCH parser, which otherwise rejects an embedded NUL as an unterminated
// string.
func TestSearchHandlesNULInQueryParam(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	for _, rawQuery := range []string{"q=%00", "q=hel%00lo"} {
		req := httptest.NewRequest(http.MethodGet, "/?"+rawQuery, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("GET /?%s status = %d, want 200 (not a 500 from an unsanitized NUL reaching MATCH)", rawQuery, rec.Code)
		}
	}
}

// A q past storage.MaxSearchBytes is searched clipped, so every copy this
// page renders has to be the clipped one: the input's value, so the box
// shows what was searched, and the paging URLs, so a reveal — and the
// hx-push-url entry the browser keeps per keystroke — carries the same
// bounded string rather than the original.
func TestOverlongSearchQueryIsClippedInEveryRenderedCopy(t *testing.T) {
	// The titles carry one long token and the query another, agreeing only
	// over the first MaxSearchBytes of it: the clipped query is a prefix
	// of the title token, so the search matches enough books to page,
	// while the unclipped query is a string the page has no other reason
	// to contain.
	clipped := strings.Repeat("x", storage.MaxSearchBytes)
	titleToken := clipped + strings.Repeat("y", 64)
	queryToken := clipped + strings.Repeat("z", 64)

	db := newPagingTestDB(t)
	seedBooks(t, db, pageSize+10, titleToken)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	rec := get(handler, "/?q="+queryToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=<overlong> = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if strings.Contains(body, queryToken) {
		t.Error("body carries the unclipped query somewhere")
	}
	// Against storage's own normalization rather than a number, since what
	// makes the rendered copies trustworthy is that the handler and the
	// service bound the query the same way, not that both spell 256.
	if want := storage.NormalizeSearchQuery(queryToken); want != clipped {
		t.Fatalf("fixture assumes the clip: NormalizeSearchQuery gives %.16q…", want)
	}
	if !strings.Contains(body, `value="`+clipped+`"`) {
		t.Errorf("search input does not echo the clipped query; got %q", lineContaining(body, "search__input"))
	}
	trigger := triggerElement(body)
	if trigger == "" {
		t.Fatalf("no reveal trigger rendered, so there are no paging URLs to check; body has %d cards", countCards(body))
	}
	if !strings.Contains(trigger, "q="+clipped) {
		t.Errorf("paging URL does not carry the clipped query: %q", trigger)
	}
}

// The search input's maxlength is what keeps a paste from being sent and
// pushed into history in full — the input is never swapped, so the
// handler's clip cannot reach it between keystrokes. The number comes from
// the view model, and a forgotten field renders maxlength="0", which makes
// the box untypeable while every other test here stays green.
func TestSearchInputCarriesTheByteCapAsMaxlength(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	rec := get(handler, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	input := lineContaining(rec.Body.String(), "search__input")
	if want := fmt.Sprintf(`maxlength="%d"`, storage.MaxSearchBytes); !strings.Contains(input, want) {
		t.Errorf("search input does not carry %s: %q", want, input)
	}
}

// The handler normalizes through storage, so a control character is gone
// before it can be rendered — a raw NUL in an attribute value otherwise
// reaches the browser, and every later copy of the query (the paging URLs)
// carries it too.
func TestControlCharacterIsStrippedFromTheRenderedQuery(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	rec := get(handler, "/?q=hel%00lo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=hel%%00lo = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.ContainsRune(body, 0) {
		t.Error("rendered body carries a raw NUL from the query")
	}
	if !strings.Contains(body, `value="hello"`) {
		t.Errorf("search input does not echo the stripped query; got %q", lineContaining(body, "search__input"))
	}
}

// The transport half of the rune-boundary rule: a clipped multibyte query
// is rendered as text, so cutting one mid-rune would put a replacement
// character in the search box and in every paging URL.
func TestClippedMultibyteQueryRendersAsValidUTF8(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	rec := get(handler, "/?q="+url.QueryEscape(strings.Repeat("é", storage.MaxSearchBytes)), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=<overlong multibyte> = %d, want 200", rec.Code)
	}
	if !utf8.ValidString(rec.Body.String()) {
		t.Error("rendered page is not valid UTF-8, so the query was cut mid-rune")
	}
	if strings.Contains(rec.Body.String(), "�") {
		t.Error("rendered page carries a replacement character from a query cut mid-rune")
	}
}

// htmx sends HX-Request on a history-restore request as well as on a live
// search, but the two want different bodies: a restore swaps whatever comes
// back into the whole document body, so answering it with the book-grid
// fragment replaces the masthead, the search bar and the scripts with a
// bare grid that can no longer search — recoverable only by a manual
// reload. htmx marks that request HX-History-Restore-Request; this pins
// that the handler tells the two apart. Reachable by ordinary Back-button
// use: hx-push-url pushes a URL per keystroke and htmx's history cache
// holds ten.
func TestHistoryRestoreRequestGetsFullPageNotFragment(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-History-Restore-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("history-restore GET /?q=Piranesi status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<html", "<head", `id="search-form"`, "htmx.min.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("history-restore response is missing %q — htmx swaps this into the whole body, so it must be a full page", want)
		}
	}
	if !strings.Contains(body, "Piranesi") {
		t.Error("history-restore response = missing the matching book")
	}
	if got := rec.Header().Get("Vary"); got != wantVary {
		t.Errorf("history-restore Vary = %q, want %q — the header decides the body, so it has to be named here", got, wantVary)
	}
}

// A query that is non-blank but sanitizes to nothing — control characters
// are stripped before anything else — is not a search, and the page must
// not claim otherwise. Deriving "searching" from the raw query instead of
// from what the service actually did renders the results line ("3 of 3 ·
// matched …") over the entire unfiltered library.
func TestControlCharacterQueryRendersIdleNotSearchResults(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/?q=%00", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?q=%%00 status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "matched") {
		t.Error("GET /?q=%00 rendered the search count line; a query that sanitizes to nothing is not a search")
	}
	if strings.Contains(body, "search__empty") {
		t.Error("GET /?q=%00 rendered the no-results block; the library is not empty and no search ran")
	}
	if !strings.Contains(body, "Piranesi") {
		t.Error("GET /?q=%00 dropped the library listing; it should render the idle grid")
	}
}

// Plate 02c specifies the results line exactly: "4 of 1,284 · matched
// title, author" — the match count against the library total, then the
// indexed fields that produced the hits, so a match on a description or an
// ISBN isn't a mystery. A bare "N books matched" loses both halves.
func TestSearchResultsLineNamesTotalAndMatchedFields(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	books := []struct{ title, author, description, isbn string }{
		{"The Left Hand of Darkness", "Ursula K. Le Guin", "A novel about winter", "9780857059985"},
		{"The Dispossessed", "Ursula K. Le Guin", "", ""},
		{"Piranesi", "Susanna Clarke", "", ""},
	}
	for i, b := range books {
		if _, err := db.CreateBook(context.Background(), storage.Book{
			ContentHash: fmt.Sprintf("hash-%d", i), Title: b.title, SortTitle: b.title,
			Description: b.description, ISBN: b.isbn, Format: "epub",
		}, []string{b.author}); err != nil {
			t.Fatalf("CreateBook %d: %v", i, err)
		}
	}
	handler := Routes(service.New(db), t.TempDir(), false, false)

	for _, tc := range []struct{ query, want string }{
		{"le+guin", "2 of 3 · matched author"},
		{"winter", "1 of 3 · matched description"},
		{"9780857059985", "1 of 3 · matched isbn"},
		{"piranesi", "1 of 3 · matched title"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/?q="+tc.query, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		want := `<p class="search__count">` + tc.want + `</p>`
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET /?q=%s results line missing %q", tc.query, want)
		}
	}
}

// Counts are grouped in the mockups wherever they appear, and the results
// line quotes the same number the masthead does — rendering it two ways on
// one screen would be worse than rendering it plainly in both.
func TestCountsAreGroupedByThousands(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "0"}, {7, "7"}, {999, "999"}, {1000, "1,000"}, {1284, "1,284"}, {12840, "12,840"}, {1234567, "1,234,567"}} {
		if got := formatCount(tc.n); got != tc.want {
			t.Errorf("formatCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// Plate 02c/02d put a "clear ×" affordance in the input and plate 01 puts a
// "/" shortcut hint beside it. Neither can be rendered per keystroke — the
// input is never re-rendered — so both are markup the browser resolves:
// CSS hides the clear link while the box is empty, and search.js unhides
// the hint once it has bound the key.
func TestSearchBarCarriesClearAndShortcutAffordances(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`<a class="search__clear" href="/">clear ×</a>`,
		`<kbd class="search__shortcut" data-search-shortcut hidden>/</kbd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search bar missing %q", want)
		}
	}
	// The status line sits outside the form, in the same box as the count.
	if !strings.Contains(body, `<p class="search__status" role="status">filtering …</p>`) {
		t.Error("page missing the filtering status line")
	}
	for _, want := range []string{
		`<script src="/static/js/search.js" defer></script>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search bar missing %q", want)
		}
	}
}

// Plate 02e: with nothing indexed there is nothing to search, so the
// control is dimmed and inert rather than inviting a query that could only
// ever come back empty.
func TestEmptyLibraryDisablesTheSearchControl(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "search--disabled") {
		t.Error("empty library: search form missing the search--disabled class")
	}
	if !strings.Contains(body, "data-search-input disabled") {
		t.Error("empty library: search input is not disabled")
	}
	if !strings.Contains(body, "No books yet") {
		t.Error("empty library: missing the empty-library block")
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir(), false, false)

	for _, path := range []string{"/nope", "/books/1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestLibraryHandlerSetsContentType(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("GET / Content-Type = %q, want %q", got, "text/html; charset=utf-8")
	}
}

func TestRenderFailureProducesNoPartialBody(t *testing.T) {
	rec := httptest.NewRecorder()
	err := render(rec, "no-such-template", nil)
	if err == nil {
		t.Fatal("render with an unknown template name returned nil error, want one")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("render body = %q, want empty on error", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Errorf("render set Content-Type %q on a failed render, want unset", rec.Header().Get("Content-Type"))
	}
}

// writeFailingResponseWriter simulates a client that drops the connection
// mid-response: every Write fails after being recorded, so a test can pin
// that nothing retries the write or appends an error status once bytes have
// already gone out — the response is committed at that point, whether or
// not the client actually received them.
type writeFailingResponseWriter struct {
	header      http.Header
	writeCalls  int
	headerCalls []int
}

func (w *writeFailingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *writeFailingResponseWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	return 0, errors.New("simulated write failure")
}

func (w *writeFailingResponseWriter) WriteHeader(statusCode int) {
	w.headerCalls = append(w.headerCalls, statusCode)
}

func TestRenderWriteFailureIsNotReturned(t *testing.T) {
	w := &writeFailingResponseWriter{}
	err := render(w, "library.html", libraryPage{Title: "Library"})
	if err != nil {
		t.Fatalf("render returned %v after a post-write failure, want nil — the caller must not react to it", err)
	}
	if w.writeCalls != 1 {
		t.Errorf("Write called %d times, want exactly 1 (no retry)", w.writeCalls)
	}
}

func TestLibraryHandlerDoesNotDoubleWriteOnWriteFailure(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.CreateBook(context.Background(), storage.Book{
		ContentHash: "hash-1",
		Title:       "The Test Book",
		SortTitle:   "Test Book",
		Format:      "epub",
	}, []string{"Jane Doe"}); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	handler := libraryHandler(service.New(db))
	w := &writeFailingResponseWriter{}
	handler(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.writeCalls != 1 {
		t.Errorf("Write called %d times, want exactly 1 — a post-commit write failure must not be retried or followed by an error write", w.writeCalls)
	}
	for _, code := range w.headerCalls {
		if code == http.StatusInternalServerError {
			t.Errorf("WriteHeader(%d) called after a write failure — this double-writes onto an already-committed response", code)
		}
	}
}

func TestLibraryHandlerRendersCleanServerErrorOnTemplateFailure(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// library.html can't actually fail to execute against a fully-populated
	// libraryPage, so the package template set is swapped for one whose
	// "library.html" always fails, to drive the handler's error path rather
	// than render's in isolation.
	original := templates
	t.Cleanup(func() { templates = original })
	templates = template.Must(template.New("library.html").Parse(`{{.NoSuchField}}`))

	handler := Routes(service.New(db), t.TempDir(), false, false)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET / status = %d, want 500", rec.Code)
	}
	if got, want := rec.Body.String(), "internal error\n"; got != want {
		t.Errorf("GET / body = %q, want %q (no template output ahead of it)", got, want)
	}
}

func TestStaticFileServed(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/css/app.css status = %d, want 200", rec.Code)
	}

	want, err := staticFS.ReadFile("static/css/app.css")
	if err != nil {
		t.Fatalf("read embedded static/css/app.css: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("GET /static/css/app.css body does not match the embedded file content (got %d bytes, want %d)", rec.Body.Len(), len(want))
	}
}

func TestCoverServedFromCoversDir(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	coversDir := t.TempDir()
	coverBytes := []byte("not-really-a-jpeg")
	if err := os.WriteFile(filepath.Join(coversDir, "hash-1.jpg"), coverBytes, 0o644); err != nil {
		t.Fatalf("write cover: %v", err)
	}

	handler := Routes(service.New(db), coversDir, false, false)

	req := httptest.NewRequest(http.MethodGet, "/covers/hash-1.jpg", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /covers/hash-1.jpg status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), coverBytes) {
		t.Errorf("GET /covers/hash-1.jpg body = %q, want %q", rec.Body.String(), coverBytes)
	}
}

func TestStaticAndCoversDoNotListDirectories(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	coversDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(coversDir, "hash-1.jpg"), []byte("cover-bytes"), 0o644); err != nil {
		t.Fatalf("write cover: %v", err)
	}

	handler := Routes(service.New(db), coversDir, false, false)

	for _, path := range []string{"/static/", "/static/css/", "/covers/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 (no directory listing)", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<a href=") {
			t.Errorf("GET %s body contains a directory listing: %q", path, rec.Body.String())
		}
	}
}

func TestStaticAssetETagIsContentDerived(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/css/app.css status = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=300"; got != want {
		t.Errorf("GET /static/css/app.css Cache-Control = %q, want %q", got, want)
	}

	// A constant ETag would pass a weaker "non-empty, echoes back to a 304"
	// check but would keep returning 304 after the served content actually
	// changed, leaving clients stale indefinitely — so pin the documented
	// derivation (sha256 of the served body, truncated to 8 bytes, quoted)
	// rather than just its shape.
	sum := sha256.Sum256(rec.Body.Bytes())
	wantETag := fmt.Sprintf(`"%x"`, sum[:8])
	if got := rec.Header().Get("ETag"); got != wantETag {
		t.Errorf("GET /static/css/app.css ETag = %q, want %q (sha256 of the served body)", got, wantETag)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
	req2.Header.Set("If-None-Match", wantETag)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Errorf("GET /static/css/app.css with If-None-Match status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("GET /static/css/app.css with If-None-Match body = %q, want empty", rec2.Body.String())
	}
}

func TestCoverCacheControlIsBoundedNotImmutable(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	coversDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(coversDir, "hash-1.jpg"), []byte("cover-bytes"), 0o644); err != nil {
		t.Fatalf("write cover: %v", err)
	}

	handler := Routes(service.New(db), coversDir, false, false)

	req := httptest.NewRequest(http.MethodGet, "/covers/hash-1.jpg", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// cover.Store keys a cover's URL on the book's content hash, not on a
	// hash of the resized/JPEG-encoded bytes actually served there, so a
	// future change to that pipeline (or a regeneration under a changed
	// one) can overwrite different bytes at an unchanged URL. immutable
	// would misrepresent that; the header must stay a bounded max-age.
	got := rec.Header().Get("Cache-Control")
	if strings.Contains(got, "immutable") {
		t.Errorf("GET /covers/hash-1.jpg Cache-Control = %q, contains immutable — the URL is not provably stable, see cover.Store's naming", got)
	}
	if want := "public, max-age=86400"; got != want {
		t.Errorf("GET /covers/hash-1.jpg Cache-Control = %q, want %q", got, want)
	}
}

func TestCoversPathTraversalDoesNotEscapeCoversDir(t *testing.T) {
	// Exercises coversHandler directly rather than through Routes: a
	// literal ".." reaching http.ServeMux gets redirected to its cleaned
	// path one layer above this handler, which would make the same
	// request through Routes pass regardless of whether this handler's
	// own protection (inherited from http.Dir) still works.
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret"), []byte("do not serve me"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	coversDir := filepath.Join(outsideDir, "covers")
	if err := os.Mkdir(coversDir, 0o755); err != nil {
		t.Fatalf("mkdir covers: %v", err)
	}
	if err := os.WriteFile(filepath.Join(coversDir, "hash-1.jpg"), []byte("cover-bytes"), 0o644); err != nil {
		t.Fatalf("write cover: %v", err)
	}

	handler := coversHandler(coversDir)

	req := httptest.NewRequest(http.MethodGet, "/covers/../secret", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("GET /covers/../secret status = 200 body = %q, want the traversal to fail", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "do not serve me") {
		t.Errorf("GET /covers/../secret leaked the outside file: %q", rec.Body.String())
	}
}

// The library page's nav renders Library as the current item (plain text,
// aria-current), not a link — the same rule navFor documents: there is
// nowhere more useful to send someone already on the page a link would
// point to.
func TestLibraryPageNavMarksLibraryCurrent(t *testing.T) {
	handler := newTestHandlerWithBook(t, "Piranesi", []string{"Susanna Clarke"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `class="masthead__link masthead__link--current" aria-current="page">Library<`) {
		t.Errorf("GET / nav missing current Library item: %q", body)
	}
	if strings.Contains(body, `href="/">Library<`) {
		t.Errorf("GET / renders Library as a link to itself: %q", body)
	}
	if !strings.Contains(body, `href="/history">History<`) {
		t.Errorf("GET / nav missing a link to History: %q", body)
	}
}

func TestCoverURL(t *testing.T) {
	if got := coverURL("/data/covers/abc123.jpg"); got != "/covers/abc123.jpg" {
		t.Errorf("coverURL = %q, want /covers/abc123.jpg", got)
	}
	if got := coverURL(""); got != "" {
		t.Errorf("coverURL(\"\") = %q, want empty", got)
	}
}

func TestAuthorLine(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"Ursula K. Le Guin"}, "Ursula K. Le Guin"},
		{[]string{"Arkady Strugatsky", "Boris Strugatsky"}, "Arkady Strugatsky & Boris Strugatsky"},
		{[]string{"A", "B", "C"}, "A and 2 others"},
	}
	for _, c := range cases {
		if got := authorLine(c.names); got != c.want {
			t.Errorf("authorLine(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}

// embeddedFiles lists every regular file under root, so a guard over the
// shipped assets covers whatever is there rather than a list that goes stale
// the first time a template is added.
func embeddedFiles(t *testing.T, fsys fs.FS, root string) []string {
	t.Helper()
	var paths []string
	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return paths
}

// The send control, the enrichment control and the inline editors share one
// .button/.spinner system.
// A rename that stops half way leaves a retired block in the stylesheet or
// retired markup pointing at nothing, and shows up only in whichever control
// nobody happened to look at.
func TestRetiredButtonClassesAreGone(t *testing.T) {
	retired := []string{"send__button", "send__spinner", "enrich__button", "enrich__spinner"}
	trees := []struct {
		fsys fs.FS
		root string
	}{
		{templateFS, "templates"},
		{staticFS, "static/css"},
	}
	for _, tree := range trees {
		for _, path := range embeddedFiles(t, tree.fsys, tree.root) {
			b, err := fs.ReadFile(tree.fsys, path)
			if err != nil {
				t.Errorf("read %s: %v", path, err)
				continue
			}
			for _, name := range retired {
				if strings.Contains(string(b), name) {
					t.Errorf("%s still references %s; the button system is .button/.spinner", path, name)
				}
			}
		}
	}
}

// The other half of the same rename, which the check above cannot see: markup
// naming a class the stylesheet has no rule for. A mistyped modifier renders
// as a bare .button, so the control loses its size, its ground and its
// disabled treatment while every handler test stays green.
func TestButtonClassesInMarkupHaveRules(t *testing.T) {
	css, err := fs.ReadFile(staticFS, "static/css/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	// The modifier has to admit digits and further hyphens, or a name like
	// button--icon-only is captured short and reported missing, while a
	// mistyped spinner--sm2 backtracks to bare spinner and passes — the
	// guard blocking a real class and waving through the failure it exists
	// to catch.
	named := regexp.MustCompile(`\b(?:button|spinner)(?:--[a-z][a-z0-9-]*)?\b`)
	seen := make(map[string]bool)
	for _, path := range embeddedFiles(t, templateFS, "templates") {
		b, err := fs.ReadFile(templateFS, path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		for _, name := range named.FindAllString(string(b), -1) {
			if seen[name] {
				continue
			}
			seen[name] = true
			// Trailing class character excluded so .button does not match
			// the .button--lg rule and report itself as defined.
			rule := regexp.MustCompile(`\.` + regexp.QuoteMeta(name) + `[^0-9A-Za-z_-]`)
			if !rule.Match(css) {
				t.Errorf("%s names .%s, which app.css has no rule for", path, name)
			}
		}
	}
}

// A description is stored with its paragraph breaks and is the one field
// rendered as flowing prose, so the property that makes those breaks
// visible is contract rather than styling. pre-line and not pre-wrap: the
// second would also reproduce a provider's leading indentation and its
// stray double spaces.
//
// The rule has to be the read view's alone. The edit <textarea> carries
// .detail__description too, and an author white-space there overrides the
// UA's pre-wrap, so a bare .detail__description rule would collapse runs
// of spaces in a control whose submitted value keeps them.
func TestDescriptionRendersParagraphBreaks(t *testing.T) {
	css, err := fs.ReadFile(staticFS, "static/css/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}

	blocks := regexp.MustCompile(`(?s)([^{}]*)\{([^}]*)\}`).FindAllStringSubmatch(string(css), -1)
	found := false
	for _, b := range blocks {
		selector, body := strings.TrimSpace(b[1]), b[2]
		if !strings.Contains(body, "white-space: pre-line") {
			continue
		}
		if !strings.Contains(selector, "detail__description") {
			continue
		}
		found = true
		if !strings.Contains(selector, "editable__read") {
			t.Errorf("white-space: pre-line is on %q, which the edit textarea also matches", selector)
		}
	}
	if !found {
		t.Error("no rule sets white-space: pre-line on the description, so stored paragraph breaks collapse")
	}

	if regexp.MustCompile(`\.detail__description[^{]*\{[^}]*pre-wrap`).Match(css) {
		t.Error("the description uses pre-wrap, which also preserves a provider's stray whitespace")
	}
}

// The CSS above is only load-bearing if the break survives to the markup.
// A handler-side strings.Fields join or a template trim would defeat it
// with the stylesheet assertion still green.
func TestDescriptionParagraphBreakReachesTheMarkup(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	id, err := db.CreateBook(ctx, storage.Book{
		ContentHash: "hash-1", Title: "Two Paragraphs", SortTitle: "two paragraphs", Format: "epub",
		Description: "First paragraph.\n\nSecond paragraph.",
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir(), false, false)
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if want := "First paragraph.\n\nSecond paragraph."; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the blank line between paragraphs did not survive to the page; body = %q", rec.Body.String())
	}
}

// The two fetch-metadata wrappers are what turn the deployment requirement
// (an HTTPS gateway in front, the plain listener unreachable otherwise)
// into something the log can report as violated. Both are tested against a
// stub next handler rather than Routes, since what they decide is
// independent of any route and Routes itself must keep admitting an empty
// header for the opt-out mode to mean anything.
func TestRequireFetchMetadataRefusesOnlyMetadataLessMutations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		headers  map[string]string
		wantCode int
		wantNext bool
		wantBody string
		wantSwap string
	}{
		{name: "POST with no header", method: http.MethodPost, wantCode: http.StatusForbidden, wantBody: "HTTPS address"},
		// An htmx caller is refused with a 200 and a swap instruction, since
		// the vendored htmx does not swap a 4xx and a refusal nobody can see
		// is indistinguishable from a broken button. Same security property:
		// next is not called either way.
		{name: "htmx POST with no header", method: http.MethodPost, headers: map[string]string{"HX-Request": "true"},
			wantCode: http.StatusOK, wantBody: "Refused", wantSwap: "afterbegin"},
		// A history-restore request is swapped into the whole body, so it
		// is not a fragment caller and gets the plain 403 like everyone else.
		{name: "htmx history-restore POST with no header", method: http.MethodPost,
			headers:  map[string]string{"HX-Request": "true", "HX-History-Restore-Request": "true"},
			wantCode: http.StatusForbidden, wantBody: "HTTPS address"},
		{name: "POST same-origin", method: http.MethodPost, headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, wantCode: http.StatusOK, wantNext: true},
		{name: "POST none", method: http.MethodPost, headers: map[string]string{"Sec-Fetch-Site": "none"}, wantCode: http.StatusOK, wantNext: true},
		// Refusing cross-site is sameSiteOnly's job, not this wrapper's;
		// it must pass the request on so that guard still gets to answer.
		{name: "POST cross-site", method: http.MethodPost, headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, wantCode: http.StatusOK, wantNext: true},
		{name: "GET with no header", method: http.MethodGet, wantCode: http.StatusOK, wantNext: true},
		{name: "HEAD with no header", method: http.MethodHead, wantCode: http.StatusOK, wantNext: true},
		{name: "OPTIONS with no header", method: http.MethodOptions, wantCode: http.StatusOK, wantNext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := RequireFetchMetadata(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))

			req := httptest.NewRequest(tc.method, "/books/1/enrich", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if called != tc.wantNext {
				t.Errorf("next called = %v, want %v", called, tc.wantNext)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if got := rec.Header().Get("HX-Reswap"); got != tc.wantSwap {
				t.Errorf("HX-Reswap = %q, want %q", got, tc.wantSwap)
			}
		})
	}
}

func TestRequireFetchMetadataLogsEveryRefusal(t *testing.T) {
	logged := captureLog(t)
	handler := RequireFetchMetadata(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for range 2 {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/books/1/send", nil))
	}

	if got := strings.Count(logged.String(), "REQUIRE_FETCH_METADATA"); got != 2 {
		t.Errorf("logged %d refusals naming the env var, want 2:\n%s", got, logged.String())
	}
}

func TestWarnMissingFetchMetadataAdmitsAndWarnsOnce(t *testing.T) {
	logged := captureLog(t)
	calls := 0
	handler := WarnMissingFetchMetadata(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))

	// A request that does carry metadata is not what the tripwire is for
	// and must not be the one that trips it.
	withMetadata := httptest.NewRequest(http.MethodPost, "/books/1/send", nil)
	withMetadata.Header.Set("Sec-Fetch-Site", "same-origin")
	handler.ServeHTTP(httptest.NewRecorder(), withMetadata)
	if logged.Len() != 0 {
		t.Fatalf("a same-origin POST tripped the warning:\n%s", logged.String())
	}

	for range 3 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/books/1/send", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: this wrapper admits everything", rec.Code)
		}
	}

	if calls != 4 {
		t.Errorf("next called %d times, want 4", calls)
	}
	if got := strings.Count(logged.String(), "REQUIRE_FETCH_METADATA=false"); got != 1 {
		t.Errorf("warned %d times, want exactly once:\n%s", got, logged.String())
	}
}

// captureLog routes the default slog logger into a buffer for the rest of
// the test, restoring the previous logger on cleanup.
//
// It replaces the process-global logger, so it must not be called from a
// test that uses t.Parallel(), nor from one that starts goroutines which
// log after the test returns: either would race the buffer and make any
// assertion on it flaky rather than fail clearly.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })
	return &buf
}

// The brand appears in three places that have to agree: the masthead, the
// document title every page renders, and the suffix edit.js re-applies
// after an inline title edit. A rename that misses the script leaves a tab
// silently renamed the moment someone saves a title, which no handler test
// would notice.
func TestBrandIsConsistentAcrossMastheadTitleAndScript(t *testing.T) {
	const brand = "AppLibris"

	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir(), false, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "<title>Library · "+brand+"</title>") {
		t.Errorf("the document title does not carry %q; body = %q", brand, body)
	}
	if !strings.Contains(body, `<span class="masthead__brand">`+brand+`</span>`) {
		t.Errorf("the masthead does not carry %q", brand)
	}

	js, err := fs.ReadFile(staticFS, "static/js/edit.js")
	if err != nil {
		t.Fatalf("read edit.js: %v", err)
	}
	if !strings.Contains(string(js), `" · `+brand+`"`) {
		t.Errorf("edit.js re-applies a different brand than the page renders")
	}
}

// .detail__meta-row is justify-content: space-between, so a <dd> sized to
// its content is pushed to the far edge. Four of the five rows carry an
// editor whose control fills the cell; `added` is the one that does not, and
// flex: 1 on every <dd> is the only thing keeping its value level with the
// rest. It reads as redundant, which is why it is pinned.
func TestMetadataValuesTakeTheRowsFreeSpace(t *testing.T) {
	css, err := fs.ReadFile(staticFS, "static/css/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}

	blocks := regexp.MustCompile(`(?s)([^{}]*)\{([^}]*)\}`).FindAllStringSubmatch(string(css), -1)
	for _, b := range blocks {
		// The capture runs back to the previous rule, so it carries any
		// comment above this one; the selector is its last line
		lines := strings.Split(strings.TrimSpace(b[1]), "\n")
		if strings.TrimSpace(lines[len(lines)-1]) != ".detail__meta-row dd" {
			continue
		}
		if !strings.Contains(b[2], "flex: 1") {
			t.Errorf(".detail__meta-row dd does not grow, so the added row's value is pushed to the far edge; body = %q", b[2])
		}
		return
	}
	t.Error("no .detail__meta-row dd rule, so every metadata value falls back to the row's space-between")
}
