package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"library/internal/service"
	"library/internal/storage"
)

func TestBookDetailHandlerRendersFullMetadata(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	id, _, _, err := db.CreateBookWithFile(ctx, storage.Book{
		ContentHash:   "hash-1",
		Title:         "The Left Hand of Darkness",
		SortTitle:     "Left Hand of Darkness, The",
		Publisher:     "Ace Books",
		PublishedDate: "1969-03-01",
		Language:      "en",
		ISBN:          "9780441478125",
		Description:   "A lone envoy arrives on the frozen world of Winter.",
		CoverPath:     "/data/covers/hash-1.jpg",
		Format:        "epub",
	}, []string{"Ursula K. Le Guin"}, "left-hand.epub", 1258291, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	svc := service.New(db)
	handler := Routes(svc, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books/%d status = %d, want 200", id, rec.Code)
	}

	// added_at is set by the schema default, so the expected date comes from
	// what the service actually read back rather than from a literal — which
	// would either hard-code today or race the clock at midnight.
	detail, err := svc.GetBook(ctx, id)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"The Left Hand of Darkness",
		"Ursula K. Le Guin",
		`class="facts__format">epub<`,
		"1.2 MB",
		"Ace Books",
		"1969-03-01",
		"en",
		"9780441478125",
		"A lone envoy arrives on the frozen world of Winter.",
		`<img class="detail__cover" src="/covers/hash-1.jpg"`,
		detail.AddedAt.Format("2006-01-02"),
		`href="/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /books/%d body missing %q; body = %q", id, want, body)
		}
	}
	if strings.Contains(body, "detail__cover--missing") {
		t.Error("a book with a stored cover rendered the no-cover box")
	}
}

// The detail page's whole reason for a larger cover rail is that a book
// without one still has to hold the layout: plate 04 draws the dashed
// "no cover" box at the same footprint as a real cover.
func TestBookDetailHandlerRendersNoCoverBox(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	id, _, _, err := db.CreateBookWithFile(context.Background(), storage.Book{
		ContentHash: "hash-1", Title: "No Cover", SortTitle: "No Cover", Format: "fb2",
	}, nil, "no-cover.fb2", 2048, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<div class="detail__cover detail__cover--missing"><span>no cover</span></div>`) {
		t.Errorf("coverless book missing the no-cover box; body = %q", body)
	}
	if strings.Contains(body, "<img class=\"detail__cover\"") {
		t.Error("coverless book rendered a cover image")
	}
}

// Every other detail fixture has one author, so the collapse-free,
// source-ordered rendering fullAuthorLine exists for is only pinned here:
// swapping in the grid's "and N others" helper, sorting the names, or
// dropping the tail all have to fail.
func TestBookDetailHandlerRendersEveryAuthorInSourceOrder(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Deliberately not alphabetical, so a sort is distinguishable from the
	// source order the file credited.
	authors := []string{"Zoe Quinn", "Adam Bell", "Mary Shelley"}
	id, _, _, err := db.CreateBookWithFile(context.Background(), storage.Book{
		ContentHash: "hash-1", Title: "Three Authors", SortTitle: "Three Authors", Format: "epub",
	}, authors, "three.epub", 4096, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if want := "Zoe Quinn, Adam Bell &amp; Mary Shelley"; !strings.Contains(body, want) {
		t.Errorf("author line missing %q; body = %q", want, body)
	}
	for _, name := range authors {
		if got := strings.Count(body, name); got != 1 {
			t.Errorf("author %q appears %d times, want exactly 1", name, got)
		}
	}
	if strings.Contains(body, "others") {
		t.Error("detail page collapsed the author list; it has room to name everyone")
	}
}

func TestBookDetailHandlerSparseMetadataShowsEmDashRows(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	id, _, _, err := db.CreateBookWithFile(context.Background(), storage.Book{
		ContentHash: "hash-1",
		Title:       "Sparse Book",
		SortTitle:   "Sparse Book",
		Format:      "fb2",
	}, nil, "sparse.fb2", 1024, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books/%d status = %d, want 200", id, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Author unknown") {
		t.Errorf("sparse book body missing %q", "Author unknown")
	}
	if !strings.Contains(body, "No description") {
		t.Errorf("sparse book body missing %q", "No description")
	}
	if strings.Contains(body, "click to add") {
		t.Errorf("sparse book body contains %q — that wording belongs to the inline-editing step, not built yet", "click to add")
	}
	if got := strings.Count(body, "detail__meta-empty"); got != 4 {
		t.Errorf("sparse book body has %d empty-field em-dash rows (publisher/published/language/isbn), want 4; body = %q", got, body)
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("sparse book body has no em dash for the empty metadata rows")
	}
}

func TestBookDetailHandler404sOnNonNumericAndUnknownID(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir())

	for _, path := range []string{"/books/not-a-number", "/books/99999"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestGridCardsLinkToBookDetail(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	id, err := db.CreateBook(context.Background(), storage.Book{
		ContentHash: "hash-1", Title: "A Book", SortTitle: "A Book", Format: "epub",
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	want := `href="/books/` + itoa(id) + `"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("grid card missing link %q; body = %q", want, rec.Body.String())
	}
}

func TestBookDetailHandlerShowsLocationsAndMissingAnnotation(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	mtime := time.Now()

	id, _, _, err := db.CreateBookWithFile(ctx, storage.Book{
		ContentHash: "hash-1", Title: "Two Locations", SortTitle: "Two Locations", Format: "epub",
	}, nil, "b/second.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	firstFileID, err := db.UpsertBookFile(ctx, id, "a/first.epub", 100, mtime)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if err := db.SetFilesMissing(ctx, []int64{firstFileID}, mtime); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()

	// The reveal is a native <details>/<summary> on purpose — no JS is
	// guaranteed to have loaded — so the structure is part of the contract,
	// not an implementation detail: listing the paths inline would satisfy
	// a bare "contains the paths" check.
	if !strings.Contains(body, `<details class="locations">`) {
		t.Errorf("locations are not inside a <details>; body = %q", body)
	}
	if want := `<summary>2 paths</summary>`; !strings.Contains(body, want) {
		t.Errorf("body missing the location count %q; body = %q", want, body)
	}

	// Whole list items, so the annotation has to land on the marked path
	// and only on it — asserting the class appears somewhere would pass
	// with it attached to every path, or to the wrong one.
	if want := `<li>a/first.epub <span class="locations__missing">missing</span></li>`; !strings.Contains(body, want) {
		t.Errorf("body missing the annotated missing location %q; body = %q", want, body)
	}
	if want := `<li>b/second.epub</li>`; !strings.Contains(body, want) {
		t.Errorf("body missing the present location %q, unannotated; body = %q", want, body)
	}
	if got := strings.Count(body, "locations__missing"); got != 1 {
		t.Errorf("missing annotation appears %d times, want exactly 1 — only a/first.epub is missing", got)
	}
}

// A book with no location at all: unreachable in normal operation, since
// deleting the last location prunes the book in the same transaction, but
// reachable through CreateBook and through the read race GetBook
// documents. The size is unknown there, which is not the same as a
// zero-byte file, and the page must not claim "0 B".
func TestBookDetailHandlerRendersNoSizeWhenBookHasNoLocation(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	id, err := db.CreateBook(context.Background(), storage.Book{
		ContentHash: "hash-1", Title: "No Locations", SortTitle: "No Locations", Format: "epub",
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books/%d status = %d, want 200", id, rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "0 B") {
		t.Errorf("book with no location rendered a zero size; body = %q", body)
	}
	if strings.Contains(body, "0 bytes") {
		t.Error(`book with no location rendered title="0 bytes"`)
	}
	if !strings.Contains(body, `<span class="facts__empty">&mdash;</span>`) {
		t.Errorf("book with no location is missing the em-dash size row; body = %q", body)
	}
	if want := `<summary>0 paths</summary>`; !strings.Contains(body, want) {
		t.Errorf("body missing %q; body = %q", want, body)
	}
}

// The counterpart: a location that really is zero bytes still reads "0 B".
func TestBookDetailHandlerRendersZeroByteFileAsZeroBytes(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	id, _, _, err := db.CreateBookWithFile(context.Background(), storage.Book{
		ContentHash: "hash-1", Title: "Empty File", SortTitle: "Empty File", Format: "epub",
	}, nil, "empty.epub", 0, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, ">0 B<") {
		t.Errorf("a genuinely zero-byte location should still render 0 B; body = %q", body)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1258291, "1.2 MB"},
		{1024, "1.0 KB"},
		// One byte short of each binary unit: the unrounded ratio is just
		// under the threshold (1023.999...), but rounds to "1024.0" at one
		// decimal place if the unit isn't bumped to compensate.
		{1024*1024 - 1, "1.0 MB"},
		{1024*1024*1024 - 1, "1.0 GB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}
	for _, c := range cases {
		if got := humanSize(c.bytes); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}
