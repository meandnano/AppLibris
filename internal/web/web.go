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
// the absolute on-disk path storage keeps. sendEnabled reflects whether
// cmd/server found RESEND_API_KEY and RESEND_FROM both set — send-to-Kindle
// routes still register either way, so a stale open tab gets a "not
// configured" explanation instead of a 404, but POST 503s until they are.
//
// This mux owns 404s for everything under /: cmd/server mounts it behind its
// own outer catch-all so /healthz can live alongside the UI, but every
// pattern below matches an exact path or a specific prefix, so a request
// that matches none of them falls through to ServeMux's own 404 rather than
// being narrowed on the outer mount.
func Routes(svc *service.Service, coversDir string, sendEnabled, enrichEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", libraryHandler(svc))
	mux.HandleFunc("GET /history", historyHandler(svc))
	mux.HandleFunc("GET /books/{id}", bookDetailHandler(svc, sendEnabled, enrichEnabled))
	mux.HandleFunc("GET /books/{id}/metadata/{field}", metadataHandler(svc, sendEnabled, enrichEnabled))
	mux.HandleFunc("POST /books/{id}/metadata/{field}", sameSiteOnly(metadataHandler(svc, sendEnabled, enrichEnabled)))
	mux.HandleFunc("POST /books/{id}/send", sameSiteOnly(sendHandler(svc, sendEnabled)))
	mux.HandleFunc("GET /books/{id}/sends/{sendID}", sendStatusHandler(svc, sendEnabled))
	mux.HandleFunc("POST /books/{id}/enrich", sameSiteOnly(enrichHandler(svc, enrichEnabled)))
	mux.HandleFunc("GET /books/{id}/enrichment/{jobID}", enrichStatusHandler(svc, enrichEnabled))
	mux.HandleFunc("POST /recipients/remove", sameSiteOnly(removeRecipientHandler(svc, sendEnabled)))
	mux.Handle("GET /static/", staticHandler())
	mux.Handle("GET /covers/", coversHandler(coversDir))
	return mux
}

// sameSiteOnly rejects a state-changing request the browser itself reports
// as cross-site. The library has no login, so a request's network position
// is the only thing between the collection and everyone else: a page on any
// origin can reach a LAN or localhost server its author cannot, and a
// form-encoded POST gets there with no CORS preflight to stop it. Sending a
// book is exactly the action worth stealing that way — the attachment goes
// to an address in the request body.
//
// Sec-Fetch-Site is what separates the two cases, and a page cannot forge
// it: the browser sets it. A request carrying no fetch metadata is allowed
// through, since a client that sends none (curl, a script, a browser older
// than the header) is not the ambient-authority vector this guards, and
// failing closed there would cost the UI for no security gain.
// isHTMXFragment reports whether r wants a fragment rather than a whole
// page. htmx sets HX-Request on every request it issues, including the one
// it makes restoring a history entry that has fallen out of its cache —
// but that response it swaps into the whole document body, so answering it
// with a fragment strips the page down to it. HX-History-Restore-Request
// is what separates the two, which is why both headers name themselves in
// every Vary set alongside this call.
func isHTMXFragment(r *http.Request) bool {
	return r.Header.Get("HX-Request") != "" && r.Header.Get("HX-History-Restore-Request") == ""
}

func sameSiteOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			next(w, r)
		default:
			http.Error(w, "cross-site request", http.StatusForbidden)
		}
	}
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
	// PathsLabel is the multi-location marker's text ("2 paths"), or "" for
	// the ordinary single-location book, which is what the template
	// branches on. Composed here rather than in the template, the same as
	// searchSummary: the template holds no formatting logic, and a count
	// that never reaches the marker cannot be rendered as "1 paths" by
	// accident.
	PathsLabel string
}

// navItem is one entry in the masthead's nav — Library and History today.
// Current is rendered as plain text rather than a link: there is nowhere
// more useful to send someone already on the page a link would point to.
type navItem struct {
	Label   string
	URL     string
	Current bool
}

// navFor builds the masthead's nav for the page named current ("library"
// or "history"), so every page composes it the same way and the two links
// can't drift out of sync with each other. The book detail page passes
// "library": it has no nav entry of its own, and highlighting Library
// there matches what the single hardcoded nav item did for every page
// before History existed.
func navFor(current string) []navItem {
	return []navItem{
		{Label: "Library", URL: "/", Current: current == "library"},
		{Label: "History", URL: "/history", Current: current == "history"},
	}
}

// headerBookCount composes the masthead's book-count note ("1,284 books"),
// shared by the library and book detail pages. Replaces the template's own
// pluralization branch — the last piece of formatting logic site-header
// held, and exactly what the searchSummary convention exists to move out
// of templates.
func headerBookCount(n int) string {
	noun := "books"
	if n == 1 {
		noun = "book"
	}
	return formatCount(n) + " " + noun
}

// libraryPage is the data library.html (and its book-grid fragment) render
// against.
//
// SearchSummary is the whole results line, composed in the handler so the
// template stays logic-free. Query and Searching distinguish "browsing the
// whole library" from "typed something, however it resolved" — a blank
// Books with Searching true is the no-results state; a blank Books with
// Searching false is the empty-library state, and the two need different
// treatment. LibraryEmpty drives plate 02e's dimmed, inert search control:
// with nothing indexed there is nothing to search, and an input that
// invites typing would be a lie.
type libraryPage struct {
	Title         string
	Nav           []navItem
	HeaderNote    string
	Books         []bookCard
	Query         string
	Searching     bool
	SearchSummary string
	LibraryEmpty  bool
}

// libraryHandler serves both the full library page and, for a live search
// request, just its book-grid fragment — the same resource, differing
// only in how much of the page comes back. GET /?q=... narrows the grid to
// a search; a blank or missing q is the unfiltered list.
//
// HX-Request alone does not mean "send the fragment". htmx sets it on a
// history-restore request too — the GET it issues when the user goes Back
// to a URL that has fallen out of its history cache (ten entries, and
// hx-push-url pushes one per keystroke, so this is ordinary Back-button
// use, not an edge case) — and there it swaps the response into the whole
// body. Answering that with the fragment would replace the masthead, the
// search bar and the scripts with a bare grid, leaving no way back but a
// manual reload. htmx marks that request HX-History-Restore-Request, so
// the fragment is for a request carrying HX-Request without it.
//
// Both headers are named in Vary because both change the body at this one
// URL: without it a shared cache or the browser's back-forward cache could
// serve a bare fragment where a full page was expected, or the reverse.
func libraryHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		query := r.URL.Query().Get("q")
		fragment := isHTMXFragment(r)

		// SearchBooks already treats a blank query as ListBooks, so this
		// covers both cases without repeating that check here, and it
		// reports whether what came back was a search — which is not the
		// same as the query looking non-blank, since input that sanitizes
		// to nothing (a lone control character, say) is no search either.
		result, err := svc.SearchBooks(r.Context(), query)
		if err != nil {
			slog.Error("list books failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		books := result.Books

		// The library total is needed on both kinds of render, not just
		// the full page: the masthead shows it, and so does the results
		// line the fragment carries ("4 of 1,284"). When nothing is
		// filtering it out, the books already in hand are that total.
		total := len(books)
		if result.Searched {
			total, err = svc.CountBooks(r.Context())
			if err != nil {
				slog.Error("count books failed", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		cards := make([]bookCard, len(books))
		for i, b := range books {
			card := bookCard{
				ID:         b.ID,
				Title:      b.Title,
				AuthorLine: authorLine(b.Authors),
				Format:     b.Format,
				CoverURL:   coverURL(b.CoverPath),
			}
			if b.Locations > 1 {
				card.PathsLabel = strconv.Itoa(b.Locations) + " paths"
			}
			cards[i] = card
		}

		page := libraryPage{
			Title:         "Library",
			Nav:           navFor("library"),
			HeaderNote:    headerBookCount(total),
			Books:         cards,
			Query:         query,
			Searching:     result.Searched,
			SearchSummary: searchSummary(len(cards), total, result.Fields),
			LibraryEmpty:  total == 0,
		}

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

// formatCount renders n with thousands grouped by commas ("1,284"), the
// form the mockups use everywhere a library count appears. Hand-rolled
// rather than pulled from golang.org/x/text: this is the only place the
// project formats a number, the counts are non-negative, and the locale is
// not a question a single-user self-hosted server needs to answer.
func formatCount(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// searchSummary composes the results line plate 02c specifies — "4 of
// 1,284 · matched title, author" — in the handler, so the template holds
// no formatting logic. fields are books_fts column names; "authors"
// becomes "author" because the line reads as prose, not as a schema.
// Returns "" when nothing matched, since the no-matches block says its own
// piece instead.
func searchSummary(matched, total int, fields []string) string {
	if matched == 0 {
		return ""
	}
	summary := formatCount(matched) + " of " + formatCount(total)
	if len(fields) == 0 {
		return summary
	}
	named := make([]string, len(fields))
	for i, f := range fields {
		if f == "authors" {
			f = "author"
		}
		named[i] = f
	}
	return summary + " · matched " + strings.Join(named, ", ")
}
