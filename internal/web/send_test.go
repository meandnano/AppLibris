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

func newSendTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var sendTestBookSeq int

func createSendTestBook(t *testing.T, db *storage.DB) int64 {
	t.Helper()
	sendTestBookSeq++
	hash := "hash-" + itoa(int64(sendTestBookSeq))
	id, _, _, err := db.CreateBookWithFile(context.Background(), storage.Book{
		ContentHash: hash, Title: "Piranesi", SortTitle: "Piranesi", Format: "epub",
	}, []string{"Susanna Clarke"}, hash+".epub", 1024, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	return id
}

func postSendForm(handler http.Handler, id int64, form url.Values, hx bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/books/"+itoa(id)+"/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestSendHandlerEnqueuesAndReturnsSendingFragment(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := Routes(service.New(db), t.TempDir(), true)

	rec := postSendForm(handler, id, url.Values{"recipient": {"reader@kindle.com"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST send status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-get="/books/`+itoa(id)+`/sends/`) {
		t.Errorf("sending fragment missing its poll hx-get; body = %q", body)
	}
	if !strings.Contains(body, "reader@kindle.com") {
		t.Errorf("sending fragment missing the recipient; body = %q", body)
	}

	send, err := db.LatestSendForBook(context.Background(), id)
	if err != nil || send == nil {
		t.Fatalf("LatestSendForBook: %+v, %v", send, err)
	}
	if send.RecipientAddress != "reader@kindle.com" {
		t.Errorf("queued send recipient = %q, want reader@kindle.com", send.RecipientAddress)
	}
}

// The status box's own hx-get (and its load-delay trigger) is what makes
// polling automatic; once a send is terminal that element is replaced by
// the plain delivered/failed block, so nothing re-arms another request.
// The form's own hx-post survives (Send again / Retry stays htmx-enhanced)
// — this test is about the *polling* contract specifically, not every
// hx-* attribute in the fragment.
func TestSendStatusHandlerTerminalFragmentDoesNotRepoll(t *testing.T) {
	db := newSendTestDB(t)
	ctx := context.Background()
	id := createSendTestBook(t, db)

	sendID, err := db.EnqueueSend(ctx, id, "Piranesi", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	claimed, err := db.ClaimNextSend(ctx, time.Now())
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextSend: %+v, %v", claimed, err)
	}
	if err := db.MarkSendDelivered(ctx, sendID, "msg_1", time.Now()); err != nil {
		t.Fatalf("MarkSendDelivered: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir(), true)
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"/sends/"+itoa(sendID), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET poll status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `hx-trigger="load`) {
		t.Errorf("terminal (delivered) fragment still carries a load-triggered poll; body = %q", body)
	}
	if !strings.Contains(body, "Delivered") {
		t.Errorf("terminal fragment missing the delivered confirmation; body = %q", body)
	}
}

func TestSendStatusHandlerMismatchedBookReturns404(t *testing.T) {
	db := newSendTestDB(t)
	ctx := context.Background()
	bookID := createSendTestBook(t, db)
	otherBookID := createSendTestBook(t, db) // a second book, distinct id

	sendID, err := db.EnqueueSend(ctx, bookID, "Piranesi", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir(), true)
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(otherBookID)+"/sends/"+itoa(sendID), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET poll for a send that belongs to a different book = %d, want 404", rec.Code)
	}
}

func TestSendHandlerNonHXRequestRedirects303(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := Routes(service.New(db), t.TempDir(), true)

	rec := postSendForm(handler, id, url.Values{"recipient": {"reader@kindle.com"}}, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST send (no HX-Request) status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/books/"+itoa(id) {
		t.Errorf("Location = %q, want /books/%d", loc, id)
	}

	send, err := db.LatestSendForBook(context.Background(), id)
	if err != nil || send == nil {
		t.Fatalf("a plain form POST must still queue the send: LatestSendForBook = %+v, %v", send, err)
	}
}

func TestSendControlWhenDisabled(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := Routes(service.New(db), t.TempDir(), false)

	getReq := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "Sending is not configured") {
		t.Errorf("detail page with sending disabled missing the explanation; body = %q", getRec.Body.String())
	}

	rec := postSendForm(handler, id, url.Values{"recipient": {"reader@kindle.com"}}, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST send with sending disabled = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sending is not configured") {
		t.Errorf("503 body missing the explanation; body = %q", rec.Body.String())
	}

	send, err := db.LatestSendForBook(context.Background(), id)
	if err != nil {
		t.Fatalf("LatestSendForBook: %v", err)
	}
	if send != nil {
		t.Errorf("POST with sending disabled queued a send: %+v, want none", send)
	}
}

// html/template auto-escapes by context, but FailureReason is exactly the
// kind of machine-supplied string (Resend's own error text) worth pinning
// directly: a malicious or malformed provider response must never reach
// the page as live markup.
func TestSendStatusHandlerEscapesFailureReason(t *testing.T) {
	db := newSendTestDB(t)
	ctx := context.Background()
	id := createSendTestBook(t, db)

	sendID, err := db.EnqueueSend(ctx, id, "Piranesi", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	if _, err := db.ClaimNextSend(ctx, time.Now()); err != nil {
		t.Fatalf("ClaimNextSend: %v", err)
	}
	if err := db.MarkSendFailed(ctx, sendID, "<script>alert(1)</script>", time.Now()); err != nil {
		t.Fatalf("MarkSendFailed: %v", err)
	}

	handler := Routes(service.New(db), t.TempDir(), true)
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"/sends/"+itoa(sendID), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("failure reason rendered unescaped; body = %q", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("failure reason not HTML-escaped as expected; body = %q", body)
	}
}

func TestSendHandlerInvalidAddressRendersFieldError(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := Routes(service.New(db), t.TempDir(), true)

	rec := postSendForm(handler, id, url.Values{"recipient": {"not-an-address"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST send with an invalid address status = %d, want 200 (a field error, not a 500)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "send__error") {
		t.Errorf("invalid-address response missing a field error; body = %q", rec.Body.String())
	}

	send, err := db.LatestSendForBook(context.Background(), id)
	if err != nil {
		t.Fatalf("LatestSendForBook: %v", err)
	}
	if send != nil {
		t.Errorf("an invalid address queued a send: %+v, want none", send)
	}
}

func TestSendHandlerUnknownBookReturns404(t *testing.T) {
	db := newSendTestDB(t)
	handler := Routes(service.New(db), t.TempDir(), true)

	rec := postSendForm(handler, 99999, url.Values{"recipient": {"reader@kindle.com"}}, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST send for an unknown book = %d, want 404", rec.Code)
	}
}

// postSendFormFrom is postSendForm with an explicit Sec-Fetch-Site, the
// header a browser stamps on every request it issues — "same-origin" for
// the app's own form, "cross-site" for a page on someone else's domain.
func postSendFormFrom(handler http.Handler, id int64, form url.Values, fetchSite string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/books/"+itoa(id)+"/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", fetchSite)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestSendHandlerRejectsCrossSitePost(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := Routes(service.New(db), t.TempDir(), true)

	// The shape of an auto-submitting form on an attacker's page: a
	// form-encoded POST needs no preflight, and the library has no login
	// to stop it, so the send would go to an address of their choosing.
	rec := postSendFormFrom(handler, id, url.Values{"new_address": {"attacker@evil.example"}}, "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST send = %d, want 403", rec.Code)
	}
	send, err := db.LatestSendForBook(context.Background(), id)
	if err != nil {
		t.Fatalf("LatestSendForBook: %v", err)
	}
	if send != nil {
		t.Errorf("a cross-site POST queued a send: %+v, want none", send)
	}
}

func TestSendHandlerAllowsSameOriginAndMetadataLessPosts(t *testing.T) {
	// "none" is a direct navigation, and an absent header is a client
	// that sends no fetch metadata at all — neither is the cross-origin
	// page the guard exists for, so both must still work.
	for _, fetchSite := range []string{"same-origin", "none", ""} {
		db := newSendTestDB(t)
		id := createSendTestBook(t, db)
		handler := Routes(service.New(db), t.TempDir(), true)

		rec := postSendFormFrom(handler, id, url.Values{"recipient": {"reader@kindle.com"}}, fetchSite)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("Sec-Fetch-Site %q: POST send = %d, want 303", fetchSite, rec.Code)
		}
		send, err := db.LatestSendForBook(context.Background(), id)
		if err != nil {
			t.Fatalf("LatestSendForBook: %v", err)
		}
		if send == nil {
			t.Errorf("Sec-Fetch-Site %q: queued nothing, want a send", fetchSite)
		}
	}
}

func TestSendHandlerInvalidAddressKeepsPreviousSendAndTypedValues(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := Routes(service.New(db), t.TempDir(), true)
	ctx := context.Background()

	// A send that has already finished, so the control is showing a
	// result the rejected address must not retract.
	sendID, err := db.EnqueueSend(ctx, id, "Piranesi", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	if _, err := db.ClaimNextSend(ctx, time.Now()); err != nil {
		t.Fatalf("ClaimNextSend: %v", err)
	}
	if err := db.MarkSendDelivered(ctx, sendID, "msg-1", time.Now()); err != nil {
		t.Fatalf("MarkSendDelivered: %v", err)
	}

	rec := postSendForm(handler, id, url.Values{
		"new_address": {"not-an-address"},
		"new_label":   {"Spare Kindle"},
	}, true)
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("POST send with an invalid address = %d, want 200; body = %s", rec.Code, body)
	}
	if !strings.Contains(body, "send__error") {
		t.Errorf("response missing the field error; body = %q", body)
	}
	if !strings.Contains(body, "Delivered") {
		t.Errorf("a rejected address retracted the delivered result; body = %q", body)
	}
	if !strings.Contains(body, `value="not-an-address"`) || !strings.Contains(body, `value="Spare Kindle"`) {
		t.Errorf("the typed address and label were not carried back; body = %q", body)
	}
}
