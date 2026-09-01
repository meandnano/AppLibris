# Step: Full-text search (as you type)

## Context

The FTS5 index is the stated reason this project runs on SQLite at all —
DESIGN.md's storage section says the engine "is currently carrying its
justification on credit," and the status table lists the index as the one
unbuilt piece of an otherwise Built storage layer. On the UI side,
search-as-you-type is the first of exactly three interactions DESIGN.md
allocates htmx to, and UI.md's mockup brief pins down its shape: a search
box at the top that **filters the grid live as you type — no separate
results page** — with four distinct states (idle, actively filtering,
results, no results).

Of the three htmx interactions, search is the only one whose backing
feature has no unbuilt prerequisite: the send control needs the job model
and inline editing needs field provenance, but search needs only the index
this step creates. So this step also vendors htmx and establishes the
partial-rendering pattern the other two interactions will reuse.

**Verified on current master before writing this plan** (a throwaway
program against the module's own `modernc.org/sqlite v1.57.0`):

- `CREATE VIRTUAL TABLE … USING fts5(…)` works — FTS5 is compiled into
  the pure-Go driver, so `CGO_ENABLED=0` holds.
- `tokenize='unicode61 remove_diacritics 2'` works, and a query for
  `tokarczúk` matches a stored `Tokarczuk` — diacritic-insensitivity in
  both directions, which a library with FB2/Russian/European content needs.
- Quoted-prefix syntax `"tok"*` works, which is the query shape the
  sanitizer below emits.

None of this needs a build-tag, a new dependency, or a driver change.

## Scope

In scope:

- A `books_fts` FTS5 index over title, authors, description and ISBN,
  kept in sync inside the same write transactions that change those
  fields.
- `storage.SearchBooks` and a `service.SearchBooks` over it.
- Vendoring htmx as an embedded static asset (no build step, per
  DESIGN.md's constraints).
- The search box on the library page, live-filtering the grid via an
  htmx partial swap, with all four UI.md states, degrading to a plain
  form GET when JS is off.

Out of scope:

- Book detail, inline editing, the send control — each blocked on other
  prerequisites, per DESIGN.md.
- Snippets/highlighting and relevance ranking (see "Ordering" below for
  why ranking is deliberately rejected, not deferred).
- The multi-location badge (blocked on nothing anymore, but a separate
  concern).
- Re-parsing existing books for richer metadata — search indexes what the
  columns already hold.

## Schema: two migrations

No backfill migration: the app has never been deployed, so there is no
database whose contents predate the index. Any existing dev database is
deleted and rescanned (the same answer `2026090103` gave for re-parsing),
and after that every row ever inserted goes through `CreateBook*`, which
syncs the index in the same transaction — the FTS table can never be
behind by construction. Nothing here touches the existing migration
files either; the step is purely additive.

One statement per file, per the house migration rules
(`YYYYMMDDNN_description.sql`, applied in filename order, each in its own
transaction — a trigger body is one statement):

1. `create_books_fts_table.sql` —

   ```sql
   CREATE VIRTUAL TABLE books_fts USING fts5(
       title, authors, description, isbn,
       tokenize='unicode61 remove_diacritics 2'
   );
   ```

   A plain FTS5 table with `rowid` = `books.id`, **not** an
   external-content table. `content='books'` cannot work here because
   `authors` is not a column on `books` — it is assembled from a join —
   and a contentless table forbids the delete-by-rowid the sync helper
   needs. The duplicated text is a few hundred bytes per book; at this
   library's scale that is noise.

   No `prefix=` index. Prefix queries without one scan the term list,
   which is instant at a few thousand books; add `prefix='2 3'` only if
   as-you-type latency is ever actually felt. (Adding it later is a
   `DROP`/`CREATE`/backfill migration, not a redesign.)

2. `create_books_fts_delete_trigger.sql` —

   ```sql
   CREATE TRIGGER books_fts_after_delete AFTER DELETE ON books
   BEGIN
       DELETE FROM books_fts WHERE rowid = old.id;
   END;
   ```

   Deletion is the one sync direction that must be a trigger rather than
   a Go helper: books die on several paths (orphan pruning inside
   `ReassignFileAndPruneOrphan`, inside `CreateBookWithFile`, and inside
   `PruneMissingFiles`), and every future path dies for free too. Insert
   and update stay in Go — see the next section.

## Storage: sync in Go, not triggers

Insert/update sync does **not** use triggers. The searchable text spans
three tables (`books`, `book_authors`, `authors`); trigger-side sync would
need triggers on all three, each rebuilding a `group_concat` join in SQL,
and a future `authors.name` correction would need one more. The codebase's
established pattern — composable package-internal `…Tx` helpers inside a
single `DB.Write` — fits better and is testable.

Add to `internal/storage`:

- `syncBookFTSTx(ctx, tx, bookID)` (package-internal): `DELETE FROM
  books_fts WHERE rowid = ?` then `INSERT … SELECT b.id, b.title,
  coalesce(group_concat(a.name, ' '), ''), b.description, b.isbn` over
  the `books`/`book_authors`/`authors` left join, scoped to one book
  (`group_concat` order is unspecified, which is fine — tokens match
  bag-of-words). Recompute-from-scratch, so there is no drift between
  "what changed" bookkeeping and reality, and the same helper serves
  create and every future metadata edit.
- Call it inside `CreateBook` and `CreateBookWithFile`, after
  `createBookTx` **and** the author-linking helper have both run — the
  FTS row must see the authors.
- `SearchBooks(ctx, query string) ([]Book, error)`:

  ```sql
  SELECT <bookColumns qualified> FROM books
  JOIN books_fts ON books_fts.rowid = books.id
  WHERE books_fts MATCH ?
  ORDER BY books.sort_title
  ```

  taking the already-sanitized match expression (sanitization lives in
  one place — see below — so the storage method's contract is "a valid
  FTS5 query string").

Nothing else on `books` currently changes searchable fields —
`UpdateBookCoverPath`, the stat/missing-file methods and the file-row
methods touch none of the four indexed columns, so they do not call the
helper. When inline metadata editing arrives, its update method calls
`syncBookFTSTx` in its own transaction and stays consistent by
construction.

### Query sanitization

Raw user input is almost never valid FTS5 query syntax — a stray `"`,
`(`, `-` or a bare `AND` makes `MATCH` return an error, and letting users
reach FTS5 operators turns typos into empty result sets. One function
(unit-testable, no DB):

- Split the input on Unicode whitespace.
- Drop empty tokens; if none remain, the query is "blank" (see service
  layer).
- Escape each token's internal double quotes by doubling them, wrap the
  token in double quotes, and append `*`.
- Join with spaces: implicit AND.

So `har pot` becomes `"har"* "pot"*` — every word a prefix, order
irrelevant, which is the right semantics for as-you-type filtering
(matches "Harry Potter" while it's still being typed, in either word
order). Input consisting only of quotes or operators degrades to a blank
query, never an error.

### Ordering: sort_title, not bm25 — a decision, not an omission

UI.md is explicit that search **filters the grid** rather than opening a
results page. A filtered grid that suddenly reorders by relevance while
the user types would be jarring — the mental model is "my library,
narrowed," so results keep the library's own `sort_title` order (which
the `COLLATE NOCASE` column makes case-insensitive for free). bm25
ranking earns its place only if a results-page UI ever exists.

## Service layer

`Service.SearchBooks(ctx, query string) ([]BookSummary, error)`:

- Sanitize via the function above. A blank query returns `ListBooks` —
  the empty search box and the freshly-loaded page are the same state,
  and the handler needs no special-casing.
- Otherwise `storage.SearchBooks`, then attach authors exactly as
  `ListBooks` does. Reuse `ListBookAuthors` (all books) rather than
  adding a filtered variant — at this scale the whole-table map is
  cheaper than the extra query plumbing, and `ListBooks` already set the
  precedent. Extract the shared summary-assembly loop into an unexported
  helper so the two methods don't diverge.

## Web transport

### Vendoring htmx

One minified file (htmx 2.x, current stable at build time) into
`internal/web/static/js/htmx.min.js`, served by the existing embedded
static handler — which already suppresses listings and sets caching, per
`#23`. Pin the exact version in a comment at the top of the file (the
minified file already carries its own license header; keep it). This is
the "vendored, embedded, no build step" arrangement DESIGN.md's Web UI
section describes. Load it with `<script defer>` from the existing
`site-scripts` (or `document-head`) partial.

### Markup

- Extract the grid (the `<ul class="grid">`, the count, and the
  empty/no-results block) from `library.html` into a `book-grid` template
  in `partials.html`, rendered inside a stable container element the swap
  targets. The count lives **inside** the swapped region so it updates
  with the results and needs no out-of-band swap.
- Add the search box to the `site-header` partial, as a real form:

  ```html
  <form action="/" method="get" role="search">
    <input type="search" name="q" value="{{.Query}}" placeholder="Search…"
           hx-get="/" hx-trigger="input changed delay:300ms, search"
           hx-target="#book-grid" hx-swap="outerHTML" hx-push-url="true">
  </form>
  ```

  With JS off this is a plain form GET rendering the full filtered page —
  progressive enhancement for free, and the same handler serves both.
  `hx-push-url` keeps the URL shareable and the back button meaningful.
  `delay:300ms` is UI.md's "actively filtering" debounce; htmx's
  `htmx-request` class on the form during flight is the hook for whatever
  spinner treatment the "actively filtering" state gets in CSS.
- The four UI.md states map to: idle (empty box, full grid), filtering
  (`htmx-request` class), results (filtered grid + count), no results (a
  distinct block — **not** the "No books yet" empty-library block, which
  means something else; "no matches for `<query>`" with the query echoed,
  auto-escaped by `html/template`).

### Handler

Extend the existing `GET /{$}` handler rather than adding a route — the
full page and the fragment are the same resource:

- Read `q`. Call `svc.SearchBooks` (blank behaves as `ListBooks`).
- If the `HX-Request` header is present, render only the `book-grid`
  partial; otherwise the full page. `render` already writes to a buffer
  first (per the render-error contract from `2026090101`), so partial
  rendering slots in as a template-name parameter.
- Add `Vary: HX-Request` on this route — the same URL now serves two
  bodies, and without it a shared cache or the browser's back-forward
  cache can serve a bare fragment as a full page.
- The page view model grows a `Query` field so the input keeps its value
  on full-page renders.

A search that fails in storage is a 500 like any other list failure — no
special error state in the grid; FTS `MATCH` syntax errors cannot reach
storage because sanitization runs first.

## Tests

`internal/storage`:

- Create a book with authors, then `SearchBooks` finds it by a title
  token, an author token, a description token, and an ISBN — and by
  prefix of each (`"tok"*`).
- Diacritics both ways: stored `Tokarczuk` found by `tokarczúk`, stored
  `García` found by `garcia`.
- Multi-token queries AND together across columns (title word + author
  word matches; title word + wrong author word doesn't).
- Deleting a book through **each** pruning path
  (`ReassignFileAndPruneOrphan` orphaning, `PruneMissingFiles`) leaves no
  FTS row behind — assert via a direct `books_fts` count, since the
  trigger is the mechanism under test.
Sanitizer (pure unit tests, no DB): quotes, `AND`/`OR`/`NOT`, parens,
`-`, `*`, lone whitespace, empty string, embedded `"` doubling — none may
produce an expression FTS5 rejects (drive the produced expression through
an in-memory FTS table to prove it, rather than asserting exact strings).

`internal/service`: blank and whitespace-only queries return the full
list; a matching query returns summaries with authors attached.

`internal/web`:

- `GET /?q=…` without `HX-Request` renders the full page, input
  value echoed (and HTML-escaped — assert with a query containing
  `<script>`).
- Same URL with `HX-Request: true` renders only the fragment (no
  `<html>`), and the response carries `Vary: HX-Request`.
- No-results markup is the no-results block, not the empty-library block.

`internal/scanner` (end-to-end): scan a directory with a real EPUB
fixture, then `SearchBooks` by a word of its embedded author — proving
scanner-created books enter the index through the composed transaction.

## CLAUDE.md

- `internal/storage` bullet: the FTS5 index (`books_fts`), the sync
  contract (delete by trigger, insert/update via `syncBookFTSTx` inside
  the owning write transaction; no backfill — every row is created
  through the synced path), and
  `SearchBooks`.
- `internal/service`: `SearchBooks`, blank-query semantics.
- `internal/web`: htmx vendored; search-as-you-type on the library page;
  the full-page/fragment split on `GET /` and the `Vary: HX-Request`
  contract.
- Remove "search" from the still-missing list (book detail, inline
  editing and send-to-Kindle remain).

## Verification

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...` clean.
- Manual, against the real library: delete the dev database, start the
  server (fresh migrations plus a full rescan populate the index); type
  in the box — results narrow per keystroke after the debounce, a
  diacritic-less query finds accented authors, clearing the box restores
  the full grid, a nonsense query shows the no-results state, and the
  same `?q=` URL pasted into a fresh tab renders the full filtered page.
- With JS disabled: submitting the form still filters via full page
  reload.
