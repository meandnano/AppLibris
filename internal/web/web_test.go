package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	handler := Routes(svc, t.TempDir(), false)

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

	handler := Routes(service.New(db), t.TempDir(), false)

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

	return Routes(service.New(db), t.TempDir(), false)
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

	handler := Routes(service.New(db), t.TempDir(), false)

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
	bookID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{
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

	return Routes(service.New(db), t.TempDir(), false)
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
	handler := Routes(service.New(db), t.TempDir(), false)

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
	handler := Routes(service.New(db), t.TempDir(), false)

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

	handler := Routes(service.New(db), t.TempDir(), false)

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

	handler := Routes(service.New(db), t.TempDir(), false)

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

	handler := Routes(service.New(db), t.TempDir(), false)
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

	handler := Routes(service.New(db), t.TempDir(), false)

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

	handler := Routes(service.New(db), coversDir, false)

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

	handler := Routes(service.New(db), coversDir, false)

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

	handler := Routes(service.New(db), t.TempDir(), false)

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

	handler := Routes(service.New(db), coversDir, false)

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
