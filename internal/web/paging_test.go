package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"library/internal/service"
	"library/internal/storage"
)

func newPagingTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedBooks creates n books whose sort_titles order predictably —
// "book 000" through "book NNN" — so a test can name which page a given
// title should land on.
func seedBooks(t *testing.T, db *storage.DB, n int, titleSuffix string) {
	t.Helper()
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("book %03d %s", i, titleSuffix)
		if _, err := db.CreateBook(context.Background(), storage.Book{
			ContentHash: fmt.Sprintf("hash-%s-%03d", titleSuffix, i),
			Title:       title,
			SortTitle:   title,
			Format:      "epub",
		}, nil); err != nil {
			t.Fatalf("CreateBook %d: %v", i, err)
		}
	}
}

func get(handler http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func countCards(body string) int {
	return strings.Count(body, `<li class="card">`)
}

func TestLibraryPageIsBoundedAndCarriesATrigger(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, pageSize+10, "x")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	rec := get(handler, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()

	if got := countCards(body); got != pageSize {
		t.Errorf("cards on page one = %d, want %d", got, pageSize)
	}
	if !strings.Contains(body, `class="grid__more"`) {
		t.Error("page one carries no reveal trigger, so nothing would load the rest")
	}
	if !strings.Contains(body, fmt.Sprintf("Loading next %d of %d", pageSize, pageSize+10)) {
		t.Errorf("trigger label missing or wrong; body has %q", triggerLine(body))
	}
}

// The trigger is one element carrying both affordances — the same
// single-markup-path rule the read views follow. Without JavaScript an
// unpaged grid was the only thing that worked here at all, so losing the
// href would make the no-JS case strictly worse than before paging.
func TestTriggerCarriesBothAnHrefAndHTMXAttributes(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, pageSize+1, "x")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	line := triggerLine(get(handler, "/", nil).Body.String())
	for _, want := range []string{`href="/?`, `hx-get="/?`, `hx-trigger="revealed"`, `hx-target="this"`, `hx-swap="outerHTML"`} {
		if !strings.Contains(line, want) {
			t.Errorf("trigger missing %s; line = %q", want, line)
		}
	}
	if !strings.Contains(line, appendParam+"=1") {
		t.Errorf("the hx-get must ask for the append fragment, not the whole grid; line = %q", line)
	}
}

func TestLastPageCarriesNoTrigger(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, 3, "x")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	body := get(handler, "/", nil).Body.String()
	if countCards(body) != 3 {
		t.Fatalf("cards = %d, want 3", countCards(body))
	}
	if strings.Contains(body, "grid__more") {
		t.Error("a library smaller than one page still renders a trigger, which would offer zero more books")
	}
}

// Paging through every page must see each book exactly once. This is the
// test that would catch an off-by-one at the page boundary, which is
// invisible with a single page of books.
func TestPagingThroughTheWholeLibrarySeesEachBookOnce(t *testing.T) {
	db := newPagingTestDB(t)
	const total = pageSize*2 + 7
	seedBooks(t, db, total, "x")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	seen := map[string]int{}
	target := "/"
	for i := 0; ; i++ {
		if i > 10 {
			t.Fatal("paging did not terminate — the cursor is not advancing")
		}
		body := get(handler, target, map[string]string{"HX-Request": "true"}).Body.String()
		for _, id := range cardIDs(body) {
			seen[id]++
		}
		next := hxGetURL(triggerLine(body))
		if next == "" {
			break
		}
		target = next
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct books across all pages, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("book %s appeared %d times across pages, want once", id, n)
		}
	}
}

// The append response is a batch of <li>s, not a whole grid: it replaces
// the trigger in place, so a #book-grid wrapper would nest one grid inside
// another.
func TestAppendResponseIsCardsOnly(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, pageSize+5, "x")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	next := hxGetURL(triggerLine(get(handler, "/", nil).Body.String()))
	if next == "" {
		t.Fatal("no trigger on page one")
	}

	body := get(handler, next, map[string]string{"HX-Request": "true"}).Body.String()
	if got := countCards(body); got != 5 {
		t.Errorf("append response has %d cards, want the remaining 5", got)
	}
	if strings.Contains(body, `id="book-grid"`) {
		t.Error("the append response wraps a whole grid, which would nest a grid inside the list")
	}
	if strings.Contains(body, "<ul") {
		t.Error("the append response opens its own list; it must be <li>s destined for the existing one")
	}
}

func TestSearchResultsPageAndCarryTheQuery(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, pageSize+10, "novel")
	seedBooks(t, db, 3, "other")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	body := get(handler, "/?q=novel", nil).Body.String()
	if got := countCards(body); got != pageSize {
		t.Errorf("search page one cards = %d, want %d", got, pageSize)
	}

	line := triggerLine(body)
	next := hxGetURL(line)
	if next == "" {
		t.Fatal("a search matching more than a page carries no trigger")
	}
	parsed, err := url.Parse(next)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("q") != "novel" {
		t.Errorf("next-page URL drops the query (%q), so page two would come from the unfiltered library", next)
	}

	// The label counts what matched, not the library total: "of 61" beside
	// a grid filtered to 58 books would name a number nothing on screen
	// refers to.
	if !strings.Contains(line, fmt.Sprintf("of %d", pageSize+10)) {
		t.Errorf("trigger label should count the matches (%d), not the library total; line = %q", pageSize+10, line)
	}
}

// A keystroke replaces the whole grid, trigger included, so paging resets
// by construction. A stale trigger left behind would append page two of
// the *previous* query — invisible until it breaks, hence the test.
func TestNewSearchResetsPaging(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, pageSize+10, "novel")
	seedBooks(t, db, pageSize+10, "essay")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// Page into the first query, then run a different one.
	first := get(handler, "/?q=novel", map[string]string{"HX-Request": "true"}).Body.String()
	if next := hxGetURL(triggerLine(first)); next == "" {
		t.Fatal("no trigger for the first query")
	}

	second := get(handler, "/?q=essay", map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(second, `id="book-grid"`) {
		t.Error("a search response must rebuild the whole grid, which is what resets paging")
	}
	line := triggerLine(second)
	if strings.Contains(line, "novel") {
		t.Errorf("the rebuilt trigger still points at the previous query; line = %q", line)
	}
	if !strings.Contains(line, "essay") {
		t.Errorf("the rebuilt trigger does not carry the new query; line = %q", line)
	}
	// No cursor from the previous query survives either.
	if strings.Contains(line, "after=book+000") {
		t.Errorf("the rebuilt trigger carries a stale cursor; line = %q", line)
	}
}

// The no-JS path: following the plain href yields a whole page whose grid
// starts at the cursor.
func TestPlainNavigationToTheNextPageRendersAWholePage(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, pageSize+5, "x")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	href := hrefURL(triggerLine(get(handler, "/", nil).Body.String()))
	if href == "" {
		t.Fatal("no plain href on the trigger")
	}
	if strings.Contains(href, appendParam+"=1") {
		t.Error("the plain href asks for the append fragment; it must be a whole page")
	}

	body := get(handler, href, nil).Body.String()
	if !strings.Contains(body, "<!doctype html>") {
		t.Error("following the plain href did not render a whole page")
	}
	if got := countCards(body); got != 5 {
		t.Errorf("cards on the second page = %d, want the remaining 5", got)
	}
	if !strings.Contains(body, "masthead") {
		t.Error("the second page lost the masthead, so there would be no way to navigate")
	}
}

// A mangled cursor names no resource, so it shows the library rather than
// 400ing — the same call ?edit= makes on the book page.
func TestMalformedCursorFallsBackToTheFirstPage(t *testing.T) {
	db := newPagingTestDB(t)
	seedBooks(t, db, 3, "x")
	handler := Routes(service.New(db), t.TempDir(), false, false)

	rec := get(handler, "/?after_id=not-a-number", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with a malformed cursor = %d, want 200", rec.Code)
	}
	if got := countCards(rec.Body.String()); got != 3 {
		t.Errorf("cards = %d, want all 3", got)
	}
}

// triggerLine returns the line of body holding the reveal trigger, or "".
func triggerLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "grid__more-link") {
			return line
		}
	}
	return ""
}

func attrValue(line, attr string) string {
	marker := attr + `="`
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := line[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	// html/template escapes the attribute, and a query built by
	// url.Values encodes spaces as "+", which comes out as "&#43;". A
	// browser decodes that; so must this, or url.Parse sees the "#" and
	// treats the rest of the URL as a fragment.
	return html.UnescapeString(rest[:j])
}

func hxGetURL(line string) string { return attrValue(line, "hx-get") }
func hrefURL(line string) string  { return attrValue(line, "href") }

// cardIDs pulls each card's book id out of its link, so a test can check
// which books a page actually showed.
func cardIDs(body string) []string {
	var ids []string
	const marker = `<a class="card__link" href="/books/`
	rest := body
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			return ids
		}
		rest = rest[i+len(marker):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return ids
		}
		ids = append(ids, rest[:j])
	}
}
