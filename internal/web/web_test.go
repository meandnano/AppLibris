package web

import (
	"context"
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
