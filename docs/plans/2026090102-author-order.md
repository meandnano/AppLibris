# Step: Preserve author order

## Context

Promoted from `docs/backlog/2026083118-author-order.md`, which is deleted in
this change. That file recorded the problem as **latent** — real but not yet
visible, because `book_authors`' natural row order happened to match
insertion order. It also named the thing that would most likely change that:
the unique index on `authors.name` added by
`2026083105-storage-schema-hardening`.

Re-validating against current master: it did. The bug is live now.

**The query plan changed.** `ListBookAuthors` runs

```sql
SELECT book_authors.book_id, authors.name
FROM book_authors
JOIN authors ON authors.id = book_authors.author_id
ORDER BY book_authors.book_id
```

and SQLite now answers it as:

```
SCAN book_authors USING COVERING INDEX sqlite_autoindex_book_authors_1
SEARCH authors USING INTEGER PRIMARY KEY (rowid=?)
```

That covering index is the `(book_id, author_id)` primary key, so rows come
back in **`author_id` order** — not rowid order, and not the order the OPF
listed its `dc:creator` elements in.

**Why that is now wrong in the shipped app.** `author_id` ascends with
*first sight anywhere in the library*, not with position in this book's OPF.
So the two orders agree only while a book introduces all of its own authors.
They diverge as soon as an author is shared with an earlier book — which is
the entire reason `findOrCreateAuthor` exists. Reproduced on master:

| | |
|---|---|
| Book 1 credits | `Terry Pratchett` → `author_id` 1 |
| Book 2's OPF credits | `[Neil Gaiman, Terry Pratchett]` |
| `ListBookAuthors` returns | `[Terry Pratchett, Neil Gaiman]` |

*Good Omens* renders with the wrong lead author. And `web.authorLine`'s
three-or-more form — `"%s and %d others"` — names whoever happens to sort
first by `author_id`, so the card can attribute a book to the wrong person
outright rather than merely listing two names in the wrong order.

`internal/epub` already preserves OPF order into `Metadata.Authors`, and the
scanner passes that slice through intact. The order is discarded at the
storage boundary and nowhere else, so this is a one-table fix.

## Second defect, found while validating

A book whose OPF credits the same name twice is **dropped from the library
entirely**. `createBookTx` inserts one `book_authors` row per name, and the
primary key is `(book_id, author_id)`, so the second insert fails:

```
CreateBook(["Adam Author", "Adam Author"])
  -> constraint failed: UNIQUE constraint failed:
     book_authors.book_id, book_authors.author_id (1555)
  -> books in index afterwards: 0
```

The whole `CreateBook` transaction rolls back. The sweep logs a Warn and
carries on, so the book simply never appears, with nothing in the UI to say
why. Repeated `dc:creator` elements are not exotic — they show up when a
person is credited under two `opf:role`s (`aut` and `edt`), or listed once
as a display name and again in a file-as form that normalises to the same
string.

This is pre-existing and not caused by the ordering fix, but it lives in the
exact loop this step rewrites, and **deduplication is required to assign
positions sensibly anyway** — you cannot give one author two positions. So
it is in scope here rather than in a plan of its own.

## Premise: still no existing database

The service remains undeployed and there is no data to migrate, so the
schema is fixed by **editing the `CREATE TABLE` migration in place** rather
than adding an `ALTER TABLE`. The backlog file proposed
`ALTER TABLE ADD COLUMN` with a `DEFAULT 0`, which was the right shape when
it looked like a real database might exist by now; it doesn't, so the
cleaner form is available.

Same caveat as `2026083105-storage-schema-hardening` and
`2026083106-sort-title-normalisation`: this works only while no instance
exists. Delete any local `data/library.db` — the edited migration is already
recorded in `schema_migrations` and will not re-run, so a stale file keeps
the old table and this step will appear not to work.

## Schema

**`2026083004_create_book_authors_table.sql`** — edit in place:

```sql
CREATE TABLE book_authors (
    book_id     INTEGER NOT NULL REFERENCES books (id) ON DELETE CASCADE,
    author_id   INTEGER NOT NULL REFERENCES authors (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    PRIMARY KEY (book_id, author_id)
);
```

**`NOT NULL` with no default**, deliberately. The backlog sketch suggested
`DEFAULT 0`; don't. A default means a future writer that forgets to set
`position` — inline metadata editing, provider enrichment — silently gets
every author at position 0, which is ties, which is arbitrary order, which
is exactly the bug being fixed here returning quietly. With no default that
writer fails loudly at insert instead. There is one insert site today, so
the cost of strictness is nil.

**The primary key stays `(book_id, author_id)`.** A book crediting the same
author at two positions is a malformed file, not a case to support — and
after the dedup below it cannot happen. `position` is an ordering attribute,
not part of identity.

**`2026090101_create_book_authors_order_index.sql`** (new):

```sql
CREATE INDEX book_authors_order_idx ON book_authors (book_id, position, author_id);
```

Not optional padding — it prevents a regression this step would otherwise
introduce. Today's query is answered entirely from the covering PK index,
in order, with no sort. Adding `position` to the `ORDER BY` would drop it to
a table scan plus a sort, on the one query that runs on **every render of
the library page**. `(book_id, position, author_id)` carries all three
columns the query touches, so it stays covering and stays pre-sorted.

`2026090101` is the next free migration number (existing migrations run
`2026083001`–`2026083013` plus `2026083101`–`2026083103`; nothing is dated
`20260901` yet). Keep
`book_authors_author_id_idx` — it serves the cascade from `authors`, which
this index does not.

## `internal/storage` changes

**`createBookTx`** — dedupe, preserving first-seen order, and number what
survives:

```go
seen := make(map[string]bool, len(authorNames))
position := 0
for _, name := range authorNames {
    if seen[name] {
        continue // a name credited twice is one author, at its first position
    }
    seen[name] = true

    authorID, err := findOrCreateAuthor(ctx, tx, name)
    if err != nil {
        return 0, err
    }
    if _, err := tx.ExecContext(ctx,
        `INSERT INTO book_authors (book_id, author_id, position) VALUES (?, ?, ?)`,
        id, authorID, position); err != nil {
        return 0, err
    }
    position++
}
```

Dedupe on the **exact name string**, matching how `findOrCreateAuthor` looks
authors up (`WHERE name = ?`) and how `authors_name_idx` enforces
uniqueness — both BINARY, since neither declares `COLLATE NOCASE`. Using a
looser comparison here would let two names that `findOrCreateAuthor` treats
as different authors collapse into one link, which is a different bug.
`internal/epub`'s `trimAll` has already trimmed and dropped empties, so no
extra normalisation belongs here.

Count `position` on the surviving authors, not the loop index, or a
duplicate leaves a gap. Gaps would still sort correctly, but `position`
values that are contiguous from 0 are easier to reason about when the next
reader wonders whether one is missing.

**`ListBookAuthors`** — add the tiebreak:

```sql
ORDER BY book_authors.book_id, book_authors.position
```

No other call site touches `book_authors`; `internal/service` and
`internal/web` both consume the assembled slice and need no change.

## Tests

`internal/storage`:

- **The regression this step exists for.** Create book 1 crediting
  `Terry Pratchett`; create book 2 crediting `[Neil Gaiman, Terry Pratchett]`;
  assert `ListBookAuthors` returns them for book 2 in exactly that order.
  This fails on master today, returning `[Terry Pratchett, Neil Gaiman]` —
  run it before the fix to confirm it does, since a test that passes either
  way would be worthless here.
- A single book's own authors keep OPF order (the case that already
  passes — pin it so the fix isn't judged solely on the shared-author case).
- `position` is 0-based and contiguous for a book's authors.
- **A repeated author name creates the book with one link**, not an error
  and not a lost book. Assert `ListBooks` returns it and its author list has
  length 1. On master this returns constraint error 1555 and no book at all.
- Deleting a book still cascades its `book_authors` rows away and leaves the
  `authors` rows — the schema edit must not disturb what
  `2026083105-storage-schema-hardening` established.

`internal/scanner`: an EPUB whose OPF lists two `dc:creator` elements ends up
with them in document order after a scan. This is the end-to-end path the
bug actually travels, and it is the one that proves `internal/epub`'s
ordering survives all the way to `ListBookAuthors`.

## CLAUDE.md

Update the schema paragraph: `book_authors` carries a `position` so a book's
authors keep the order its source file listed them in — `author_id` order is
first-sight-in-the-library order, which is not the same thing once an author
is shared between books. Note that a name credited twice in one file links
once, at its first position.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Delete any local `data/library.db` first — see the premise section.
- Manual: put two EPUBs in the library where the second shares an author
  with the first and credits them *second* in its own OPF. Scan, and confirm
  the second book's card names its authors in the file's order. Before this
  step the shared author leads.
