package web

import (
	"log/slog"
	"net/http"
	"strconv"

	"library/internal/service"
)

// enrichHandler serves POST /books/{id}/enrich: putting one book on the
// metadata-enrichment queue. Same shape as sendHandler, because it is the
// same underlying thing — a queued background job against one book — and
// the resemblance is meant to be visible rather than tidied away: a button
// that posts, a pending state that polls, a terminal state that says what
// happened.
//
// It answers an htmx request with the enrich-control fragment and everyone
// else with a 303 back to the book page, whose initial render picks the job
// up through LatestEnrichment. So the no-JavaScript path works without a
// second code path to drift.
//
// With no provider configured this 503s with the disabled fragment rather
// than 404ing, sendHandler's rule: a stale open tab gets an explanation,
// and cmd/server has already logged why at startup.
func enrichHandler(svc *service.Service, enrichEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		if !enrichEnabled {
			page := bookDetailPage{ID: id, EnrichEnabled: false}
			w.WriteHeader(http.StatusServiceUnavailable)
			if err := render(w, "enrich-control", page); err != nil {
				slog.Error("render template failed", "template", "enrich-control", "error", err)
			}
			return
		}

		state, err := svc.EnrichBook(r.Context(), id)
		if err != nil {
			slog.Error("enqueue enrichment failed", "id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if state == nil {
			http.NotFound(w, r)
			return
		}

		if !isHTMXFragment(r) {
			http.Redirect(w, r, "/books/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}

		page := bookDetailPage{ID: id, EnrichEnabled: true}
		applyEnrichmentState(&page, state)
		if err := render(w, "enrich-control", page); err != nil {
			slog.Error("render template failed", "template", "enrich-control", "error", err)
		}
	}
}

// enrichStatusHandler serves GET /books/{id}/enrichment/{jobID}, the
// control's poll target. Scoped under the book id so a mismatched pairing
// 404s rather than rendering one book's job state under another's page,
// exactly as GET /books/{id}/sends/{sendID} is.
//
// It answers with the same enrich-control fragment enrichHandler does, so
// the fragment's own hx-get re-arms identically regardless of which route
// produced it — and stops by construction once the job is terminal, since
// the status box carrying hx-get/hx-trigger="load" is only ever rendered
// for a pending job. The form's own hx-post survives every state, so
// "Fetch again"/"Retry" stays htmx-enhanced; that is a user action, not
// automatic polling.
//
// No Vary header: unlike the POST above, this serves one body to every
// caller.
func enrichStatusHandler(svc *service.Service, enrichEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		jobID, err := strconv.ParseInt(r.PathValue("jobID"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		state, err := svc.EnrichmentState(r.Context(), jobID)
		if err != nil {
			slog.Error("get enrichment state failed", "job_id", jobID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if state == nil || state.BookID != id {
			http.NotFound(w, r)
			return
		}

		page := bookDetailPage{ID: id, EnrichEnabled: enrichEnabled}
		applyEnrichmentState(&page, state)
		if err := render(w, "enrich-control", page); err != nil {
			slog.Error("render template failed", "template", "enrich-control", "error", err)
		}
	}
}
