# Backlog: preserve author order

## Problem

A book's authors have no defined order. `book_authors` is
`(book_id, author_id)` with no position column, and
`storage.ListBookAuthors` orders only by `book_id`:

```sql
SELECT book_authors.book_id, authors.name
FROM book_authors
JOIN authors ON authors.id = book_authors.author_id
ORDER BY book_authors.book_id
```

Within one book, the rows come back in whatever order SQLite chooses — in
practice `book_authors`' rowid order, which happens to match insertion
order today, but is not guaranteed and will change the moment the query
plan does (an index-driven scan, a join reordering, the `authors_name_idx`
added by `2026083105-storage-schema-hardening`).

The OPF file lists `dc:creator` elements in a meaningful order — first
author first — and `epub.ReadMetadata` preserves it into `Metadata.Authors`.
That ordering is then discarded at the storage boundary. So a book credited
"Gaiman & Pratchett" can render "Pratchett & Gaiman", and
`web.authorLine`'s `"%s and %d others"` form can attribute a book to the
wrong lead author entirely.

## Why this is backlog, not a plan

It renders wrong only for multi-author books, which are a minority of a
typical library, and the wrongness is cosmetic — both names are present and
correct, just possibly transposed. Nothing downstream depends on order:
there is no browse-by-author, no sort-by-author, no provider lookup keyed on
"the first author".

It also currently *looks* right, because rowid order matches insertion
order. That makes it a latent bug rather than a visible one — which is
precisely the argument for writing it down now, while the cause is
understood, rather than rediscovering it as a mystery after an unrelated
index changes a query plan.

## Sketch

Add `position INTEGER NOT NULL DEFAULT 0` to `book_authors`, set from the
loop index in `createBookTx`, and order by it:

```sql
ORDER BY book_authors.book_id, book_authors.position
```

`book_authors` is a child table that nothing references, so this is a plain
`ALTER TABLE ADD COLUMN` — one migration file, no rebuild. The `DEFAULT 0`
means existing rows all sort equal, i.e. they keep today's arbitrary order
rather than being corrupted; correcting them requires re-parsing the source
files, which is the same re-parse pass that
`2026083114-epub-metadata-completeness` defers to a future enrichment queue.
Do not add a bespoke backfill for this alone.

Note the interaction with the primary key: `(book_id, author_id)` stays the
right key — a book crediting the same author twice is a malformed file, not
a case to support — so `position` is a plain column, not part of the key.

## Validate before planning

- Confirm the ordering is still unspecified after
  `2026083105-storage-schema-hardening` lands; the unique index on
  `authors.name` is the most likely thing to change the join's row order,
  and if it does, this may have stopped being latent and become a visible
  bug worth promoting to a plan.
- Check whether a browse-by-author or author-sort feature is imminent. If
  one is, fold this into that step instead — it needs the same column.
