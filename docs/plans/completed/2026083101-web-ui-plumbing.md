# Step: Web UI service layer and templates/CSS file layout

## Context

The scanner and storage layers are done — every book has metadata and a
cover, but there's no way to look at any of it except `sqlite3`. Real
mockups now exist (Claude Design, from `UI.md`), so the next piece is the
plumbing that will hold them: DESIGN.md's Web UI section calls for
server-rendered Go templates + htmx, with **no JSON API for the UI** and
business logic living in **a service layer beneath thin HTTP handlers** —
explicitly so a future `/api/v1` can sit beside the web handlers as a second
transport over the same service calls, instead of logic getting trapped in
template handlers.

This step builds that plumbing — the service layer, the route/handler
skeleton, and the `go:embed` template/CSS file layout — and wires it
together end-to-end with a placeholder page. It deliberately does **not**
write the real templates or CSS: that's a follow-up step once the actual
Claude Design markup is translated into Go templates. The one real template
file this step adds is scaffolding, not the browse page.

**Scope boundary.** Only the library-browse read path (`ListBooks`) is
built now — it's the one piece of DESIGN.md's data model that's fully
populated already (books, authors, covers). Book detail, inline editing,
search, and send-to-Kindle all need features that don't exist yet
(FTS5 index, recipients/send_log, the Resend integration) and stay out of
scope here.

## `internal/service` (new package)

The layer DESIGN.md's "Layering for a future API" section calls for —
importable by both `internal/web` (this step) and a future `internal/api`,
so it can't live inside the web package itself.

**`service.go`**
- `Service` struct wrapping `*storage.DB`; `New(db *storage.DB) *Service`.
- `BookSummary` struct: `ID, Title, Authors []string, Format, CoverPath` —
  exactly what a library-grid entry needs, nothing UI.md doesn't ask for.
- `ListBooks(ctx) ([]BookSummary, error)`: fetches all books (ordered by
  `sort_title`) and their authors, and assembles them. Backed by two new
  `internal/storage` methods (below) rather than the service package
  writing SQL directly, matching how `internal/scanner` already only calls
  typed `storage` methods.

**`internal/storage` additions** (`books.go`):
- `ListBooks(ctx) ([]Book, error)` — all books, `ORDER BY sort_title`.
- `ListBookAuthors(ctx) (map[int64][]string, error)` — every book_id ->
  author names mapping in one query (a `JOIN` on `book_authors`/`authors`),
  for the service layer to assemble against `ListBooks`'s results. At
  personal-library scale, loading the whole mapping in one shot beats an
  N+1 per-book author query.

Not included yet, called out so it isn't mistaken for an oversight: a
location count / "has multiple files" flag on `BookSummary` (UI.md asks for
this indicator, and `book_files` already supports it) — left for once a
real template actually needs to render it.

**`service_test.go` / storage test additions** — round-trip `ListBooks` and
`ListBookAuthors` against a temp DB seeded via the existing
`CreateBook`/`UpsertBookFile` methods; a service-level test confirming
`ListBooks` assembles the right authors onto the right book, including a
book with zero authors and a book with several.

## `internal/web` (new package) — routes, handlers, template/CSS layout

**`web.go`**
- `Routes(svc *service.Service) http.Handler` — builds and returns the
  mux. Uses Go 1.22+ method-pattern routing (`mux.HandleFunc("GET /", ...)`)
  already implied by this module's `go 1.26.1`.
- One thin handler for now: parses nothing, calls
  `svc.ListBooks(r.Context())`, renders. Matches DESIGN.md's "handlers stay
  thin: parse request, call service method, render."

**`render.go`**
- `//go:embed templates/*.html` into an `embed.FS`; `template.Must(template.ParseFS(...))` at package init.
- A small `render(w http.ResponseWriter, name string, data any) error`
  helper wrapping `ExecuteTemplate`, with the HTTP error logging pattern
  already used in `cmd/server/main.go`.
- `//go:embed static` into a second `embed.FS`, mounted at `GET /static/`
  via `http.FileServerFS` — embedding the `static` directory *with* its
  `static/` prefix intact means the embedded paths already match the URL
  paths, so no `http.StripPrefix` juggling is needed.

**File layout** (the actual deliverable this step is about):

```
internal/web/
  web.go
  render.go
  web_test.go
  templates/
    library.html      // placeholder only — proves the pipeline, not the real page
  static/
    css/
      app.css          // placeholder only — empty/minimal, real CSS comes later
```

`library.html` and `app.css` are intentionally trivial (e.g. a bare
`<ul>{{range .}}<li>{{.Title}}</li>{{end}}</ul>`, an empty stylesheet) —
just enough content to satisfy `go:embed` (an embed pattern matching zero
files fails the build) and to prove data flows all the way from SQLite to
an HTTP response. The real markup and the real page/partial breakdown
(base layout, the htmx-swapped search/send-status/inline-edit fragments
UI.md describes) get designed in the next step, once the actual Claude
Design HTML is in hand — guessing that structure now would likely just get
thrown away.

**`web_test.go`** — `httptest`-based: `GET /` against `Routes(svc)` backed
by a seeded temp DB returns `200` and body containing a known seeded book's
title; `GET /static/css/app.css` returns `200`.

## Wiring (`cmd/server/main.go`)

- Construct `service.New(db)` after `storage.Open`.
- Mount `web.Routes(svc)` on the existing `mux` alongside `/healthz` (the
  existing manual `mux.HandleFunc("/healthz", ...)` stays as-is).

## CLAUDE.md

Add `internal/service` and `internal/web` to "Current implementation":
what `ListBooks` does, and that the web layer is routes+a placeholder page
only — real templates are still to come.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean, including the
  new storage/service/web tests above.
- Manual: point `LIBRARY_DIR` at a folder with a couple of real EPUBs (or
  reuse the fixture-generation approach from earlier steps), `go run
  ./cmd/server`, then `curl localhost:8080/` and confirm the scanned
  books' titles come back in the (placeholder) HTML response — proving
  storage -> service -> handler -> template -> HTTP actually works
  end-to-end before any real design goes into it.
