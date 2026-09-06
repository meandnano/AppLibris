# Step: Library grid paging

## Position in the sequence

**Fourth of four, and the only one independent of the other three.** It
shares no code with enrichment and could be built at any point.

It is placed last because it optimises something that currently works,
while 04–06 complete the last feature DESIGN.md designs and never built.
There is one condition that should move it earlier: **if the grid becomes
visibly slow to render or scroll on the real library, do this first.**
That is a measurement, not a guess — and step 05 makes it more likely,
because filling in covers replaces cheap dashed boxes with hundreds of
image requests.

## Context

This is the last item in `ui-handoff/SCREENS.md` that is drawn but not
built. §01:

> **Paging:** the mockup shows a mono "Loading next 48 of 1,284" line
> under the grid. Intended as an htmx `revealed` trigger appending the
> next page of cards.

Every other "not built yet" note in that file has since been built — FTS
search, the book detail page, inline editing, the send control, send
history, and the multi-location badge.

The gap is real and not only cosmetic. `storage.ListBooks` is
`SELECT … FROM books ORDER BY sort_title` with no `LIMIT`, and the
handler maps every row into a `bookCard` and renders every card. At the
handoff's own reference size of 1,284 books that is 1,284 rows scanned,
1,284 cards of HTML, and 1,284 lazily-loaded cover requests in one
document. It works — `loading="lazy"` keeps the images from all being
fetched at once — but nothing about it is bounded, and the search path
has the same shape.

Note what is *not* wrong here: `CountBooks` is already a separate `count(*)`
independent of any filter, so the masthead's total and the search results
line ("4 of 1,284") do not depend on the grid having loaded everything.
That separation was built for the search summary and is exactly what
paging needs.

## Scope

In scope: a bounded first page on `GET /{$}`, an append-on-reveal trigger
for subsequent pages, and the same treatment for search results.

Out of scope, with reasons:

- **Infinite scroll with virtualisation.** Cards are appended and stay in
  the DOM. Removing them on scroll-out means restoring scroll position on
  Back, which is a class of bug this codebase has no reason to take on
  for a library of a few thousand books.
- **Numbered pages or a "load more" button.** The handoff draws a
  revealed trigger; a button is a second interaction to specify and
  maintain, and the no-JS path (below) already provides the
  non-JavaScript equivalent.
- **Sort or filter controls.** The order is `sort_title`, always. Adding
  a sort dimension makes the cursor design harder and nobody has asked.

## Decision 1: keyset pagination, not OFFSET

`LIMIT ? OFFSET ?` is the obvious implementation and the wrong one here,
for a reason specific to this application: **the library changes
underneath the reader.** The scanner runs every 15 minutes and on every
filesystem event, and it inserts books in `sort_title` order-agnostic
fashion. With `OFFSET`, a book inserted above the current position while
someone is scrolling shifts every subsequent row down by one, so the next
page repeats a card and skips none — or, on a delete, skips one silently.

Keyset pagination has no such window:

```sql
SELECT <cols> FROM books
WHERE (sort_title, id) > (?, ?)
ORDER BY sort_title, id
LIMIT ?
```

The cursor is the last row's `(sort_title, id)`. `id` breaks the tie
because `sort_title` is emphatically not unique — it is a normalised
title with the leading article stripped, so two editions of the same book
collide by construction — and a non-unique cursor column either loops or
skips.

**`sort_title` is `COLLATE NOCASE`**, which the comparison must respect
or the cursor will disagree with the `ORDER BY` and pages will overlap.
This is the single most likely bug in the step; assert it with a fixture
whose titles differ only in case.

An index on `(sort_title, id)` makes this a range scan. Migration
`2026090309_create_books_sort_title_id_index.sql`.

## Decision 2: the search path pages too, through the same code

`SearchBooks` joins `books_fts` and orders by `sort_title` — not by
relevance, deliberately, so that the same cursor works unchanged. That
choice was made for a different reason (a stable order while typing) and
happens to be exactly what makes paging uniform here.

Do not build a second paging mechanism for search. One cursor type, one
page size, both list methods taking it. A search that matches 900 books
has the same problem as an unfiltered library and should not be the case
that was forgotten.

## Decision 3: the no-JS path is a plain link

The revealed trigger is an enhancement. Without JavaScript the same
"next" element is an ordinary `<a href="/?after=…">` that loads a full
page starting from that cursor — the same single-markup-path rule the
rest of the UI follows (a read affordance is an `<a>` with both an `href`
and an `hx-get`; here it is an `<a>` with both an `href` and an
`hx-get` plus `hx-trigger="revealed"`).

This matters more than usual: with JavaScript off, an unpaged grid is the
*only* thing that currently works at all, so a paging implementation that
forgets the fallback makes the no-JS experience strictly worse than
before the change.

## Decision 4: page size

`pageSize = 48`, matching the handoff's own "Loading next 48 of 1,284"
line. Not a coincidence to be improved on — a number in a mockup is a
design decision about how much scrolling one reveal buys, and there is no
evidence to overrule it with.

Named constant, in the transport rather than storage: it is a
presentation decision, and the storage method should take whatever limit
it is given.

## Storage

`ListBooks` and `SearchBooks` gain a cursor and a limit. Rather than
adding two parameters to two methods and their existing callers, add a
small type:

```go
// BookPage is a keyset cursor into the sort_title, id ordering. The zero
// value is the first page. AfterTitle is compared with the same NOCASE
// collation the column carries, so the comparison and the ORDER BY agree.
type BookPage struct {
	AfterTitle string
	AfterID    int64
	Limit      int
}
```

A `Limit` of zero means unbounded, which keeps the scanner's and tests'
existing whole-library calls working unchanged — and is worth a comment
saying that the web transport never passes zero, so the unbounded path
exists for callers that genuinely want every row.

## Service and transport

`ListBooks`/`SearchBooks` take the page and return the summaries plus
whether more exist. Getting "are there more?" right without a second
count: **ask for `Limit+1` rows and trim** — the same trick
`SendHistory` already uses to decide `truncated`. Reusing a technique the
codebase already has beats introducing a second one.

The handler composes the next-page URL and the mono line
("Loading next 48 of 1,284", the count from `CountBooks`) — handler-side
text composition, the `searchSummary` convention. On the last page the
element renders as nothing rather than as a line saying zero.

The `book-grid` fragment already exists for search. Appending needs a
*different* swap than replacing, so the reveal target is the trigger
element itself with `hx-swap="outerHTML"`, and its response is the next
batch of `<li>`s followed by a fresh trigger — the standard htmx
click-to-load shape, and the reason the trigger must live inside the same
list container as the cards.

**Watch the interaction with search.** A keystroke replaces the whole
grid (`hx-target="#book-grid"`, `outerHTML`), which must also reset
paging — a stale trigger left behind would append page two of the
*previous* query. Since the search response rebuilds the whole grid
including its trigger, this is correct by construction; assert it, since
it is invisible until it breaks.

## Tests

**Storage:**

- A page returns exactly `Limit` rows and the next page continues from
  the cursor with no overlap and no gap.
- Titles differing only in case page correctly — the NOCASE collation
  regression.
- Two books with identical `sort_title` both appear exactly once across
  pages — the tie-break regression.
- A book inserted above the cursor between pages does not cause a repeat
  or a skip (the case that rules out `OFFSET`).
- `Limit: 0` returns everything, so existing callers are unaffected.

**Service/web:**

- Page one renders `pageSize` cards and a trigger; the last page renders
  no trigger.
- The trigger element carries both an `href` and htmx attributes.
- A search result set larger than a page pages too, and the next-page URL
  carries the query.
- A new search resets paging rather than appending to the old results.

**Mutation checks:**

1. Drop `id` from the cursor comparison → the duplicate-`sort_title` test
   fails.
2. Use a case-sensitive comparison → the NOCASE test fails.
3. Replace keyset with `OFFSET` → the insert-between-pages test fails.
4. Render the trigger on the last page → its test fails.

## CLAUDE.md

The `internal/storage` bullet gains `BookPage` and the two reasons the
cursor is keyset and includes `id` — the concurrent-insert window and the
non-unique `sort_title`. The `internal/web` bullet gains the reveal
trigger, the `Limit+1` technique shared with `SendHistory`, and the note
that a new search rebuilds the trigger and thereby resets paging.

## DESIGN.md (on `init`)

The Web UI section notes that the grid pages rather than rendering the
whole library, and that the order is `sort_title` for both the unfiltered
and search paths precisely so one cursor serves both.

## Verification

- `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test -race ./...`.
- The four mutations above.
- Manual, with a library large enough to page — generate a few hundred
  files if the real one is smaller, since a paging bug is invisible at
  one page. Scroll through every page and confirm no book appears twice
  and none is missing, by count.
- Manual with JavaScript disabled: the next-page link navigates and the
  grid still works.
- Manual: type a search that matches more than a page, scroll to append,
  then change the query and confirm the results replace rather than
  accumulate.
