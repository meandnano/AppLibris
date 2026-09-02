# Step: Inline metadata editing

## Context

This is the **inline-editing half** of DESIGN.md's remaining “Web UI:
inline editing, send control” row. The send-control half already has an active,
detailed vertical-slice plan in `docs/plans/2026090201-send-to-kindle.md`.
Keeping the steps separate is intentional: they meet only in the book-detail
template and CSS, while their durable state, service operations and failure
modes are unrelated. Either may land first without leaving the other half
implemented or forcing an artificial deployment order.

The detail page from
`docs/plans/completed/2026090107-book-detail-page.md` deliberately renders
metadata one field at a time and leaves sparse values visible. That gives this
step stable places to attach plate 05's read/edit swaps without redesigning the
page. The htmx and progressive-enhancement conventions come from
`docs/plans/completed/2026090106-full-text-search.md`: thin handlers over
`internal/service`, named fragments, a full-page response when JavaScript is
absent, and `Vary` whenever one URL can return both forms.

The non-visual prerequisite is field provenance. DESIGN.md is explicit that
manual editing must not ship before provenance because a later provider pass
would otherwise overwrite hand-fixed values. Provider lookup and enrichment
remain out of scope, but the durable contract they need does not: embedded
values are recorded as `embedded`, every submitted manual value is recorded as
`manual`, and that marker survives even when the user deliberately clears a
field. A future resolver can then fill only an unclaimed field and can never
mistake an intentionally blank manual value for missing metadata.

The visual source of truth is plates 03–05 in `ui-handoff/` on `init`, especially
SCREENS.md's read/edit parity rule and TOKENS.md's constant-swap-height rule.
The implementation uses the existing token names in `app.css`; it does not copy
literal colours or introduce generic rounded form controls.

## Scope

In scope:

- Inline editing for title, authors, description, publisher, published date,
  language and ISBN on `GET /books/{id}`
- Field-source storage and creation-time `embedded` provenance for those seven
  user-editable fields
- Atomic metadata, author-relation, provenance and FTS updates
- htmx read/edit fragment swaps plus a complete no-JavaScript form path
- Sparse-field invitations, validation feedback, focus and keyboard handling
- FTS/search and library-sort consistency immediately after a manual edit

Out of scope:

- **Cover editing.** No cover-upload or replacement state exists in the
  handoff, and file handling would be a separate feature. `cover_path` is not
  silently made editable through a generic endpoint.
- **Format, size, locations and added date.** These are file/system facts, not
  editable metadata.
- **`sort_title` as a visible field.** It remains derived from title using the
  scanner's existing article-stripping and case-folding rule. Exposing both
  would let the invariant drift for no user-visible benefit.
- **Provider enrichment or provenance display.** This step builds the contract
  providers must respect, but UI.md explicitly excludes provider results and
  provenance UI.
- **Edit history, undo and optimistic locking.** The server is single-user and
  manual edits are infrequent. Each submission is atomic and last submission
  wins; adding versions would create a second persistence feature not present
  in the design.
- **Book deletion, cover replacement, recipient management and send history.**
  None is part of inline metadata editing.

## Schema and provenance contract

Add two migrations after the five `2026090201`–`2026090205` filenames already
reserved by the sibling send-control plan. Migration numbers are independent of
plan numbers, and reserving unique names keeps either implementation order from
creating a collision.

1. `2026090206_create_field_sources_table.sql`

   ```sql
   CREATE TABLE field_sources (
       book_id INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
       field   TEXT NOT NULL CHECK (field IN (
           'title', 'authors', 'publisher', 'published_date',
           'language', 'isbn', 'description'
       )),
       source  TEXT NOT NULL,
       PRIMARY KEY (book_id, field)
   );
   ```

   The composite primary key is the only index needed for this step and for a
   future resolver asking for one book's field sources. `source` is deliberately
   not a closed enum: `embedded` and `manual` are the sources available today,
   while provider names are compile-time registrations DESIGN.md has not built
   yet. The `field` set is closed because it names actual mutable data and a typo
   there would permanently weaken the overwrite guard.

2. `2026090207_backfill_embedded_field_sources.sql`

   One `INSERT ... SELECT` statement joined with `UNION ALL` records
   `embedded` for each existing non-empty scalar and records `authors` once for
   every book having at least one `book_authors` row. Empty values get no row:
   they are genuinely available to future enrichment until a user explicitly
   clears them, at which point the write path creates a `manual` row despite the
   empty value.

Do not add `sort_title`, `format`, file facts or timestamps to this table.
`sort_title` is derived; the others do not participate in metadata source
selection. Do not add `cover_path` in anticipation of an unplanned cover editor;
the future provider step can extend the field check in its own migration once
its cover-merging policy exists.

Creation-time provenance is part of the invariant, not a backfill-only repair.
`createBookTx` records `embedded` for each non-empty editable scalar and for a
non-empty author list in the same transaction that inserts the book, author
links and FTS row. Existing `CreateBook` and `CreateBookWithFile` signatures stay
stable: every production creation call currently comes from the embedded
metadata scanner, and no provider or manual-create path exists. Document that
default at the storage boundary so a future caller must deliberately extend the
creation API rather than accidentally mislabel provider data.

## Shared field vocabulary

Define a typed `storage.MetadataField` and constants for the seven accepted
names. The storage switch, service validation and web route parser all use that
vocabulary; raw path text never becomes a SQL identifier. Add
`ParseMetadataField(string) (MetadataField, bool)` for the transport boundary.

The field groups are:

- single-line required: `title`
- ordered list: `authors`
- multiline: `description`
- single-line optional: `publisher`, `published_date`, `language`, `isbn`

Authors are submitted as one name per line. A comma is valid inside a person's
name, so comma-splitting would make the data model less expressive than it is
today. Trim surrounding whitespace on each line, discard blank lines and
deduplicate exact repeated names at first occurrence, matching
`createBookTx`'s current relation rule. An empty list deliberately produces an
authorless book and a `manual` source marker.

Trim surrounding whitespace on every scalar. Preserve internal whitespace and
newlines in description. Title must remain non-empty after trimming; all other
fields may be cleared. Bound title and each author name to 1 KiB, each optional
single-line value to 4 KiB, the author list to 100 names, and description to
64 KiB. These are corruption/request bounds, not UI counters. The handler uses
`http.MaxBytesReader` before `ParseForm`, so bypassing browser constraints cannot
make an unbounded form allocation.

## Storage writes

Add `internal/storage/metadata.go` rather than extending the already-large
`books.go` with the new public write surface. Package-internal helpers may stay
beside the existing author and FTS helpers when sharing them avoids cycles.

Expose two operations:

```go
func (db *DB) UpdateBookField(
    ctx context.Context,
    bookID int64,
    field MetadataField,
    value string,
    modifiedAt time.Time,
) (bool, error)

func (db *DB) UpdateBookAuthors(
    ctx context.Context,
    bookID int64,
    names []string,
    modifiedAt time.Time,
) (bool, error)
```

The boolean reports whether the book existed, preserving the storage/service
contract that an unknown id is absence rather than an operational error.

Each method owns exactly one `DB.Write` transaction. Inside it:

1. Confirm the book exists before changing related rows
2. Update the scalar column, or replace that book's `book_authors` links in the
   submitted order using the existing find-or-create author helper
3. For a title update, recompute `sort_title`
4. Set `books.modified_at` using `formatTime(modifiedAt)`
5. Upsert `(book_id, field, 'manual')` into `field_sources`, including when the
   new value/list is empty
6. Call `syncBookFTSTx` before commit

Recomputing FTS for every editable field is deliberately simple. Only title,
authors, description and ISBN affect the index today, but one shared atomic path
is cheaper to reason about than a second field classification that can drift
when search grows. The database's single writer and this library's edit volume
make the extra scoped delete/insert negligible.

Never interpolate `field` into SQL. Use a switch whose scalar cases execute
fixed statements; reject `authors` from `UpdateBookField` and require the
relation-specific method. This keeps both SQL injection and an accidental edit
of a non-user field impossible below the HTTP layer.

The existing title-to-sort-title helper currently lives in `internal/scanner`.
Move it to `internal/storage` as `SortTitle`, retain its current tests, and call
it from both scanner ingestion and the title-update transaction. There must be
one rule for the initial sort key and the edited sort key. Do not change its
English-article behavior in this step.

## Service layer

Add an explicit update command rather than exposing storage types through the
web package:

```go
type MetadataUpdate struct {
    Field string
    Value string
}

var ErrInvalidMetadata = errors.New("invalid metadata")

func (s *Service) UpdateBookMetadata(
    ctx context.Context,
    bookID int64,
    update MetadataUpdate,
) (*BookDetail, error)
```

`UpdateBookMetadata` parses the field, applies the group-specific normalization
and limits above, and returns an error wrapping `ErrInvalidMetadata` with a
short user-facing explanation when invalid. It uses the service's clock rather
than `time.Now` buried in storage; add a private `now func() time.Time` defaulted
by `New` and overridden in package tests. It calls the appropriate atomic
storage method, returns `nil, nil` for an unknown book, then reloads `BookDetail`
so the response fragment comes from committed canonical data rather than echoing
the request.

Keep validation here because a future API must receive the same rules. The web
handler parses form shape and chooses a response; it does not trim, split or
decide whether a title is valid.

## Web routes and response contract

Add two method-specific route shapes:

- `GET /books/{id}/metadata/{field}` renders that field's read or edit state;
  `?edit=1` selects edit state
- `POST /books/{id}/metadata/{field}` validates, writes and renders the committed
  read state

The enclosing book id and field are parsed identically for both. A malformed id,
unknown field or missing book returns the same plain 404. Unknown fields are not
400: they identify no route-level resource, and treating them as arbitrary form
input would imply the generic updater this plan explicitly forbids.

For htmx requests, GET and POST return only the stable field wrapper. An invalid
POST returns the edit wrapper with the submitted value and inline error text at
status 200 so htmx 2.0.10's default response policy actually swaps it; that
vendored policy does not swap 4xx responses. The ordinary full-page path below
still uses status 422, preserving the useful HTTP distinction where it will not
silently defeat the interaction. Add `Cache-Control: no-store` to edit and
mutation responses; a browser or shared cache must not restore a stale edit form
or pre-edit fragment.

For ordinary requests:

- `GET ...?edit=1` redirects to `/books/{id}?edit={field}`
- read-state GET redirects to `/books/{id}`
- a successful POST uses `303 See Other` to `/books/{id}`
- an invalid POST renders the complete detail page at status 422 with that field
  in edit mode, its submitted value and the validation message

Extend `bookDetailHandler` to accept one valid `edit` query value. An invalid
value is ignored rather than turning a valid book page into a 404. This is the
no-JavaScript fallback and also makes the browser Back button leave edit mode
predictably.

Every URL above can yield a fragment or navigation response depending on
`HX-Request`, so set `Vary: HX-Request, HX-History-Restore-Request`. Treat an htmx
history-restore request as an ordinary browser request, matching the search
handler's established rule; a fragment must never replace the full document
during history recovery.

Use one handler helper to build the full `bookDetailPage` for initial GET,
no-JavaScript edit, and validation failure. Do not duplicate the count, cover,
size, location and author shaping in the metadata handler.

## Templates and interaction

Split the editable pieces into named templates in `partials.html` with one
stable outer element per field:

- `book-field-title`
- `book-field-authors`
- `book-field-description`
- `book-field-meta` for each table-shaped optional field

Each wrapper has `id="book-field-{field}"` and `data-editable-field`. Read-state
links use `hx-get`, `hx-target="closest [data-editable-field]"` and
`hx-swap="outerHTML"`; their real `href` is the full-page `?edit={field}` URL.
Cancel uses the read-state endpoint in the same target, with `/books/{id}` as
its non-htmx `href`. Forms carry real `method="post"` and `action`, plus matching
`hx-post`/target/swap attributes.

Preserve semantic markup in read mode: the title remains the page's `h1`, the
authors and description remain paragraphs, and metadata remains a `dl`. The
edit affordance is a real link/button with an accessible name, not a click
handler on a `div`. Empty values use the handoff's invitations (“Author unknown
— click to add”, “No description — click to add one”) and em-dash rows, but the
accessible name says which field will be edited.

Plate 05's parity rule is the acceptance criterion:

- Read and edit wrappers use identical padding, font, line height and minimum
  block size
- Read controls have no input chrome at rest; hover/focus reveals
  `--bg-sunken` and the mono accent “edit” label using negative outer margins
  that do not move surrounding content
- Edit controls use `--accent`, `--accent-soft`, square corners, the field's
  display typography, primary Save and bordered Cancel
- Title keeps the detail-page serif scale; description keeps its serif body
  scale and a `min-height: 108px`; metadata values stay sans; ISBN stays mono
- The send panel, when the sibling send plan lands, remains above description
- At the detail page's existing narrow breakpoint, buttons wrap without
  horizontal overflow and retain the 34px minimum tap target

Add `internal/web/static/js/edit.js` only for progressive ergonomics: on an
inserted edit fragment, focus/select the input; Escape activates Cancel; Enter
submits single-line inputs; `Ctrl+Enter`/`Meta+Enter` submits textareas while
plain Enter remains a newline. The server and all save/cancel actions continue
to work without it. Render hints that match those real bindings: “⏎ save · esc
cancel” for single-line inputs and “⌘/ctrl + ⏎ save · esc cancel” for textareas.
Include the script through the existing `site-scripts` partial so full pages and
htmx-inserted forms share one delegated listener.

After a successful title swap, the same delegated `htmx:afterSwap` listener
updates `document.title` to “{title} · Bookshelf”. Without this, the visible
heading would change while the browser tab retained the old title until reload.
Do not add a head-swapping extension for this one text assignment.

Use an `aria-live="polite"` error element in the edit wrapper. On successful
swap, return focus to the read affordance for that field; implement this with
the same delegated script rather than inline JavaScript or per-field handlers.

## Interaction with search and scanner behavior

Manual edits must be visible everywhere immediately after commit:

- `syncBookFTSTx` makes title, author, description and ISBN changes searchable
  without a rescan
- updating `sort_title` with title changes moves the card to its new library
  order on the next browse render
- the detail response reloads canonical data, so normalization is visible

The scanner's known-content path does not currently reapply embedded metadata;
it only refreshes file facts and covers. Keep that behavior. Creation-time
field-source rows establish the rule a future enrichment/rescan implementation
must use: `manual` wins even when the stored value is blank. Add a storage test
that makes this empty-manual case explicit so later provider work cannot infer
provenance from value emptiness.

## Tests

`internal/storage`:

- A newly created book records `embedded` for each present editable field and
  authors, records nothing for absent fields, and creation plus provenance plus
  FTS is atomic on failure
- The backfill marks populated scalar/author fields on a database fixture from
  the previous schema and leaves empty fields unclaimed
- Updating each scalar changes only its intended column, bumps `modified_at`,
  records `manual`, and leaves a manually cleared optional field with a manual
  source row
- Title updates recompute `sort_title`; title, author, description and ISBN
  updates replace the FTS row and are immediately searchable only by the new
  value
- Author replacement preserves submitted order, deduplicates repeated names,
  supports clearing the list and leaves unrelated books sharing an author
  unchanged
- Unknown book ids return `false` without creating provenance; `authors` cannot
  pass through the scalar updater; invalid field text never reaches SQL

`internal/service`:

- Table-driven coverage for every field's trimming, size/count limits and empty
  policy, including a whitespace-only title error and a deliberately empty
  author list
- The injected clock is passed through to `modified_at`
- A successful update returns reloaded canonical detail; an unknown book returns
  nil; validation wraps `ErrInvalidMetadata` and writes nothing

`internal/web`:

- Every detail field renders a read affordance, including sparse author,
  description and optional metadata states
- htmx edit GET returns only the correctly typed form wrapper; cancel/read GET
  returns only the read wrapper; both carry the stable id and swap attributes
- Successful htmx POST returns escaped canonical read markup; invalid htmx POST
  returns a swappable 200 edit fragment, preserves escaped submitted text,
  includes the accessible error and performs no write, while the corresponding
  no-JavaScript response is 422
- Non-htmx edit GET redirects to `?edit=field`; the full page renders exactly
  that field in edit mode; successful POST is a 303; invalid POST is a complete
  422 page
- A history-restore request receives navigation behavior rather than a fragment,
  and response headers vary on both htmx headers
- Non-numeric ids, unknown books and unknown field names 404; oversized form
  bodies are rejected before parsing
- Markup assertions cover title/description typography hooks, textarea minimum
  height hook, keyboard hints and the send-before-description ordering

Run existing storage FTS, service search, library page and book detail suites
unchanged as regression coverage.

## CLAUDE.md

After implementation, update the current-implementation map:

- `internal/storage`: field-source schema; absent-versus-cleared provenance
  semantics; atomic scalar/author/provenance/FTS updates; shared sort-title rule
- `internal/service`: the metadata update command, normalization/validation and
  injected clock
- `internal/web`: metadata routes, named field fragments, 422 behavior,
  history-restore handling and no-JavaScript `?edit=` path
- Remove inline metadata editing from the still-missing list; retain provider
  enrichment and provenance display as missing

Do not update DESIGN.md in this implementation branch: it lives on `init`, while
the implementation and current-state map live on `master` by repository
convention.

## Verification

- `gofmt -w` on touched Go files, then `go build ./...`, `go vet ./...` and
  `go test -count=1 ./...`
- Manual desktop pass in both light and dark themes against plates 03–05: edit,
  cancel, clear and save every field; confirm no vertical or horizontal jump
  between read/edit states and no hard-coded colour breaks either theme
- Manual sparse-book pass: add an author and description from their invitation
  states, fill and then clear every optional metadata row, and confirm each
  remains editable
- Manual search pass: change title, authors, description and ISBN, then search
  old and new values without rescanning; only new values match and title order
  follows the edited title
- Manual keyboard pass: focus visibility, Tab order, Escape cancellation, Enter
  for a single line, newline versus modified-Enter in textareas, and focus
  restoration after a successful swap; editing the title also updates the
  browser-tab title
- Manual no-JavaScript pass: enter edit mode, save valid values, trigger a title
  validation error, cancel, and confirm every response remains a complete usable
  page
- Manual responsive pass above and below the existing detail breakpoint: no
  overflow, controls wrap cleanly, and the 34px tap-target floor holds
