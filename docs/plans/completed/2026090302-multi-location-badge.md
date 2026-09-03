# Step: Multi-location badge (byte-identical duplicate detection in the UI)

## Context

DESIGN.md's "Duplicate detection" section has read **Partial** since the
data model landed:

> **Status: Partial.** The data model does this correctly — one `books`
> row, one `book_files` row per location. The UI does not flag it; the
> library grid has no multi-location indicator yet.
>
> The flag was deliberately blocked on scanner correctness: stale rows and
> absolute paths would have made the location count lie. Both blockers are
> now fixed — missing files are reconciled and paths are stored relative —
> so the badge is buildable whenever a UI step picks it up.

This is that step. It is the smallest remaining in-scope item in
DESIGN.md — the others are provider enrichment (large, and the thing
`field_sources` is waiting for), the send history view, and recipient
management.

Three things make it small:

- **No schema change and no migration.** `book_files` already holds one
  row per location, `book_files_book_id_idx` already indexes `book_id`,
  and the aggregate this needs is one `GROUP BY` over that index.
- **The design is drawn, not open.** Plate 01 of
  `ui-handoff/mockups/Bookshelf Mockups.dc.html` (on the `init` branch)
  draws the marker, and `ui-handoff/SCREENS.md` §01 names exactly what is
  missing:

  > **Multi-location indicator:** the mockup shows a dotted-underline "2
  > paths" marker beside the badge when one book is on disk in more than
  > one place. `BookSummary` carries no location count yet, so this is
  > drawn but not built — add the count to the summary before wiring it.

- **The detail page already does the hard half.** `GET /books/{id}`
  enumerates a book's paths in a `<details>` with the same
  dotted-underline accent affordance. This step adds the *signal* on the
  grid; the *answer* — which paths — already exists one click away. That
  division is the whole feature: the grid says "look here", the detail
  page says "here is what and where".

The prerequisites DESIGN.md named are genuinely done, and it is worth
being precise about why they mattered, because they are what keeps the
count from lying:

- **Missing-file reconciliation** (`2026083110`). Without it, a deleted
  copy left its `book_files` row behind forever and every such book would
  read "2 paths" permanently. Now a vanished path is marked, then pruned
  after `MISSING_GRACE`.
- **Relative paths** (`2026083002` and the scanner work after it). With
  absolute paths, remounting the library at a different prefix
  re-registered every file under a new path — one book, N locations, all
  of them the same file. The badge would have flagged the entire library.

## Scope

In scope: a location count on `service.BookSummary`, fed by one aggregate
query, rendered as the marker plate 01 draws — on the library grid and,
because they share the `book-grid` fragment, on search results too.

Out of scope, with reasons:

- **Near-duplicate detection** (same book, different compression or
  edition). DESIGN.md defers this explicitly and says why: it needs
  normalised title/author/ISBN matching and must surface as a
  *suggestion*, since false positives (omnibus editions, translations)
  are annoying to undo. This step is byte-identical only, which is
  exactly what a content hash already gives for free.
- **Any de-duplication action** — no "delete the other copy" button, no
  merge. The scanner owns the library directory's contents today, and
  DESIGN.md's rule for it is that writes only ever create new paths.
  Deleting a book file from a web request is a different feature with a
  different risk profile, and it needs a confirmation design that does
  not exist. Flagging is useful on its own: the user fixes it in their
  file manager.
- **A "duplicates only" filter or view.** Plate 01 draws a marker on the
  card, not a filter in the masthead. A filter is a reasonable follow-up
  once the marker shows how common duplicates actually are in a real
  library — which nobody knows yet, and which the marker is what
  measures. Building the filter first would be guessing at a workload.
- **The paging line** plate 01 also draws ("Loading next 48 of 1,284").
  Unrelated to this feature and unbuilt for its own reasons.

## The one real decision: do missing locations count?

A `book_files` row that has gone missing is still a row until
`MISSING_GRACE` (default 24h) elapses. So a book with one live copy and
one recently-deleted copy can be counted either way, and the two answers
differ for up to a day.

**Decision: count every location row, including missing ones.** The badge
is a link to the detail page, and the detail page enumerates *all* rows,
annotating the missing ones. If the grid counted only live locations, a
card reading "1 path" — or no marker at all — would open onto a page
listing two, which is the same number rendered two ways on two screens
that link directly to each other. That is the defect CLAUDE.md's
"one screen must not show the same number two ways" rule exists to
prevent, and here the two screens are one click apart.

The cost is bounded and self-healing: for at most `MISSING_GRACE` a book
whose duplicate was just deleted still shows the marker, and clicking
through says plainly which path is missing. Then the row is pruned and
the marker disappears on its own.

The alternative — counting only `missing_since IS NULL` — was rejected
for the reason above, and is recorded here rather than in a comment
because the query is the kind of thing a future reader will otherwise
"fix" by adding the filter.

**Implementation consequence:** the count must be `count(*)` with no
`WHERE` on `missing_since`. Say so in the doc comment, with the reason,
or it will not survive contact with a reader who assumes it is an
oversight.

## Storage

One new method in `internal/storage/books.go`, beside `ListBookAuthors`,
whose shape it deliberately copies:

```go
// CountFilesByBook returns how many file locations each book has, keyed
// by book id. Books with no locations are absent from the map rather
// than present with a zero, which callers should treat as 1-or-fewer:
// the last location's deletion prunes the book in the same transaction,
// so a book with zero locations is a race, not a state.
//
// Missing locations are counted. A row whose path has vanished stays in
// book_files until it has been missing past MISSING_GRACE, and the
// detail page lists it — annotated — for that whole window. Filtering
// them out here would make the grid and the detail page disagree about
// the same book's location count while linking to each other.
func (db *DB) CountFilesByBook(ctx context.Context) (map[int64]int, error)
```

```sql
SELECT book_id, count(*) FROM book_files GROUP BY book_id
```

`book_files_book_id_idx` covers the grouping, so this is an index scan,
not a table scan.

Why a whole-library map rather than a per-book count: it mirrors
`ListBookAuthors` exactly, and for the same reason — the grid renders
every book, so one aggregate beats N round trips. `ListAuthorsForBook`
exists as the single-book counterpart for the detail page; this step
needs no counterpart, because the detail page already loads the rows
themselves via `ListBookFiles` and can take `len` of them.

## Service

`BookSummary` gains one field:

```go
type BookSummary struct {
	ID        int64
	Title     string
	Authors   []string
	Format    string
	CoverPath string
	// Locations is how many places on disk this book's content sits at.
	// 1 for almost every book; more than 1 is what the grid flags.
	Locations int
}
```

`summarize` fetches the map alongside the author map and fills it in.
Both are one query each, both are whole-library, and both are already
paid for by every grid render:

```go
func (s *Service) summarize(ctx context.Context, books []storage.Book) ([]BookSummary, error) {
	authorsByBook, err := s.db.ListBookAuthors(ctx)
	if err != nil {
		return nil, err
	}
	locationsByBook, err := s.db.CountFilesByBook(ctx)
	if err != nil {
		return nil, err
	}
	...
}
```

Because `summarize` is shared, `ListBooks` and `SearchBooks` both get the
count with no further change — which is the point of that helper and the
reason search results show the marker for free.

`BookDetail` needs nothing: it already carries `Locations
[]FileLocation`, and the detail page already renders `{{len
.Locations}}`.

## Web transport

`bookCard` gains one field, and it is a **string composed in the
handler**, not an integer the template formats:

```go
type bookCard struct {
	ID         int64
	Title      string
	AuthorLine string
	Format     string
	CoverURL   string
	// PathsLabel is the multi-location marker's text ("2 paths"), or ""
	// for the ordinary single-location book, which is what the template
	// branches on. Composed here rather than in the template, the same
	// as SearchSummary: the template holds no formatting logic, and a
	// count that never reaches the marker cannot be rendered as "1
	// paths" by accident.
	PathsLabel string
}
```

Set it where the summaries are mapped onto cards:

```go
if b.Locations > 1 {
	card.PathsLabel = strconv.Itoa(b.Locations) + " paths"
}
```

The plural is unconditional because the label only exists above 1. Do
*not* reuse the detail page's `{{if eq (len .Locations) 1}}path{{else}}`
branch here — that template renders the count for every book including
the singular case, so it needs the branch; this one does not, and
carrying a dead singular branch invites someone to "fix" the threshold
later and get "1 paths".

Markup, into the existing `.card__meta` line in `book-grid`
(`templates/partials.html`), matching plate 01's order — format badge
first, marker second:

```html
<p class="card__meta">
  <span class="badge">{{.Format}}</span>
  {{if .PathsLabel}}<span class="card__paths">{{.PathsLabel}}</span>{{end}}
</p>
```

Note what is *not* here: the mockup puts `title="Same file found in more
than one location"` on the span. Keep that attribute — it is the only
explanation of what "2 paths" means — but do not rely on it, since
`title` does not survive touch input or keyboard focus. The card is
already wrapped in a single `<a>`, so the marker's text joins that link's
accessible name: "The Left Hand of Darkness, Ursula K. Le Guin, epub, 2
paths". That reads acceptably, and adding a nested interactive element
inside the card link to improve it would break the card.

## CSS

One rule in `internal/web/static/css/`, next to `.badge`, taking its
values from plate 01 and reusing the accent treatment `.locations
summary` already established on the detail page — the two markers mean
the same thing and should look like it:

```css
.card__paths {
  font-family: var(--mono);
  font-size: 9.5px;
  letter-spacing: 0.08em;
  color: var(--accent);
  border-bottom: 1px dotted var(--accent);
}
```

Deliberately *not* a `.badge` modifier: `.badge` is a bordered box with
padding, and the mockup draws the marker as bare accent text with a
dotted underline. They sit side by side in the same row (`.card__meta`
already sets `display: flex; align-items: center; gap: 7px`) and the
contrast between boxed and underlined is what distinguishes "what format
this is" from "there is something to look at here".

Check both themes. `--accent` is themed, and the dotted underline against
`--bg` is the one place a token swap could go muddy.

## Tests

**Storage** (`internal/storage/books_test.go`):

- A book with one location, a book with two, and a book with three, in
  one library: the map has the right count for each.
- A book whose only location is marked missing still counts 1 — the
  decision above, pinned so the `WHERE` clause cannot be added without a
  test going red.
- Deleting a book removes it from the map entirely (the `ON DELETE
  CASCADE` path), rather than leaving a zero.
- An empty library returns an empty, non-nil map.

**Service** (`internal/service/service_test.go`):

- `ListBooks` reports `Locations` per book, and a book with one location
  reports 1 rather than 0 — the map-absence contract translated at the
  boundary.
- `SearchBooks` reports it too, since it shares `summarize`. This is the
  regression that matters: a future refactor that splits the two paths
  would otherwise silently drop the marker from search results only.

**Web** (`internal/web/web_test.go`):

- A book with two locations renders `2 paths` on the grid; a book with
  one renders no marker at all (assert the class is absent, not just the
  text — an empty span is still a rendering bug).
- The marker survives into the `book-grid` fragment for an `HX-Request`
  search, not just the full page.
- Three locations render "3 paths", so the count is the real number and
  not a hardcoded 2.

**Mutation checks** — the discipline this repo uses, and there are three
worth running because each guards a decision rather than a line:

1. Change `count(*)` to a filtered count excluding missing rows → the
   storage missing-row test must fail.
2. Change the `> 1` threshold to `> 0` → a web test must fail on a
   single-location book rendering a marker.
3. Drop the `PathsLabel` assignment entirely → the grid tests must fail.
   (This is the one that would otherwise pass on the strength of the
   storage and service tests alone, which is how the last step's kernel
   watch leak got through: assert the property, not the plumbing.)

## CLAUDE.md

The `internal/storage` bullet gains `CountFilesByBook` next to
`ListBookAuthors`, with the missing-rows-are-counted rule and its reason
— that rule is the one a reader will otherwise undo.

The `internal/service` bullet gains `BookSummary.Locations`.

The `internal/web` bullet gains the marker: composed in the handler like
`SearchSummary`, rendered only above 1, and sharing the detail page's
accent affordance because the two mean the same thing.

## DESIGN.md (on `init`)

Once this ships, the Duplicate detection section moves from **Partial**
to **Built** for the byte-identical half, keeping near-duplicate
detection listed as deferred. The status table row becomes:

```
| Duplicate detection (byte-identical) | Built — one entry per content hash, multiple locations, flagged on the grid |
```

And the "unbuilt but not deferred" paragraph in the out-of-scope section
drops the multi-location badge from its list, leaving provider enrichment
and the send history view.

## Verification

- `gofmt -l .`, `go vet ./...`, `go build ./...` clean.
- `go test -race -count=1 ./...`.
- The three mutations above, each confirmed to turn its own test red.
- Manual, against a real library: put the same EPUB at two paths, let a
  sweep run, confirm the grid marks it "2 paths" and the detail page
  lists both. Then delete one copy and confirm the marker persists (the
  grace window) with the detail page annotating the missing path — the
  decision above, seen rather than assumed.
- Both themes, and a narrow viewport: the marker shares a flex row with
  the format badge, so it is the card's most likely wrap point.
