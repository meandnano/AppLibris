package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"library/internal/service"
)

// sendHandler serves POST /books/{id}/send: form-encoded recipient (an
// address chosen from the picker) or new_address plus new_label (the
// inline "add address" disclosure). It answers an htmx request with the
// send-control fragment and everyone else with a 303 back to the book page
// — the same progressive-enhancement split libraryHandler uses for search,
// and for the same reason: a plain form POST with JS disabled must still
// work, landing back on a page whose initial render (via LatestSend)
// resumes the job it just queued.
//
// When sending is unconfigured, this still 503s with the disabled
// fragment rather than 404ing: a stale open tab gets an explanation
// instead of a dead link, and cmd/server has already logged why at
// startup.
func sendHandler(svc *service.Service, sendEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		if !sendEnabled {
			page := bookDetailPage{ID: id, SendEnabled: false}
			applySendState(&page, nil)
			w.WriteHeader(http.StatusServiceUnavailable)
			if err := render(w, "send-control", page); err != nil {
				slog.Error("render template failed", "template", "send-control", "error", err)
			}
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		address := r.FormValue("recipient")
		label := ""
		if newAddress := strings.TrimSpace(r.FormValue("new_address")); newAddress != "" {
			address = newAddress
			label = r.FormValue("new_label")
		}

		state, err := svc.QueueSend(r.Context(), id, address, label)
		if err != nil && !errors.Is(err, service.ErrInvalidAddress) {
			slog.Error("queue send failed", "id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err == nil && state == nil {
			http.NotFound(w, r)
			return
		}

		fragment := r.Header.Get("HX-Request") != "" && r.Header.Get("HX-History-Restore-Request") == ""
		if !fragment {
			http.Redirect(w, r, "/books/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}

		page := bookDetailPage{ID: id, SendEnabled: true}
		if errors.Is(err, service.ErrInvalidAddress) {
			page.SendError = "That doesn't look like an email address."
			applySendState(&page, nil)
		} else {
			applySendState(&page, state)
		}
		recipients, rErr := svc.Recipients(r.Context())
		if rErr != nil {
			slog.Error("list recipients failed", "error", rErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		page.Recipients = recipients

		if err := render(w, "send-control", page); err != nil {
			slog.Error("render template failed", "template", "send-control", "error", err)
		}
	}
}

// sendStatusHandler serves GET /books/{id}/sends/{sendID}, the send
// control's poll target. Scoped under the book id so a mismatched pairing
// 404s rather than rendering one book's send status under another's page.
// Answers with the same send-control fragment sendHandler does, so the
// fragment's own hx-get re-arms identically regardless of which route
// produced it, and stops by construction once the send reaches a terminal
// state — the status box that carries hx-get/hx-trigger="load" is only
// ever rendered for a non-terminal send, so a delivered or failed fragment
// has nothing left to re-trigger it (the form's own hx-post survives every
// state, so "Send again"/"Retry" stays htmx-enhanced; that's an ordinary
// user action, not automatic polling).
func sendStatusHandler(svc *service.Service, sendEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		sendID, err := strconv.ParseInt(r.PathValue("sendID"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		state, err := svc.SendState(r.Context(), sendID)
		if err != nil {
			slog.Error("get send state failed", "send_id", sendID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if state == nil || state.BookID != id {
			http.NotFound(w, r)
			return
		}

		page := bookDetailPage{ID: id, SendEnabled: sendEnabled}
		applySendState(&page, state)
		if sendEnabled {
			recipients, err := svc.Recipients(r.Context())
			if err != nil {
				slog.Error("list recipients failed", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			page.Recipients = recipients
		}

		if err := render(w, "send-control", page); err != nil {
			slog.Error("render template failed", "template", "send-control", "error", err)
		}
	}
}
