package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	handler := Routes(svc, t.TempDir())

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

	handler := Routes(service.New(db), t.TempDir())

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

func TestUnknownPathReturns404(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db), t.TempDir())

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

	handler := Routes(service.New(db), t.TempDir())

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

	handler := Routes(service.New(db), t.TempDir())
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

	handler := Routes(service.New(db), t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/css/app.css status = %d, want 200", rec.Code)
	}
}

func TestCoverServedFromCoversDir(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	coversDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(coversDir, "hash-1.jpg"), []byte("not-really-a-jpeg"), 0o644); err != nil {
		t.Fatalf("write cover: %v", err)
	}

	handler := Routes(service.New(db), coversDir)

	req := httptest.NewRequest(http.MethodGet, "/covers/hash-1.jpg", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /covers/hash-1.jpg status = %d, want 200", rec.Code)
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
