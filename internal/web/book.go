package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"library/internal/service"
)

// bookLocationView is one of a book's file locations, shaped for the
// template.
type bookLocationView struct {
	Path    string
	Missing bool
}

// bookDetailPage is the data book.html renders against. Title doubles as
// the browser tab title (via document-head) and the on-page heading — the
// book's own title, unlike libraryPage's constant "Library". Count is the
// masthead's library-wide total, same contract as libraryPage.Count.
type bookDetailPage struct {
	Title             string
	Count             int
	ID                int64
	CoverURL          string
	Format            string
	FileSizeHuman     string
	FileSizeBytesText string
	Locations         []bookLocationView
	AuthorLine        string
	Publisher         string
	PublishedDate     string
	Language          string
	ISBN              string
	Description       string
	AddedAt           string
}

// bookDetailHandler serves GET /books/{id}. A non-numeric id and an
// unknown id both 404 identically (http.NotFound) — deliberately
// indistinguishable, since neither is a client error worth its own page.
func bookDetailHandler(svc *service.Service) http.HandlerFunc {
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

		locations := make([]bookLocationView, len(detail.Locations))
		for i, l := range detail.Locations {
			locations[i] = bookLocationView{Path: l.Path, Missing: l.Missing}
		}

		page := bookDetailPage{
			Title:             detail.Title,
			Count:             count,
			ID:                detail.ID,
			CoverURL:          coverURL(detail.CoverPath),
			Format:            detail.Format,
			FileSizeHuman:     humanSize(detail.FileSize),
			FileSizeBytesText: fmt.Sprintf("%d bytes", detail.FileSize),
			Locations:         locations,
			AuthorLine:        fullAuthorLine(detail.Authors),
			Publisher:         detail.Publisher,
			PublishedDate:     detail.PublishedDate,
			Language:          detail.Language,
			ISBN:              detail.ISBN,
			Description:       detail.Description,
			AddedAt:           detail.AddedAt.Format("2006-01-02"),
		}

		if err := render(w, "book.html", page); err != nil {
			slog.Error("render template failed", "template", "book.html", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
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
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
