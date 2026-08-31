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

In scope: deriving a real `sort_title` on write, fixing the collation on
read, and backfilling existing rows.

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
4. If the result is empty (a title that is only an article, or only
   whitespace), fall back to the lower-cased original — never store an
   empty `sort_title`, or the book sorts to the very top of the library.

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

The column can't be altered in place, so this is a `books` table rebuild —
four statements, four migration files, per the one-statement-per-file
convention. `books` is a *parent* table, so unlike the child-table rebuilds
in `2026083105-storage-schema-hardening` this one does interact with foreign
keys: `book_files` and `book_authors` reference it. Rebuilding under
`foreign_keys=ON` inside a transaction will either rewrite the children's
references onto the temp name or trip a constraint, and the pragma can't be
toggled inside the transaction the migration runner opens.

**Therefore prefer the cheap option:** skip the rebuild and put the
collation at the query site instead —
`ORDER BY sort_title COLLATE NOCASE` in `storage.ListBooks`. Same ordering,
no rebuild, no runner change. The downside is that it has to be repeated on
every future query that orders by title (search results, the API), so add a
comment on the column in CLAUDE.md's schema notes saying the collation lives
in the queries.

If a later step needs a `books` rebuild anyway (adding a column that can't
be `ALTER TABLE ADD`, say), move the collation onto the column then and
drop the per-query clause.

## Backfill

`2026083114_backfill_sort_title.sql`:

```sql
UPDATE books SET sort_title = lower(
    CASE
        WHEN lower(title) LIKE 'the %' THEN substr(title, 5)
        WHEN lower(title) LIKE 'an %'  THEN substr(title, 4)
        WHEN lower(title) LIKE 'a %'   THEN substr(title, 3)
        ELSE title
    END
);
```

`lower()` in SQLite is ASCII-only, matching `NOCASE` — and deliberately
*not* matching Go's `strings.ToLower`, which folds Unicode. A title with
non-ASCII capitals will therefore be backfilled slightly differently from
how the scanner would write it. That divergence is invisible at the
ordering level (both sort by code point beyond ASCII either way) and
self-corrects the next time the book's metadata is rewritten; not worth a
Go-side migration runner to avoid. Note it in the migration comment.

The `CASE` ordering matters: test `'an %'` before `'a %'`, or "An Ideal
Husband" loses only `A` and files under "n Ideal Husband".

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
| `apple book` | `apple book` |
| `  Spaced  ` | `spaced` |

The `Theory` case is the one that catches a naive `strings.HasPrefix(t,
"The")` without the trailing space.

`internal/storage`: insert the four titles from the Context section above
and assert `ListBooks` returns them as `anna karenina`, `apple book`,
`hobbit`, `zebra book` — i.e. assert the *fixed* order explicitly, since
the current test (`TestListBooks`) passes today with the broken one.

## CLAUDE.md

Note under the schema paragraph that `sort_title` is derived (leading
article stripped, case folded) rather than a copy of `title`, and that
title ordering applies `COLLATE NOCASE` at the query.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: `go run ./cmd/server` against a library containing a lowercase
  filename-titled book and something starting with "The", and confirm the
  grid is alphabetical by the word a reader would look under.
