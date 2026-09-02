// Package web is the HTTP transport for the browser UI: thin handlers that
// parse a request, call into internal/service, and render — no business
// logic lives here, per DESIGN.md's "Layering for a future API."
package web

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"library/internal/service"
)

// Routes builds the web UI's HTTP handler. coversDir is the directory the
// scanner writes stored cover thumbnails into; it is served read-only under
// /covers/ so the library grid can reference a cover by URL rather than by
// the absolute on-disk path storage keeps.
//
// This mux owns 404s for everything under /: cmd/server mounts it behind its
// own outer catch-all so /healthz can live alongside the UI, but every
// pattern below matches an exact path or a specific prefix, so a request
// that matches none of them falls through to ServeMux's own 404 rather than
// being narrowed on the outer mount.
func Routes(svc *service.Service, coversDir string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", libraryHandler(svc))
	mux.Handle("GET /static/", staticHandler())
	mux.Handle("GET /covers/", coversHandler(coversDir))
	return mux
}

// bookCard is one library-grid entry shaped for the template: a
// service.BookSummary with its author list flattened to a single line and
// its stored cover path turned into a URL. The template stays logic-free;
// the shaping happens here.
type bookCard struct {
	ID         int64
	Title      string
	AuthorLine string
	Format     string
	CoverURL   string
}

// libraryPage is the data library.html (and its book-grid fragment) render
// against. Count feeds the masthead, which only ever appears on a full
// page — never the book-grid fragment a live search swaps in — so it's
// the library's total size on a full-page render (never the
// search-filtered count, which lives separately in Books/search__count,
// so the masthead reads the same whether this search was typed live or
// landed on by a shared/bookmarked ?q= link) but is left as the cheaper
// filtered count on a fragment render, where nothing reads it. Query and
// Searching distinguish "browsing the whole library" from "typed
// something, however it resolved" — a blank Books with Searching true is
// the no-results state; a blank Books with Searching false is the
// empty-library state, and the two need different treatment.
type libraryPage struct {
	Title     string
	Count     int
	Books     []bookCard
	Query     string
	Searching bool
}

// libraryHandler serves both the full library page and, for an htmx
// request, just its book-grid fragment — the same resource, differing
// only in how much of the page comes back. GET /?q=... narrows the grid to
// a search; a blank or missing q is the unfiltered list. Vary: HX-Request
// matters because the two responses at this one URL have different
// bodies: without it a shared cache or the browser's back-forward cache
// could serve a bare fragment where a full page was expected, or the
// reverse.
func libraryHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request")

		query := r.URL.Query().Get("q")
		searching := strings.TrimSpace(query) != ""
		fragment := r.Header.Get("HX-Request") != ""

		// SearchBooks already treats a blank query as ListBooks, so this
		// covers both cases without repeating that check here.
		books, err := svc.SearchBooks(r.Context(), query)
		if err != nil {
			slog.Error("list books failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// The masthead count Count feeds is only ever rendered on a full
		// page — never on the book-grid fragment a live search swaps in —
		// so a fragment response has no use for the library's total and
		// isn't worth an extra query for it on every keystroke. len(books)
		// is already the total whenever not searching.
		total := len(books)
		if searching && !fragment {
			total, err = svc.CountBooks(r.Context())
			if err != nil {
				slog.Error("count books failed", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		cards := make([]bookCard, len(books))
		for i, b := range books {
			cards[i] = bookCard{
				ID:         b.ID,
				Title:      b.Title,
				AuthorLine: authorLine(b.Authors),
				Format:     b.Format,
				CoverURL:   coverURL(b.CoverPath),
			}
		}

		page := libraryPage{Title: "Library", Count: total, Books: cards, Query: query, Searching: searching}

		templateName := "library.html"
		if fragment {
			templateName = "book-grid"
		}
		if err := render(w, templateName, page); err != nil {
			slog.Error("render template failed", "template", templateName, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

// authorLine renders a book's authors as the single line a grid card has room
// for. Three or more names would wrap past the card, so they collapse.
func authorLine(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " & " + names[1]
	default:
		return names[0] + " and " + strconv.Itoa(len(names)-1) + " others"
	}
}

// coverURL maps a stored cover's filesystem path to its /covers/ URL. Covers
// are named by content hash in one flat directory, so the base name is the
// whole URL.
func coverURL(coverPath string) string {
	if coverPath == "" {
		return ""
	}
	return "/covers/" + filepath.Base(coverPath)
}
