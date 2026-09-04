package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"library/internal/service"
	"library/internal/storage"
)

func newHistoryTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The table plate 07's timestamp format needs: same day, previous
// calendar day, older, and — the case a naive now.Sub(t) < 24*time.Hour
// implementation gets wrong — 23:50 the previous calendar day viewed only
// twenty minutes later, once the date has turned over.
//
// Every case builds its times in time.Local (not time.UTC) so the test's
// result does not depend on which zone happens to be configured on the
// machine running it — relativeTime converts to time.Local internally, so
// constructing already-local times makes that conversion a no-op here.
func TestRelativeTime(t *testing.T) {
	loc := time.Local
	cases := []struct {
		name string
		now  time.Time
		at   time.Time
		want string
	}{
		{
			name: "same calendar day",
			now:  time.Date(2026, 9, 3, 14, 0, 0, 0, loc),
			at:   time.Date(2026, 9, 3, 9, 15, 0, 0, loc),
			want: "today, 09:15",
		},
		{
			name: "previous calendar day",
			now:  time.Date(2026, 9, 3, 14, 0, 0, 0, loc),
			at:   time.Date(2026, 9, 2, 22, 41, 0, 0, loc),
			want: "yesterday, 22:41",
		},
		{
			name: "older than yesterday",
			now:  time.Date(2026, 9, 3, 14, 0, 0, 0, loc),
			at:   time.Date(2026, 8, 28, 9, 15, 0, 0, loc),
			want: "28 Aug, 09:15",
		},
		{
			name: "23:50 yesterday viewed at 00:10 today",
			now:  time.Date(2026, 9, 3, 0, 10, 0, 0, loc),
			at:   time.Date(2026, 9, 2, 23, 50, 0, 0, loc),
			want: "yesterday, 23:50",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeTime(tc.at, tc.now); got != tc.want {
				t.Errorf("relativeTime(%v, %v) = %q, want %q", tc.at, tc.now, got, tc.want)
			}
		})
	}
}

func TestHistoryEmptyState(t *testing.T) {
	db := newHistoryTestDB(t)
	handler := Routes(service.New(db), t.TempDir(), true, false)

	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /history status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Nothing sent yet") {
		t.Errorf("GET /history empty body = %q, want the empty state", body)
	}
	if strings.Contains(body, "No books yet") {
		t.Errorf("GET /history empty body reuses the library's empty state: %q", body)
	}
}

func TestHistoryRendersDeliveredFailedAndSendingRows(t *testing.T) {
	db := newHistoryTestDB(t)
	ctx := context.Background()
	now := time.Now()

	deliveredID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-1", Title: "Delivered Book", Format: "epub"}, nil, "a.epub", 100, now)
	if err != nil {
		t.Fatalf("CreateBookWithFile delivered: %v", err)
	}
	deliveredSendID, err := db.EnqueueSend(ctx, deliveredID, "Delivered Book", "reader@kindle.com", now.Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("EnqueueSend delivered: %v", err)
	}
	claimed, err := db.ClaimNextSend(ctx, now.Add(-3*time.Hour).Add(time.Minute))
	if err != nil || claimed == nil || claimed.ID != deliveredSendID {
		t.Fatalf("ClaimNextSend delivered: %+v, %v", claimed, err)
	}
	if err := db.MarkSendDelivered(ctx, deliveredSendID, "msg-1", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("MarkSendDelivered: %v", err)
	}

	failedID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-2", Title: "Failed Book", Format: "epub"}, nil, "b.epub", 100, now)
	if err != nil {
		t.Fatalf("CreateBookWithFile failed: %v", err)
	}
	failedSendID, err := db.EnqueueSend(ctx, failedID, "Failed Book", "reader@kindle.com", now.Add(-90*time.Minute))
	if err != nil {
		t.Fatalf("EnqueueSend failed: %v", err)
	}
	if claimed, err := db.ClaimNextSend(ctx, now.Add(-90*time.Minute).Add(time.Minute)); err != nil || claimed == nil || claimed.ID != failedSendID {
		t.Fatalf("ClaimNextSend failed: %+v, %v", claimed, err)
	}
	if err := db.MarkSendFailed(ctx, failedSendID, "14.2 MB exceeds the 28 MB limit", now.Add(-80*time.Minute)); err != nil {
		t.Fatalf("MarkSendFailed: %v", err)
	}

	sendingID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-3", Title: "Sending Book", Format: "epub"}, nil, "c.epub", 100, now)
	if err != nil {
		t.Fatalf("CreateBookWithFile sending: %v", err)
	}
	if _, err := db.EnqueueSend(ctx, sendingID, "Sending Book", "reader@kindle.com", now.Add(-time.Minute)); err != nil {
		t.Fatalf("EnqueueSend sending: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir(), true, false)
	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /history status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Delivered Book") || !strings.Contains(body, `history__status--ok">Delivered<`) {
		t.Errorf("GET /history missing the delivered row: %q", body)
	}
	if !strings.Contains(body, "Failed Book") || !strings.Contains(body, `history__status--err">Failed<`) {
		t.Errorf("GET /history missing the failed row: %q", body)
	}
	if !strings.Contains(body, "14.2 MB exceeds the 28 MB limit") {
		t.Errorf("GET /history failed row missing its reason: %q", body)
	}
	if !strings.Contains(body, "Sending Book") || !strings.Contains(body, `history__status--pending">Sending<`) {
		t.Errorf("GET /history missing the sending row (queued renders as Sending): %q", body)
	}
	if strings.Contains(body, ">Queued<") {
		t.Errorf("GET /history renders a literal Queued label; the built control's Sending collapse should win: %q", body)
	}
}

// A send whose book has since been pruned still has a row — send_log
// denormalises the title precisely so it survives — but with no link,
// since there is nowhere for it to point.
func TestHistoryRowForDeletedBookRendersWithoutALink(t *testing.T) {
	db := newHistoryTestDB(t)
	ctx := context.Background()
	now := time.Now()

	bookID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-1", Title: "Vanishing Book", Format: "epub"}, nil, "a.epub", 100, now)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if _, err := db.EnqueueSend(ctx, bookID, "Vanishing Book", "reader@kindle.com", now.Add(-time.Hour)); err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	f, err := db.FindFileByPath(ctx, "a.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}
	if _, _, err := db.PruneMissingFiles(ctx, []int64{f.ID}); err != nil {
		t.Fatalf("PruneMissingFiles: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir(), true, false)
	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Vanishing Book") {
		t.Fatalf("GET /history missing the deleted book's row: %q", body)
	}
	if strings.Contains(body, `<a class="history__title" href="/books/`) {
		t.Errorf("GET /history links a row whose book is gone: %q", body)
	}
	if !strings.Contains(body, `history__title--gone`) {
		t.Errorf("GET /history row for a deleted book missing the unlinked styling hook: %q", body)
	}
}

func TestHistoryScopeLineNamesTheCapOnlyWhenTruncated(t *testing.T) {
	db := newHistoryTestDB(t)
	ctx := context.Background()
	now := time.Now()

	bookID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, now)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	for i := 0; i < service.SendHistoryLimit+1; i++ {
		at := now.Add(-time.Duration(service.SendHistoryLimit-i) * time.Second)
		if _, err := db.EnqueueSend(ctx, bookID, "Book", fmt.Sprintf("reader%d@kindle.com", i), at); err != nil {
			t.Fatalf("EnqueueSend %d: %v", i, err)
		}
	}

	handler := Routes(service.New(db), t.TempDir(), true, false)
	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	want := fmt.Sprintf("last 30 days · %d most recent", service.SendHistoryLimit)
	if !strings.Contains(body, want) {
		t.Errorf("GET /history scope line missing %q: %q", want, body)
	}
}

func TestHistoryScopeLineIsPlainWhenNotTruncated(t *testing.T) {
	db := newHistoryTestDB(t)
	ctx := context.Background()
	now := time.Now()

	bookID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, now)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if _, err := db.EnqueueSend(ctx, bookID, "Book", "reader@kindle.com", now.Add(-time.Hour)); err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir(), true, false)
	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "last 30 days<") {
		t.Errorf("GET /history scope line = %q, want the plain \"last 30 days\"", body)
	}
	if strings.Contains(body, "most recent") {
		t.Errorf("GET /history scope line names the cap without truncation: %q", body)
	}
}

// GET /history has to work whether or not sending is configured: it is a
// log, not an action.
func TestHistoryRendersWithSendingDisabled(t *testing.T) {
	db := newHistoryTestDB(t)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /history (sending disabled) status = %d, want 200", rec.Code)
	}
}

// The History page's own nav item should read current, and Library (now
// that a second page exists) should be a real link back.
func TestHistoryPageNavMarksHistoryCurrent(t *testing.T) {
	db := newHistoryTestDB(t)
	handler := Routes(service.New(db), t.TempDir(), true, false)

	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `class="masthead__link masthead__link--current" aria-current="page">History<`) {
		t.Errorf("GET /history nav missing current History item: %q", body)
	}
	if !strings.Contains(body, `href="/">Library<`) {
		t.Errorf("GET /history nav missing a link back to Library: %q", body)
	}
}
