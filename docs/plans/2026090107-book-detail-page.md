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

Pattern choice UI.md leaves open ("expanded card, row, or side panel —
your call"): a **dedicated page**. A side panel or expanded card needs
client-side behaviour, and htmx isn't vendored until the search step
(`2026090106`) lands — a plain server-rendered page matches the current
no-JS state, gives every book a shareable URL, and nothing prevents a
later htmx enhancement from loading the same fragment into a panel.

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
  `document-head`, `site-header` and `site-scripts` partials. New CSS
  extends the embedded `app.css`; consult the detail mockups on the
  `init` branch (`UI.md` and the Claude Design canvas) for look and
  spacing, as the grid page did.
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
  - Empty optional fields (publisher, ISBN, description, language) drop
    their rows entirely rather than rendering "—" filler; UI.md is
    explicit that sparse metadata is the case to design for. Zero authors
    renders the same "Author unknown" treatment the card uses; no cover
    renders the placeholder block at detail size.
- Metadata rendered as a definition list (or equivalent) with one
  element per field — this is the structure inline editing will later
  swap per-field, per UI.md's "distinct piece of markup for each state"
  instruction, so getting field-granular markup now costs nothing and
  saves a redesign. Likewise leave one clearly-bounded container where
  the send control and status area will go — empty today, not mocked.
- Locations: shown as a list of relative paths (storage stores them
  relative to the library root, which is exactly the right display
  form — the absolute mount point is deployment detail). A missing-marked
  location gets a subdued "missing" annotation.
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
  present in the body; description absent from the body when the field is
  empty (asserting the dropped-row rule, not just happy path).
- Non-numeric id and unknown numeric id both 404.
- The grid page's cards carry `href="/books/{id}"`.
- A book whose location is marked missing shows the annotation.

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
  multiple locations if the library has a duplicate. Confirm the back
  button returns to the grid, a mistyped `/books/99999` 404s, and long
  descriptions/titles don't break the layout.
