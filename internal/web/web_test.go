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

	handler := Routes(service.New(db), coversDir)

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

	handler := Routes(service.New(db), coversDir)

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

	handler := Routes(service.New(db), t.TempDir())

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

	handler := Routes(service.New(db), coversDir)

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
