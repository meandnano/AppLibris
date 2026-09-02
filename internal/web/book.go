package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"library/internal/service"
)

// bookDetailPage is the data book.html renders against. Title doubles as
// the browser tab title (via document-head) and the on-page heading — the
// book's own title, unlike libraryPage's constant "Library". Count and
// CountText are the masthead's library-wide total, same contract as
// libraryPage's — the shared site-header partial renders CountText and
// pluralizes on Count, so both have to be here or the masthead breaks.
// Locations is service.FileLocation as it comes: the template reads Path
// and Missing under those names, so a per-page copy of the same two fields
// would be a rename of nothing. FileSizeHuman is empty when the book has
// no location to take a size from — the template renders an em dash there
// rather than "0 B", which is a size, and a wrong one.
//
// ID is the book id the send form posts to — removed as unread after #29,
// it comes back here with that caller. The Send* fields are also this
// struct's data for the "send-control" fragment the send POST and status
// poll routes render standalone (most other fields left zero, same reuse
// #28's book-grid fragment already makes of libraryPage). Send itself is
// service.SendState as it comes — nil when the book has never been sent,
// the idle state before any address is picked — with SendPending,
// SendButtonLabel, SendButtonPrimary, SendAt and SendPollURL as its
// derived presentation fields, computed once by applySendState so the
// template branches on state without formatting a timestamp or composing a
// label itself, the same discipline searchSummary applies to the results
// line.
type bookDetailPage struct {
	Title             string
	Count             int
	CountText         string
	CoverURL          string
	Format            string
	FileSizeHuman     string
	FileSizeBytesText string
	Locations         []service.FileLocation
	AuthorLine        string
	Publisher         string
	PublishedDate     string
	Language          string
	ISBN              string
	Description       string
	AddedAt           string

	ID                int64
	SendEnabled       bool
	Recipients        []service.RecipientOption
	Send              *service.SendState
	SendPending       bool
	SendButtonLabel   string
	SendButtonPrimary bool
	SendAt            string
	SendPollURL       string
	SendError         string
}

// bookDetailHandler serves GET /books/{id}. A non-numeric id and an
// unknown id both 404 identically (http.NotFound) — deliberately
// indistinguishable, since neither is a client error worth its own page.
func bookDetailHandler(svc *service.Service, sendEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		detail, err := svc.GetBook(r.Context(), id)
		if err != nil {
			slog.Error("get book failed", "id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if detail == nil {
			http.NotFound(w, r)
			return
		}

		count, err := svc.CountBooks(r.Context())
		if err != nil {
			slog.Error("count books failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		var sizeHuman, sizeBytes string
		if detail.HasFileSize {
			sizeHuman = humanSize(detail.FileSize)
			sizeBytes = fmt.Sprintf("%d bytes", detail.FileSize)
		}

		page := bookDetailPage{
			Title:             detail.Title,
			Count:             count,
			CountText:         formatCount(count),
			CoverURL:          coverURL(detail.CoverPath),
			Format:            detail.Format,
			FileSizeHuman:     sizeHuman,
			FileSizeBytesText: sizeBytes,
			Locations:         detail.Locations,
			AuthorLine:        fullAuthorLine(detail.Authors),
			Publisher:         detail.Publisher,
			PublishedDate:     detail.PublishedDate,
			Language:          detail.Language,
			ISBN:              detail.ISBN,
			Description:       detail.Description,
			AddedAt:           detail.AddedAt.Format("2006-01-02"),
			ID:                detail.ID,
		}
		if err := populateSendControl(r.Context(), svc, sendEnabled, &page); err != nil {
			slog.Error("load send state failed", "id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if err := render(w, "book.html", page); err != nil {
			slog.Error("render template failed", "template", "book.html", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

// populateSendControl fills page's send-control fields: the saved
// recipients (skipped when sending is unconfigured — there is nothing to
// pick from) and this book's most recent send, if any. Shared by the full
// detail-page render and the standalone send-control fragment handlers in
// send.go, so the three routes derive the picker and status box the same
// way.
func populateSendControl(ctx context.Context, svc *service.Service, sendEnabled bool, page *bookDetailPage) error {
	page.SendEnabled = sendEnabled
	if !sendEnabled {
		return nil
	}

	recipients, err := svc.Recipients(ctx)
	if err != nil {
		return err
	}
	page.Recipients = recipients

	send, err := svc.LatestSend(ctx, page.ID)
	if err != nil {
		return err
	}
	applySendState(page, send)
	return nil
}

// applySendState fills page's derived send-status fields from send (nil
// for a book that has never been sent). Computed once here — the button
// label, whether the control is mid-send, and the formatted timestamp —
// rather than in the template, the same discipline searchSummary applies
// to the results line.
func applySendState(page *bookDetailPage, send *service.SendState) {
	page.Send = send
	if send == nil {
		page.SendButtonLabel = "Send"
		page.SendButtonPrimary = true
		return
	}

	page.SendAt = send.At.Format("2006-01-02 15:04")
	switch send.Status {
	case "queued", "sending":
		page.SendPending = true
		page.SendButtonLabel = "Sending"
		page.SendPollURL = fmt.Sprintf("/books/%d/sends/%d", page.ID, send.ID)
	case "delivered":
		page.SendButtonLabel = "Send again"
	case "failed":
		page.SendButtonLabel = "Retry"
		page.SendButtonPrimary = true
	default:
		page.SendButtonLabel = "Send"
		page.SendButtonPrimary = true
	}
}

// fullAuthorLine joins every author name for display — unlike authorLine's
// grid-card "and N others" collapse, the detail page has room to name
// everyone.
func fullAuthorLine(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " & " + names[len(names)-1]
	}
}

// humanSize formats n bytes as e.g. "12.3 MB", binary (1024-based) units.
// Picking the unit off the truncated ratio isn't quite enough on its own:
// a value like 1023.97 is < 1024 but rounds to "1024.0" at one decimal
// place, so a size a handful of bytes under 1 GiB would otherwise print as
// "1024.0 MB". Bumping the unit once more when the rounded value would
// reach the next tier's threshold keeps the displayed number and its unit
// in agreement.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := "KMGTPE"
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	if value >= unit-0.05 && exp < len(units)-1 {
		div *= unit
		exp++
		value = float64(n) / float64(div)
	}
	return fmt.Sprintf("%.1f %cB", value, units[exp])
}
