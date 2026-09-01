package web

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	}, nil); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	svc := service.New(db)
	handler := Routes(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "The Test Book") {
		t.Errorf("GET / body = %q, want it to contain %q", body, "The Test Book")
	}
}

func TestStaticFileServed(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler := Routes(service.New(db))

	req := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/css/app.css status = %d, want 200", rec.Code)
	}
}
