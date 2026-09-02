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

	id, _, _, err := db.CreateBookWithFile(context.Background(), storage.Book{
		ContentHash:   "hash-1",
		Title:         "The Left Hand of Darkness",
		SortTitle:     "Left Hand of Darkness, The",
		Publisher:     "Ace Books",
		PublishedDate: "1969-03-01",
		Language:      "en",
		ISBN:          "9780441478125",
		Description:   "A lone envoy arrives on the frozen world of Winter.",
		Format:        "epub",
	}, []string{"Ursula K. Le Guin"}, "left-hand.epub", 1258291, time.Now())
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
	for _, want := range []string{
		"The Left Hand of Darkness",
		"Ursula K. Le Guin",
		`class="facts__format">epub<`,
		"1.2 MB",
		"Ace Books",
		"1969-03-01",
		"9780441478125",
		`href="/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /books/%d body missing %q; body = %q", id, want, body)
		}
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
	if !strings.Contains(body, "2 paths") {
		t.Errorf("body missing the location count %q; body = %q", "2 paths", body)
	}
	if !strings.Contains(body, "a/first.epub") || !strings.Contains(body, "b/second.epub") {
		t.Errorf("body missing one or both location paths; body = %q", body)
	}
	if !strings.Contains(body, "locations__missing") {
		t.Errorf("body has no missing-location annotation for a/first.epub; body = %q", body)
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
