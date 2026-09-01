# Step: Book detail page

## Context

Every unbuilt in-scope item was weighed for "next": the filesystem
watcher, the send-to-Kindle prerequisites (schema, job model, picker),
provider enrichment with provenance, the multi-location badge, and this.
The detail page wins on three grounds:

- **It just became unblocked, deliberately.** The web-transport
  correctness step (`docs/plans/completed/2026083112`) removed the
  catch-all precisely so that "a `/books/{id}` route can exist" without a
  stale link silently rendering the whole library. That route is this
  step.
- **It's the mounting surface for the primary action.** UI.md puts the
  send-to-Kindle control and inline metadata editing on the book detail
  view. Both are blocked on their own prerequisites (job model; field
  provenance) — but when those land, they need this page to land on.
  Building the send backend first would produce a feature with no UI
  home.
- **The data is finally all there.** EPUB metadata completeness
  (`2026083114`), FB2 metadata (`2026090103`) and author ordering
  (`2026090102`) mean every field UI.md's detail view shows — title,
  authors, publisher, published date, language, ISBN, description,
  format, file size — is populated for both formats. A month ago this
  page would have been mostly blank rows.

No schema change, no migration. Everything displayed already exists in
`books`, `authors`/`book_authors` and `book_files`.

**Design is settled, not open.** The Claude Design handoff now lives at
`ui-handoff/` on the `init` branch (checked out at the
`/Users/mike/dev/library-init` worktree; note the remote `init` doesn't
have the handoff commit at the time of writing — push it before relying
on the remote). Plates 03 (read state) and 04 (sparse metadata) of
`ui-handoff/mockups/Bookshelf Mockups.dc.html` are this page, drawn in
both themes; `ui-handoff/SCREENS.md` sections 03/04 carry the build
notes and `ui-handoff/TOKENS.md` the values. Everything visual in this
plan follows those, and where an earlier draft of this plan guessed
differently (dropped empty rows, inline location paths), the mockup
wins — the specifics are called out below.

## Scope

In scope: a `GET /books/{id}` page — larger cover, full metadata, file
location(s) — plus the grid cards linking to it, and the storage/service
methods to feed it.

Out of scope, with reasons:

- **Inline editing** — gated on field provenance, per DESIGN.md; editing
  without it means enrichment later clobbers hand-fixes.
- **The send control** — gated on the recipients/send_log schema and job
  model. The page should leave an obvious place for it (see Markup), not
  mock it.
- **Full-resolution covers.** Only the ~400px thumbnail is stored;
  DESIGN.md's covers section explicitly defers on-demand full-res
  extraction. The detail page shows the stored thumbnail at a larger
  display size than the grid card, which is what "larger cover" can mean
  today.
- **The multi-location badge on the grid.** Unblocked now, but a separate
  concern; note that this page listing a book's locations delivers most
  of that item's value anyway (the badge is just its grid-level echo).

Pattern: a **dedicated page with a back link**. UI.md left this open
("expanded card, row, or side panel — your call"), and the mockups have
since made the call — SCREENS.md 03 opens with "Its own page with a back
link, not a panel or modal" — which also happens to be the only pattern
buildable in the current no-JS state (htmx arrives with the search step,
`2026090106`). Every book gets a shareable URL.

## Storage

Two additions to `internal/storage`:

- `FindBookByID(ctx, id) (*Book, error)` — nil when absent, matching the
  other finders. (This exact method existed briefly and was removed as
  unused during the cover-regeneration review fixes; it comes back with
  an actual caller.)
- `ListBookFiles(ctx, bookID) ([]BookFile, error)` — the book's
  locations, ordered by `file_path` for stable display. A targeted query,
  not a filter over `ListFilesUnder("")`: the page fetches one book, so
  fetching every location in the library to keep one row's worth would be
  backwards.

Authors: add `ListAuthorsForBook(ctx, bookID) ([]string, error)`
(`ORDER BY ba.position`). `ListBookAuthors` loads the whole library's
author map, which is right for the grid page and wrong for a single-book
page.

## Service

`Service.GetBook(ctx, id int64) (*BookDetail, error)` — nil, nil when the
book doesn't exist (the transport turns that into a 404; "absent" is not
an error at this layer, consistent with the storage finders).

```go
type BookDetail struct {
    ID            int64
    Title         string
    Authors       []string
    Publisher     string
    PublishedDate string
    Language      string
    ISBN          string
    Description   string
    CoverPath     string
    Format        string
    FileSize      int64
    AddedAt       time.Time
    Locations     []FileLocation
}

type FileLocation struct {
    Path    string
    Missing bool
}
```

One deliberate shape decision: **`FileSize` is a book-level field, not a
per-location one.** Locations of one book are byte-identical by
construction — content hash is identity — so their sizes are equal, and
showing a size per path would imply a difference that cannot exist. Take
it from the first location. `Locations` carries only the relative path
and whether the row is currently marked missing (`missing_since` set), so
the page can show a location that's inside its grace period as such
rather than pretending it's fine.

A book with zero locations can't be observed — the last location's
deletion prunes the book in the same transaction — so `GetBook` treats
"book row exists, no files" as the race it is and simply renders no size
rather than erroring.

## Web transport

- Route: `mux.HandleFunc("GET /books/{id}", …)` in `Routes`. Parse
  `r.PathValue("id")` with `strconv.ParseInt`; a non-numeric id and an
  unknown id are both plain 404s (`http.NotFound`), indistinguishable on
  purpose — neither is a client error worth a distinct page.
- Template: `book.html` alongside `library.html`, reusing the
  `document-head`, `site-header` and `site-scripts` partials, translated
  from plates 03/04 the way the grid page was translated from plate 01
  (the handoff's `library-ui.patch` is the reference for that
  translation style). New CSS extends the embedded `app.css` using the
  existing token variables only — `app.css` already embodies
  `tokens.css`, and TOKENS.md is explicit that literals break dark mode.
  Layout per SCREENS.md 03: a two-column grid (`300px` cover rail,
  `minmax(0, 1fr)` content, gap `44px`), cover at `aspect-ratio: 0.66`
  with the mono file-facts list (format, size, locations) beneath it;
  right column ordered title/author → send slot → description →
  metadata table. The metadata table is hairline rows, mono label /
  sans value, ISBN in mono; title serif at `max-width: 26ch`,
  description serif at `max-width: 62ch`.
- View model (`bookPage`), shaped in the handler so the template stays
  logic-free:
  - Authors joined for display in full — the grid's "and N others"
    collapse is a card-width concession the detail page doesn't need.
  - `FileSize` formatted human-readable (`12.3 MB`) by a small handler
    helper; raw bytes available in a `title` attribute if cheap.
  - `AddedAt` formatted as a date; `PublishedDate` displayed **as
    stored** — it's free-text from embedded metadata (sometimes a year,
    sometimes a full date), and pretending to parse it would lie
    confidently.
  - Empty optional fields (publisher, ISBN, language, description)
    render as **visible rows with an em dash**, not dropped — plate 04's
    rule, reversing this plan's earlier draft: "a hidden field can't be
    filled in", and sparse metadata is the common FB2 case, drawn as a
    first-class state. One deviation until inline editing exists: the
    mockup's italic "click to add" invitations promise an interaction
    this step doesn't build, so render the same italic `--fg-faint`
    treatment with just "Author unknown" / "No description" — the
    "click to add" wording arrives with the editing step. No cover
    renders the dashed "no cover" box at the same footprint as a real
    cover, per the plate.
- Field-granular markup: one element per metadata field, since plate 05
  swaps read view for edit view in place, per field — getting the
  granularity now means wiring editing later is connecting markup, not
  redesigning it. The send slot keeps its plate-03 position in source
  order (above the description — "it is the reason the page gets
  opened") as a bounded container, but stays empty and collapsed rather
  than mocking plate 06's `min-height: 148px` states; that height rule
  applies when the control exists to swap.
- Locations: per SCREENS.md 03, the rail shows a location **count** with
  the dotted-underline accent affordance, paths "revealed on demand, not
  listed inline". No JS exists yet, so the reveal is a native
  `<details>`/`<summary>` styled to the token set — the summary is the
  count, the expansion lists relative paths (storage stores them
  relative to the library root, which is the right display form — the
  absolute mount point is deployment detail), each missing-marked
  location carrying a subdued "missing" annotation.
- Grid cards in `library.html` become links: wrap the card content in
  `<a href="/books/{{.ID}}">`. Card markup only — no handler change on
  the grid side.

### Coordination with the search plan

`2026090106` (unbuilt as of writing) also touches `library.html` and
`partials.html` (grid extraction into a `book-grid` partial). The two
steps are independent — whichever lands second rebases mechanically,
since the card-link change lives inside the card markup both plans leave
intact. No ordering requirement.

## Tests

`internal/storage`:

- `FindBookByID` round-trips a created book; unknown id → nil, nil.
- `ListBookFiles` returns exactly the book's locations, ordered by path,
  with `missing_since` surfaced; a book with two locations returns two
  rows and another book's files don't leak in.
- `ListAuthorsForBook` preserves source order (positions), and returns
  empty (not nil-error) for an authorless book.

`internal/service`:

- `GetBook` assembles the full shape: metadata, ordered authors, size
  from a location, locations with the missing flag.
- Unknown id → nil, nil.

`internal/web`:

- `GET /books/{id}` for a real book: 200, title/author/format/size
  present in the body, and a back link to `/`.
- Sparse book: the empty field's row is still present with its em dash
  (asserting plate 04's visible-row rule, not just happy path), and no
  "click to add" text appears — that wording belongs to the editing
  step.
- Non-numeric id and unknown numeric id both 404.
- The grid page's cards carry `href="/books/{id}"`.
- A two-location book renders the count in the summary and both paths in
  the `<details>` expansion; a missing-marked location shows the
  annotation.

## CLAUDE.md

- `internal/storage`: `FindBookByID`, `ListBookFiles`,
  `ListAuthorsForBook`.
- `internal/service`: `GetBook`/`BookDetail`, the nil-on-absent
  contract, the book-level `FileSize` reasoning.
- `internal/web`: the `/books/{id}` route and its 404 behaviour; cards
  link to it. Remove "book detail" from the still-missing list (search,
  inline editing and send-to-Kindle remain).
- DESIGN.md's status table and Web UI status note: book detail moves to
  Built when this lands.

## Verification

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...` clean.
- Manual, against the real library: click through from the grid to
  several books — one EPUB with full metadata, one FB2, one with sparse
  metadata (no description/ISBN), one with no cover, and one with
  multiple locations if the library has a duplicate. Compare each
  against plates 03/04 in `ui-handoff/mockups/Bookshelf Mockups.dc.html`
  side by side, **in both themes** (the mockup header toggles dark
  mode). Confirm the back link returns to the grid, a mistyped
  `/books/99999` 404s, the `<details>` reveal works without JS, and long
  descriptions/titles — including non-Latin titles, which plate 04 calls
  out — don't break the layout.
