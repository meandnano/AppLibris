package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"library/internal/service"
	"library/internal/storage"
)

// enrichRoutes builds the mux with enrichment on and sending off — the
// enrichment control is what these tests read, and leaving the send
// control disabled keeps its markup out of the fragments they assert on.
func enrichRoutes(db *storage.DB) http.Handler {
	return Routes(service.New(db), "", false, true)
}

func postEnrich(handler http.Handler, id int64, hx bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/books/"+itoa(id)+"/enrich", nil)
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestEnrichHandlerEnqueuesAndReturnsPendingFragment(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)

	rec := postEnrich(handler, id, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST enrich status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-get="/books/`+itoa(id)+`/enrichment/`) {
		t.Errorf("pending fragment missing its poll hx-get; body = %q", body)
	}
	if !strings.Contains(body, "Fetching metadata") {
		t.Errorf("pending fragment missing its label; body = %q", body)
	}

	job, err := db.LatestEnrichmentForBook(context.Background(), id)
	if err != nil || job == nil {
		t.Fatalf("LatestEnrichmentForBook: %+v, %v", job, err)
	}
	if job.Status != storage.EnrichmentQueued {
		t.Errorf("queued job status = %q, want queued", job.Status)
	}
}

// The whole polling story rests on this: a terminal fragment carries no
// trigger at all, so htmx stops asking by construction rather than because
// something counted the attempts.
func TestEnrichTerminalFragmentCarriesNoPollTrigger(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)
	ctx := context.Background()

	if _, err := db.EnqueueEnrichment(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimNextEnrichment(ctx, time.Now())
	if err != nil || job == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", job, err)
	}
	if err := db.MarkEnrichmentDone(ctx, job.ID, []storage.MetadataField{storage.FieldPublisher}, time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := getEnrichmentStatus(handler, id, job.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET enrichment status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "hx-trigger") {
		t.Errorf("terminal fragment carries an hx-trigger, so polling would never stop; body = %q", body)
	}
	if !strings.Contains(body, "hx-post") {
		t.Errorf("terminal fragment lost the form's hx-post, so Fetch again would fall back to a navigation; body = %q", body)
	}
}

func getEnrichmentStatus(handler http.Handler, bookID, jobID int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(bookID)+"/enrichment/"+itoa(jobID), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// The result names what moved, so nobody has to hunt the page for it.
func TestEnrichResultNamesTheFieldsWritten(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)
	ctx := context.Background()

	job := claimedJob(t, db, id)
	if err := db.MarkEnrichmentDone(ctx, job.ID, []storage.MetadataField{storage.FieldPublisher, storage.FieldDescription}, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := getEnrichmentStatus(handler, id, job.ID).Body.String()
	if !strings.Contains(body, "Added publisher, description") {
		t.Errorf("result line does not name the fields written; body = %q", body)
	}
	if !strings.Contains(body, "enrich__status--ok") {
		t.Errorf("a job that wrote fields is not rendered as a success; body = %q", body)
	}
}

// "Nothing to add" is the ordinary outcome for a book whose embedded
// metadata is already complete. Rendering it as a failure would train
// people to distrust a feature working exactly as intended.
func TestEnrichNothingToAddRendersAsSuccess(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)
	ctx := context.Background()

	job := claimedJob(t, db, id)
	if err := db.MarkEnrichmentDone(ctx, job.ID, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := getEnrichmentStatus(handler, id, job.ID).Body.String()
	if !strings.Contains(body, "Nothing to add") {
		t.Errorf("body = %q, want the nothing-to-add line", body)
	}
	if !strings.Contains(body, "enrich__status--ok") {
		t.Errorf("nothing-to-add is not rendered as a success; body = %q", body)
	}
	if strings.Contains(body, "enrich__status--err") || strings.Contains(body, "Failed") {
		t.Errorf("nothing-to-add is rendered as a failure; body = %q", body)
	}
}

func TestEnrichFailedJobRendersItsReason(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)
	ctx := context.Background()

	job := claimedJob(t, db, id)
	if err := db.MarkEnrichmentFailed(ctx, job.ID, "the book is no longer in the library", time.Now()); err != nil {
		t.Fatal(err)
	}

	body := getEnrichmentStatus(handler, id, job.ID).Body.String()
	if !strings.Contains(body, "the book is no longer in the library") {
		t.Errorf("failed fragment missing its reason; body = %q", body)
	}
	if !strings.Contains(body, "enrich__status--err") {
		t.Errorf("failed fragment not rendered as a failure; body = %q", body)
	}
	if strings.Contains(body, "hx-trigger") {
		t.Errorf("failed fragment still polls; body = %q", body)
	}
}

// Scoped under the book id so a mismatched pairing 404s rather than
// rendering one book's job state under another's page.
func TestEnrichmentStatusRejectsAJobFromAnotherBook(t *testing.T) {
	db := newSendTestDB(t)
	owner := createSendTestBook(t, db)
	other := createSendTestBook(t, db)
	handler := enrichRoutes(db)

	job := claimedJob(t, db, owner)

	if rec := getEnrichmentStatus(handler, other, job.ID); rec.Code != http.StatusNotFound {
		t.Errorf("GET another book's job = %d, want 404", rec.Code)
	}
	if rec := getEnrichmentStatus(handler, owner, job.ID); rec.Code != http.StatusOK {
		t.Errorf("GET the owning book's job = %d, want 200", rec.Code)
	}
}

func TestEnrichmentStatusUnknownJob404s(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)

	if rec := getEnrichmentStatus(handler, id, 9999); rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown job = %d, want 404", rec.Code)
	}
}

// With JavaScript off the same form is an ordinary navigation, landing on
// a page whose initial render picks the queued job up.
func TestEnrichHandlerNonHTMXRedirectsToTheBook(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)

	rec := postEnrich(handler, id, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("non-htmx POST enrich = %d, want 303", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "/books/"+itoa(id); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	job, err := db.LatestEnrichmentForBook(context.Background(), id)
	if err != nil || job == nil {
		t.Fatalf("the job must still be queued on the no-JS path: %+v, %v", job, err)
	}
}

func TestEnrichHandlerUnknownBook404s(t *testing.T) {
	db := newSendTestDB(t)
	handler := enrichRoutes(db)

	if rec := postEnrich(handler, 4242, true); rec.Code != http.StatusNotFound {
		t.Errorf("POST enrich for an unknown book = %d, want 404", rec.Code)
	}
}

// No provider configured means the control cannot do what it offers, so it
// 503s with the disabled fragment rather than 404ing — a stale open tab
// gets an explanation, the same treatment the send control gets when
// Resend is unconfigured.
func TestEnrichHandlerDisabledServesTheDisabledFragment(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := Routes(service.New(db), "", false, false)

	rec := postEnrich(handler, id, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST enrich with no provider = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("disabled fragment missing its explanation; body = %q", rec.Body.String())
	}
	job, err := db.LatestEnrichmentForBook(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Errorf("a job was queued with no provider to run it: %+v", job)
	}
}

func TestEnrichHandlerRejectsCrossSitePost(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)

	req := httptest.NewRequest(http.MethodPost, "/books/"+itoa(id)+"/enrich", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST enrich = %d, want 403", rec.Code)
	}
	job, err := db.LatestEnrichmentForBook(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Errorf("a cross-site POST queued a job: %+v", job)
	}
}

// The detail page's initial render picks up whatever job the book already
// has, so a page opened mid-run resumes polling instead of showing a bare
// button.
func TestBookPageResumesAPendingEnrichment(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)

	job := claimedJob(t, db, id)

	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET book = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/enrichment/"+itoa(job.ID)) {
		t.Errorf("book page does not resume polling the running job %d", job.ID)
	}
}

func claimedJob(t *testing.T, db *storage.DB, bookID int64) *storage.EnrichmentJob {
	t.Helper()
	ctx := context.Background()
	if _, err := db.EnqueueEnrichment(ctx, bookID, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimNextEnrichment(ctx, time.Now())
	if err != nil || job == nil {
		t.Fatalf("ClaimNextEnrichment: %+v, %v", job, err)
	}
	if job.BookID != bookID {
		t.Fatalf("claimed a job for book %d, want %d", job.BookID, bookID)
	}
	return job
}

func TestEnrichmentResultLine(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   string
	}{
		{"nothing", nil, "Nothing to add"},
		{"empty slice", []string{}, "Nothing to add"},
		{"one", []string{"publisher"}, "Added publisher"},
		{"several", []string{"publisher", "language", "description"}, "Added publisher, language, description"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := enrichmentResultLine(c.fields); got != c.want {
				t.Errorf("enrichmentResultLine(%v) = %q, want %q", c.fields, got, c.want)
			}
		})
	}
}

// Decision 1: the marker is a caveat, not a label. A provider's name is
// the only provenance worth showing — "embedded" is the default and
// therefore not information, and "manual" tells the person who typed it
// something they already know.
func TestProviderSourceNote(t *testing.T) {
	cases := []struct{ source, want string }{
		{"openlibrary", "via openlibrary"},
		{"googlebooks", "via googlebooks"},
		{"embedded", ""},
		{"manual", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := providerSourceNote(c.source); got != c.want {
			t.Errorf("providerSourceNote(%q) = %q, want %q", c.source, got, c.want)
		}
	}
}

// The same rule through the real page, since the marker has to survive
// makeFieldViews and the template to be worth anything.
func TestBookPageMarksOnlyProviderSourcedFields(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)
	ctx := context.Background()

	// publisher comes from a provider; the title was embedded by the
	// scanner at creation, and language is edited by hand below.
	if _, _, err := db.ApplyEnrichedFields(ctx, id, map[storage.MetadataField]string{
		storage.FieldPublisher: "Bloomsbury",
	}, map[storage.MetadataField]string{
		storage.FieldPublisher: "openlibrary",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateBookField(ctx, id, storage.FieldLanguage, "en", time.Now()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()

	// Counted rather than dumped: this is a whole page, and the useful
	// signal is how many markers it carries, not its 8KB of markup.
	if got := strings.Count(body, "editable__source"); got != 1 {
		t.Errorf("source markers on the page = %d, want exactly 1 (publisher's)", got)
	}
	if !strings.Contains(body, "via openlibrary") {
		t.Error("provider-sourced publisher is not marked")
	}
	for _, unwanted := range []string{"via manual", "via embedded"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("a non-provider source was marked: %q", unwanted)
		}
	}
}

// Saving a field sets its source to manual, so the marker must be gone
// from the fragment that comes back. This works only because the POST
// handler reloads the book instead of echoing the submitted value — which
// is exactly the kind of thing a later "optimisation" removes, hence the
// test.
func TestEditingAProviderSourcedFieldClearsItsMarker(t *testing.T) {
	db := newSendTestDB(t)
	id := createSendTestBook(t, db)
	handler := enrichRoutes(db)
	ctx := context.Background()

	if _, _, err := db.ApplyEnrichedFields(ctx, id, map[storage.MetadataField]string{
		storage.FieldPublisher: "Bloomsbury",
	}, map[storage.MetadataField]string{
		storage.FieldPublisher: "openlibrary",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The read fragment still carries it before the edit.
	beforeReq := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"/metadata/publisher", nil)
	beforeReq.Header.Set("HX-Request", "true")
	before := httptest.NewRecorder()
	handler.ServeHTTP(before, beforeReq)
	if !strings.Contains(before.Body.String(), "via openlibrary") {
		t.Fatalf("publisher fragment is not marked before the edit; body = %q", before.Body.String())
	}

	after := postField(handler, id, "publisher", url.Values{"value": {"Gollancz"}}, htmx)
	if after.Code != http.StatusOK {
		t.Fatalf("POST publisher = %d; body = %s", after.Code, after.Body.String())
	}
	body := after.Body.String()
	if strings.Contains(body, "editable__source") {
		t.Errorf("the marker survived a manual edit; body = %q", body)
	}
	if !strings.Contains(body, "Gollancz") {
		t.Errorf("fragment does not show the saved value; body = %q", body)
	}
}
