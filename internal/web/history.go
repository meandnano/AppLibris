package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"library/internal/service"
)

// historyRow is one row of the history table, shaped for the template —
// every string composed here, per the searchSummary convention, so the
// template does no formatting of its own.
type historyRow struct {
	Title      string
	BookURL    string // "" when the book has been deleted — renders unlinked
	Recipient  string
	Status     string // "Delivered" / "Sending" / "Failed" — see historyStatus
	StatusKind string // "ok" / "pending" / "err" / "muted" — drives the CSS class
	Reason     string
	When       string
}

// historyPage is the data history.html renders against.
type historyPage struct {
	Title      string
	Nav        []navItem
	HeaderNote string
	Rows       []historyRow
	Empty      bool
}

// historyHandler serves GET /history — every send across the library over
// service.SendHistory's window, newest first. Built to answer one
// question: is this book already on the Kindle? Unlike the send control,
// this page renders even when sending is unconfigured: it is a log, not an
// action, and a library that used to send but no longer has a key set
// still has history worth reading.
func historyHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		records, truncated, err := svc.SendHistory(r.Context())
		if err != nil {
			slog.Error("send history failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		now := time.Now()
		rows := make([]historyRow, len(records))
		for i, rec := range records {
			label, kind := historyStatus(rec.Status)
			bookURL := ""
			if rec.BookID != 0 {
				bookURL = "/books/" + strconv.FormatInt(rec.BookID, 10)
			}
			rows[i] = historyRow{
				Title:      rec.BookTitle,
				BookURL:    bookURL,
				Recipient:  rec.Recipient,
				Status:     label,
				StatusKind: kind,
				Reason:     rec.FailureReason,
				When:       relativeTime(rec.At, now),
			}
		}

		page := historyPage{
			Title:      "History",
			Nav:        navFor("history"),
			HeaderNote: historyScopeLine(truncated),
			Rows:       rows,
			Empty:      len(rows) == 0,
		}
		if err := render(w, "history.html", page); err != nil {
			slog.Error("render template failed", "template", "history.html", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

// historyStatus maps a send's raw status to the row's label and the CSS
// kind that colors it. queued and sending both render "Sending" with the
// same "pending" kind — the same collapse the send control on the book
// detail page already makes, since the UI has no separate treatment for
// the gap between enqueue and claim. Plate 07's own mock draws queued as a
// distinct "Queued" in --fg-muted, but that is mockup data; the built
// control's rule wins here on purpose, since two screens naming the same
// state differently would be worse than either naming it alone. Recorded
// here because it is a deliberate departure from the plate, not an
// oversight.
func historyStatus(status string) (label, kind string) {
	switch status {
	case "delivered":
		return "Delivered", "ok"
	case "failed":
		return "Failed", "err"
	case "queued", "sending":
		return "Sending", "pending"
	default:
		return status, "muted"
	}
}

// historyScopeLine composes the masthead note for the history page: "last
// 30 days" ordinarily, or naming the cap once SendHistory reports that
// service.SendHistoryLimit actually cut rows out of the window. A fixed
// "last 30 days" over a silently truncated list would be a claim the page
// cannot support — this page exists to be believed.
func historyScopeLine(truncated bool) string {
	if !truncated {
		return "last 30 days"
	}
	return fmt.Sprintf("last 30 days · %d most recent", service.SendHistoryLimit)
}

// relativeTime renders t the way plate 07 does — "today, 14:02",
// "yesterday, 22:41", "28 Aug, 09:15" — with now passed in rather than
// read from the clock, so every case is a table test with no sleeping and
// no injected clock. Day boundaries are calendar days in the server's
// local zone, not 24-hour spans: a send at 23:50 is "yesterday" at 00:10,
// which is what a person means by it — comparing calendar dates (via
// AddDate, never a raw time.Sub) is what makes that correct across a DST
// transition, where a literal 24 hours can land on the wrong side of
// midnight. The zone is the server's local zone because that is the only
// zone the server knows — the browser's is not available to a
// server-rendered page without JavaScript.
func relativeTime(t, now time.Time) string {
	t = t.Local()
	now = now.Local()
	clock := t.Format("15:04")

	switch {
	case sameDate(t, now):
		return "today, " + clock
	case sameDate(t.AddDate(0, 0, 1), now):
		return "yesterday, " + clock
	default:
		return t.Format("2 Jan") + ", " + clock
	}
}

func sameDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
