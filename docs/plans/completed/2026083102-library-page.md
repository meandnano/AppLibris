# Step: Library page — real template and CSS

## Context

`docs/plans/completed/2026083101-web-ui-plumbing.md` built the web layer's
plumbing and deliberately stopped short of real markup: `library.html` is a
bare `<ul>` of titles and `app.css` is an empty placeholder. The Claude
Design mockups from `UI.md` now exist, so this step translates the one
screen the current data model can actually fill — the library grid — into a
Go template and a real stylesheet.

## Scope

In scope: the library grid, its empty state, cover serving, and the shared
page chrome (masthead, light/dark).

Out of scope, because the data or backing feature doesn't exist yet: the
book detail page, inline metadata editing, search, send-to-Kindle and its
status states, and send history. Those mockups exist but need FTS5, a
`recipients`/`send_log` schema, and the mail integration first; each gets
its own plan. The grid card's multi-location indicator is also left out —
`BookSummary` still carries no location count (called out as deliberate in
the plumbing plan).

## Serving covers

`books.cover_path` is an absolute filesystem path inside `COVERS_DIR`, which
nothing serves today, so a template can't reference it. `Routes` takes
`coversDir` and mounts it read-only at `GET /covers/` via
`http.FileServer(http.Dir(coversDir))`. Covers are named by content hash in
one flat directory, so `coverURL` is just `"/covers/" + filepath.Base(p)`.
`cmd/server` already resolves `coversDir` and passes it to the scanner; it
now passes the same value to `web.Routes`.

Covers are not embedded like `static/` — they're runtime data written by the
scanner, not build-time assets.

## Handler shaping

Handlers stay thin, but templates stay logic-free, so `libraryHandler` maps
`[]service.BookSummary` to a `[]bookCard` view model: authors flattened by
`authorLine` (one name as-is, two joined with `&`, three or more collapsed
to "First and N others" — a card is one line wide) and `CoverURL` derived as
above. The page struct carries `Title` and `Count` for the shared chrome.

## Templates

`templates/partials.html` holds `document-head`, `site-header` and
`site-scripts`; `library.html` is the page. No base/block inheritance:
`ParseFS` puts every template in one set, so two pages defining `content`
would collide. Pages including named partials scales to the detail and
history pages without that problem.

## Styling

`static/css/app.css` carries the design as CSS custom properties: colour
tokens, the three families (Newsreader for titles, IBM Plex Sans for UI, IBM
Plex Mono for machine text), and the grid. Dark mode is the same tokens
redeclared — `prefers-color-scheme` by default, overridden by
`data-theme` on `<html>`, which `static/js/theme.js` sets and persists in
`localStorage`. A tiny inline script in the head applies the stored value
before first paint so a dark-mode reader gets no light flash.

Covers vary in aspect ratio, so each card's cover hangs from a fixed-height
shelf, bottom-aligned to it and left-aligned within the card, giving the grid
a shelf line while titles stay flush with their cover's left edge. The grid is
`auto-fill` over a fixed card width, so it reflows from desktop to phone
without a breakpoint; one breakpoint at 640px shrinks the shelf and padding.

Fonts load from Google Fonts with full local fallback stacks, so a
self-hosted instance with no outbound network degrades to Georgia and
system-ui rather than breaking.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- `web_test.go`: the grid renders a seeded book's title and author; an empty
  library renders the empty state; `/static/css/app.css` and a file in
  `coversDir` both serve; unit tests for `authorLine` and `coverURL`.
- Manual: point `LIBRARY_DIR` at real EPUBs, `make run`, and confirm covers
  render, the shelf line holds across mixed aspect ratios, and the theme
  toggle survives a reload.
