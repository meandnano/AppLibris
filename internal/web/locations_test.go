package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"library/internal/service"
	"library/internal/storage"
)

// newLocationsTestBook returns a handler and a book with two locations, the
// first of which (a/first.epub) is marked missing, plus that row's id.
func newLocationsTestBook(t *testing.T) (http.Handler, *storage.DB, int64, int64) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	mtime := time.Now()

	id, _, _, _, err := db.CreateBookWithFile(ctx, storage.Book{
		ContentHash: "hash-1", Title: "Two Locations", SortTitle: "two locations", Format: "epub",
	}, nil, "b/second.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	missingID, err := db.UpsertBookFile(ctx, id, "a/first.epub", 100, mtime)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if err := db.SetFilesMissing(ctx, []int64{missingID}, mtime); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}

	return Routes(service.New(db), t.TempDir(), false, false), db, id, missingID
}

func postForgetLocation(handler http.Handler, bookID, fileID int64, hx bool, fetchSite string) *httptest.ResponseRecorder {
	form := url.Values{"file": {itoa(fileID)}}
	req := httptest.NewRequest(http.MethodPost, "/books/"+itoa(bookID)+"/locations/forget", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// Forgetting is a delete, so the method is part of the contract and the
// cross-site guard has to be on the route rather than assumed from the
// pattern: there is no login, and the header is the only thing between the
// collection and any page the browser happens to have open.
func TestForgetLocationRoutePostOnlyAndSameSite(t *testing.T) {
	handler, db, bookID, fileID := newLocationsTestBook(t)
	ctx := context.Background()

	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(bookID)+"/locations/forget", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET forget = %d, want 405", rec.Code)
	}

	if rec := postForgetLocation(handler, bookID, fileID, false, "cross-site"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", rec.Code)
	}
	if f, err := db.FindFileByPath(ctx, "a/first.epub"); err != nil || f == nil {
		t.Errorf("FindFileByPath = %+v, %v; want the row untouched by a refused POST", f, err)
	}

	// The three headers sameSiteOnly admits, including the empty one the
	// opt-out mode depends on. Each is checked against a fresh row, since
	// the first one through actually forgets it.
	for _, fetchSite := range []string{"same-origin", "none", ""} {
		handler, db, bookID, fileID := newLocationsTestBook(t)
		if rec := postForgetLocation(handler, bookID, fileID, false, fetchSite); rec.Code != http.StatusSeeOther {
			t.Errorf("Sec-Fetch-Site %q: POST forget = %d, want 303", fetchSite, rec.Code)
		}
		if f, err := db.FindFileByPath(ctx, "a/first.epub"); err != nil || f != nil {
			t.Errorf("Sec-Fetch-Site %q: FindFileByPath = %+v, %v; want the row gone", fetchSite, f, err)
		}
	}
}

func TestForgetLocationFragmentRendersLocations(t *testing.T) {
	handler, db, bookID, fileID := newLocationsTestBook(t)
	ctx := context.Background()

	rec := postForgetLocation(handler, bookID, fileID, true, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx POST forget = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<dd id="locations">`) {
		t.Errorf("fragment is not the swap target's own element; body = %q", body)
	}
	if !strings.Contains(body, "<summary>1 path</summary>") {
		t.Errorf("fragment still counts the forgotten path; body = %q", body)
	}
	if strings.Contains(body, "a/first.epub") {
		t.Errorf("fragment still lists the forgotten path; body = %q", body)
	}
	if !strings.Contains(body, "b/second.epub") {
		t.Errorf("fragment dropped the surviving path; body = %q", body)
	}
	// A whole page would carry the masthead; this must be the fragment.
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("htmx got the whole page rather than the locations fragment; body = %q", body)
	}

	if f, err := db.FindFileByPath(ctx, "a/first.epub"); err != nil || f != nil {
		t.Errorf("FindFileByPath = %+v, %v; want the row gone", f, err)
	}
}

// Forgetting the last location prunes the book, and there is then no page
// to go back to — a fragment cannot be swapped into one whose subject does
// not exist either, so htmx is told to navigate rather than swap.
func TestForgetLocationRedirectsWhenBookPruned(t *testing.T) {
	for _, hx := range []bool{false, true} {
		db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
		if err != nil {
			t.Fatalf("storage.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		ctx := context.Background()
		mtime := time.Now()

		bookID, _, _, _, err := db.CreateBookWithFile(ctx, storage.Book{
			ContentHash: "hash-1", Title: "Only Copy", SortTitle: "only copy", Format: "epub",
		}, nil, "gone.epub", 100, mtime)
		if err != nil {
			t.Fatalf("CreateBookWithFile: %v", err)
		}
		file, err := db.FindFileByPath(ctx, "gone.epub")
		if err != nil || file == nil {
			t.Fatalf("FindFileByPath = %+v, %v", file, err)
		}
		if err := db.SetFilesMissing(ctx, []int64{file.ID}, mtime); err != nil {
			t.Fatalf("SetFilesMissing: %v", err)
		}

		handler := Routes(service.New(db), t.TempDir(), false, false)
		rec := postForgetLocation(handler, bookID, file.ID, hx, "same-origin")

		if hx {
			if rec.Code != http.StatusOK {
				t.Errorf("htmx POST = %d, want 200 carrying HX-Redirect", rec.Code)
			}
			if got := rec.Header().Get("HX-Redirect"); got != "/" {
				t.Errorf("HX-Redirect = %q, want /", got)
			}
		} else {
			if rec.Code != http.StatusSeeOther {
				t.Errorf("POST = %d, want 303", rec.Code)
			}
			if got := rec.Header().Get("Location"); got != "/" {
				t.Errorf("Location = %q, want /", got)
			}
		}

		if book, err := db.FindBookByID(ctx, bookID); err != nil || book != nil {
			t.Errorf("FindBookByID = %+v, %v; want the book pruned", book, err)
		}
	}
}

// The button is the rule made visible: a path that is there is not
// something to forget, and offering it would only invite a click the delete
// then refuses.
func TestForgetButtonOnlyBesideMissingRows(t *testing.T) {
	handler, _, bookID, fileID := newLocationsTestBook(t)

	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(bookID), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()

	if got := strings.Count(body, `class="locations__forget-form"`); got != 1 {
		t.Errorf("forget form appears %d times, want exactly 1 — only a/first.epub is missing; body = %q", got, body)
	}
	if want := `<input type="hidden" name="file" value="` + itoa(fileID) + `">`; !strings.Contains(body, want) {
		t.Errorf("body missing the forget form's file id %q; body = %q", want, body)
	}
	if want := `hx-target="#locations"`; !strings.Contains(body, want) {
		t.Errorf("forget form does not target the locations block; body = %q", body)
	}
	// Both an action and an hx-post, the one-markup-path rule every other
	// control follows.
	if want := `action="/books/` + itoa(bookID) + `/locations/forget"`; !strings.Contains(body, want) {
		t.Errorf("forget form has no plain action for the no-JavaScript path; body = %q", body)
	}
}
