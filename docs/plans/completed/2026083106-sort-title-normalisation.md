# Step: sort_title normalisation

## Context

DESIGN.md's data model lists `sort_title` as a field distinct from `title`.
It currently isn't one: `scanner.go`'s `createBook` sets
`SortTitle: meta.Title` verbatim, and `storage.ListBooks` does
`ORDER BY sort_title` under SQLite's default BINARY collation. Verified
against the real query, four books come back in this order:

```
Anna Karenina
The Hobbit
Zebra Book
apple book
```

Two separate bugs in one line of output. Every lowercase-initial title
sorts after every uppercase-initial one, because BINARY compares bytes and
`'Z'` (0x5A) < `'a'` (0x61). And "The Hobbit" files under T, which is the
exact thing a distinct `sort_title` column exists to prevent.

This is the only ordering the one shipped page has, so it is the most
visible correctness bug in the app today.

## Scope

In scope: deriving a real `sort_title` on write, and putting the collation
on the column so ordering stays correct regardless of what a writer stores.

Out of scope: author sort names (`Tolkien, J.R.R.` vs `J.R.R. Tolkien`) —
DESIGN.md doesn't call for a sort form on authors, and browse-by-author
doesn't exist yet; when it does, it gets its own step. Also out of scope:
locale-aware collation. SQLite's `NOCASE` is ASCII-only, so a Cyrillic or
accented title still sorts by code point. That is acceptable for a personal
library and fixing it properly means either an ICU build (impossible under
the pure-Go driver) or a hand-rolled collation registered on every
connection — a real step of its own, if it ever matters.

## Deriving sort_title

New unexported helper in `internal/scanner`, next to `filenameTitle`:

```go
// sortTitle derives the form a title files under: leading articles moved
// out of the way and case folded, so "The Hobbit" sorts under H and
// "apple book" sorts with the A's.
func sortTitle(title string) string
```

Rules, in order:

1. Trim surrounding whitespace.
2. Strip one leading article, case-insensitively, when followed by a space:
   `The `, `A `, `An `. English only — the library is mostly English and
   guessing articles across languages ("Der", "La", "Il", "El") mis-files
   any title that legitimately starts with those words. If `language` ever
   proves reliable enough to key off, that is a later refinement.
3. Lower-case the result (`strings.ToLower`).
4. If *stripping the article* emptied the result, fall back to the
   lower-cased original — a title of "A" must file under `a`, not sort to
   the very top of the library on an empty key. (A title that was empty or
   whitespace to begin with still yields `""`; that is the honest answer,
   and it is unreachable in practice because `extractMetadata` always falls
   back to a filename-derived title.)

Do **not** strip punctuation or leading digits. `'Salem's Lot` and
`1984` file under `'` and `1` respectively, which is where a reader looking
alphabetically would expect a "before A" bucket, and inventing a rule here
means inventing an inverse for the UI later.

`createBook` sets `SortTitle: sortTitle(meta.Title)` — note it derives from
the *resolved* title, i.e. after the filename fallback, so a book with no
embedded title still sorts sensibly.

## Collation

Deriving a lower-cased `sort_title` makes the BINARY comparison correct on
its own for ASCII, but that leaves the column's correctness depending on
every writer remembering to fold case. Belt and braces: also declare the
column `COLLATE NOCASE`, so the ordering is right even if a future writer
(inline metadata editing, provider enrichment) sets `sort_title` without
folding.

**The collation goes on the column, in the schema.** Verified: a
column-level `COLLATE NOCASE` governs a bare `ORDER BY sort_title`, so
`storage.ListBooks` needs no query change and no future title-ordered query
(search results, `/api/v1`) has to remember a clause.

> **Revised decision.** An earlier draft of this plan put the collation at
> the query site instead, on the grounds that changing a column's collation
> needs a `books` table rebuild — and `books` is a *parent* table, so unlike
> the child-table rebuilds in `2026083105-storage-schema-hardening` that
> rebuild interacts with foreign keys the migration runner cannot disable
> inside its per-file transaction.
>
> That reasoning assumed a database worth preserving. There isn't one: the
> service has never been deployed and no instance exists. So there is no
> rebuild to do and no migration to add — **edit
> `2026083001_create_books_table.sql` in place** and declare the column
> `sort_title TEXT NOT NULL COLLATE NOCASE`. Every database is created by
> running the migrations from empty, so every database gets it.
>
> This works exactly once. The moment a real database exists, editing an
> applied migration stops being an option — the runner records it in
> `schema_migrations` and never re-runs it — and the rebuild reasoning above
> becomes correct again. Any later collation change is a rebuild.

One consequence worth knowing: `NOCASE` changes **equality** as well as
ordering, so `WHERE sort_title = 'ZEBRA BOOK'` matches `zebra book`.
Nothing does equality on `sort_title` today, and for a sort key that
behaviour is desirable rather than surprising — but it also means a
`UNIQUE` index on this column would treat case variants as collisions, so
don't add one casually.

## Backfill

**None needed.** Databases are created by running the migrations from
empty and no deployed instance exists, so no rows carry an un-normalised
`sort_title`.

One caveat for anyone holding a local `data/library.db` from an earlier
run: the edited `CREATE TABLE` will **not** re-apply to it — the runner
recorded that migration as done and never re-runs it — so an old file
silently keeps the BINARY collation and the un-derived sort titles. Delete
it and let the next start rebuild from scratch.

## Tests

`internal/scanner`, table-driven over `sortTitle`:

| input | expected |
|---|---|
| `The Hobbit` | `hobbit` |
| `A Wizard of Earthsea` | `wizard of earthsea` |
| `An Ideal Husband` | `ideal husband` |
| `THE GREAT GATSBY` | `great gatsby` |
| `Theory of Everything` | `theory of everything` (not `ory of everything`) |
| `A` | `a` (article-only, falls back) |
| `The ` | `the` (falls back rather than empty) |
| `Android Dreams` | `android dreams` |
| `apple book` | `apple book` |
| `Zebra Book` | `zebra book` |
| `  Spaced Out  ` | `spaced out` |
| `1984` | `1984` |
| `'Salem's Lot` | `'salem's lot` |
| `The A Team` | `a team` (one article, not two) |
| `   ` | `""` |

`Theory of Everything` and `Android Dreams` are the cases that catch a naive
`strings.HasPrefix(t, "The")` / `"An"` without the trailing space — the two
most likely ways to get this wrong.

`internal/storage`: `TestListBooksOrdersCaseInsensitively` inserts
`zebra book`, `anna karenina` and `Apple Book` and asserts they come back in
that case-insensitive order. Under the default BINARY collation they come
back as `[Apple Book, anna karenina, zebra book]`, so the test fails without
the schema change — confirmed by reverting it. The pre-existing
`TestListBooks` passes either way, which is why it never caught this.

## CLAUDE.md

Note under the schema paragraph that `sort_title` is derived (leading
article stripped, case folded) rather than a copy of `title`, and that the
column is declared `COLLATE NOCASE`, so any `ORDER BY sort_title` is
case-insensitive without a per-query clause.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: `go run ./cmd/server` against a library containing a lowercase
  filename-titled book and something starting with "The", and confirm the
  grid is alphabetical by the word a reader would look under.
