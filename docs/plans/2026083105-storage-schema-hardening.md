# Step: Storage schema hardening

## Context

Three defects in the schema as it stands after `2026083002-book-file-locations`.
They are grouped into one step because they are all schema edits landing in the
same handful of migration files, and splitting them would mean touching those
files three times.

**1. No `ON DELETE CASCADE` anywhere.** `book_files.book_id`,
`book_authors.book_id` and `book_authors.author_id` are plain `REFERENCES`
clauses, and `storage.Open` turns `foreign_keys` ON. Nothing deletes a book
today, so nothing has failed yet — but **deleting a book is currently
impossible** without hand-written multi-table cleanup at every call site.
Verified against the real driver and DSN: with a plain-`REFERENCES` child row
present, `DELETE FROM books` fails with `FOREIGN KEY constraint failed (787)`.

Two planned steps need exactly that delete:
`2026083110-missing-file-reconciliation` (a book whose files are all gone)
and `2026083108-scanner-orphan-books` (a book left with no locations when
content at a path is replaced). Both are blocked on this.

**2. Two incompatible timestamp formats in one table.** `UpsertBookFile`
binds a Go `time.Time` for `modified_at`; `added_at` on the same row is
filled by SQLite's `CURRENT_TIMESTAMP` default. Verified against the real
driver:

| column | stored text |
|---|---|
| `book_files.modified_at` | `2026-08-31 12:34:56.123456789 +0200 CEST` |
| `book_files.added_at` | `2026-08-31 11:16:09` |

`datetime()` / `date()` / `strftime()` return **NULL** on the first form
(verified), so `modified_at` is invisible to every SQLite date function, and
comparing it against `added_at` — or against a `send_log` timestamp later —
is meaningless. DESIGN.md's send log exists specifically to answer "did I
already put this on the Kindle?", which is a time-ordered question; it
should not be built on top of a column shape that can't answer one.

**3. No unique index on `authors.name`.** `findOrCreateAuthor` is a
SELECT-then-INSERT with no database-level uniqueness guarantee, and the
SELECT is a full table scan of `authors` for every author of every new
book — the dominant cost after hashing during a first sweep of a few
thousand books.

## Premise: there is no existing database

The service has never been deployed and no instance exists. Every database
is created by running the migrations from empty.

That changes the shape of this step completely. **Fix the schema by editing
the `CREATE TABLE` migrations in place**, rather than adding rebuild
migrations to transform data that doesn't exist. No table rebuilds, no
backfills, no `INSERT INTO … SELECT`, no legacy-format tolerance in the Go
reader.

> An earlier draft of this plan specified the full SQLite 12-step rebuild
> for `book_files` and `book_authors` — four migration files each, plus
> recreating their indexes, plus a carefully-reasoned backfill that had to
> avoid a timezone trap in the old `modified_at` text. All of that was
> correct *given a database worth preserving*. There isn't one, so it is
> now pure ceremony, and the ceremony was the riskiest part of the step.
>
> This works exactly once. The moment a real database exists, editing an
> applied migration stops being possible — the runner records it in
> `schema_migrations` and never re-runs it — and the rebuild reasoning
> becomes correct again. The same reversal was already applied to
> `docs/plans/completed/2026083106-sort-title-normalisation.md`; that plan
> records the reasoning in more detail.

Anyone holding a local `data/library.db` from an earlier run must delete
it. The edited migrations are already recorded as applied and will not
re-run, so an old file silently keeps the old schema — and here that means
no cascades and no unique index, i.e. the two later plans failing in
confusing ways.

## Schema edits

All in place, no new files except the one index.

**`2026083007_create_book_files_table.sql`** — add the cascade and change
the `added_at` default:

```sql
CREATE TABLE book_files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id         INTEGER NOT NULL REFERENCES books (id) ON DELETE CASCADE,
    file_path       TEXT NOT NULL,
    file_size       INTEGER NOT NULL,
    modified_at     TIMESTAMP NOT NULL,
    added_at        TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'))
);
```

**`2026083004_create_book_authors_table.sql`** — both FKs cascade:

```sql
CREATE TABLE book_authors (
    book_id     INTEGER NOT NULL REFERENCES books (id) ON DELETE CASCADE,
    author_id   INTEGER NOT NULL REFERENCES authors (id) ON DELETE CASCADE,
    PRIMARY KEY (book_id, author_id)
);
```

Cascading `author_id` too is deliberate: deleting an author should take its
join rows, not leave them dangling. Nothing deletes authors yet, and this
step does not add that — but the constraint should say what is true.

**`2026083001_create_books_table.sql`** — change `added_at` and
`modified_at` from `DEFAULT CURRENT_TIMESTAMP` to the same
`DEFAULT (strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'))`.

Verified: SQLite accepts the parenthesised expression as a column default,
it produces exactly the layout below (`2026-08-31T13:36:42.000000000Z`), it
scans back into a `time.Time` through the driver, and `datetime()` reads
it. The earlier draft had to leave `books`' defaults inconsistent because
rebuilding a parent table was too expensive; that compromise is gone.

**`2026083101_create_authors_name_index.sql`** (new) —
`CREATE UNIQUE INDEX authors_name_idx ON authors (name);`

A new file rather than a `UNIQUE` constraint on the column, to match how
every other index in this schema is expressed (`books_content_hash_idx`,
`book_files_file_path_idx`). `2026083101` is the next free migration
number: existing migrations run `2026083001`–`2026083013`.

## Deliberately *not* collapsing the book_files migration history

Migrations `2026083010`–`2026083013` move `file_path`/`file_size` off
`books` and into `book_files`, and `2026083006` creates an index that
`2026083011` then drops. Against an empty database all of that is a no-op:
the `INSERT INTO book_files … SELECT … FROM books` copies zero rows.

It is tempting to collapse them — drop those five files and remove
`file_path`/`file_size` from `books`' `CREATE TABLE` — since the same
no-existing-database argument applies. **Don't.**
`docs/plans/completed/2026083002-book-file-locations.md` is immutable and
describes those migrations by name; deleting them would leave a completed
plan documenting files that no longer exist, and the completed plans are
the project's only record of why the schema looks like it does. The cost of
keeping them is one no-op INSERT against an empty table, once, at first
start.

The edits above are different in kind: they change what a migration
*creates*, and every completed plan's description of them stays true.

## Timestamp format

Standardise on **UTC, fixed-width RFC 3339**, layout
`2006-01-02T15:04:05.000000000Z07:00`, e.g.
`2026-08-31T12:34:56.123456789Z`. Verified against the real driver: it
round-trips exactly into `time.Time` (the driver parses TEXT back when the
column's declared type is `TIMESTAMP`), `datetime()` reads it, and
lexicographic `ORDER BY` matches chronological order.

Explicitly **not** `time.RFC3339Nano`: it strips trailing zeros from the
fraction, so a whole-second `…56Z` sorts after `…56.5Z` as text, and
`…56.1Z` after `…56.10000001Z`. The instant still round-trips, but a plain
`ORDER BY` on the column would be subtly wrong, and that is exactly what a
send-history view will want to do.

Add to `internal/storage`:

```go
// sqliteTimeLayout is the one format this package writes timestamps in:
// UTC, fixed-width so text ordering matches chronological ordering, and
// parseable by SQLite's own date functions. The CREATE TABLE defaults use
// strftime to produce the identical shape.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(sqliteTimeLayout) }
```

## `internal/storage` changes

- `sqliteTimeLayout` and `formatTime` as above.
- `UpsertBookFile` and `UpdateBookFileStat` bind `formatTime(mtime)` rather
  than the raw `time.Time`.
- **Reads are unchanged.** `scanBookFile` keeps scanning straight into
  `time.Time`; the driver parses the new format. The earlier draft needed a
  `parseTime` helper that degraded unparseable values to the zero time, so
  that rows left mid-migration with a blank `modified_at` wouldn't error a
  whole query. With no migration and no legacy rows, every stored value is
  written by `formatTime` and that tolerance is dead code — leave it out
  rather than carrying a defensive branch nothing can reach.

## Tests

- `UpsertBookFile` → `FindFileByPath` returns the same instant, **and** the
  raw stored text matches `sqliteTimeLayout`. Assert on
  `CAST(modified_at AS TEXT)`, not just the round-trip: the round-trip
  passes today with the broken format, which is exactly why this went
  unnoticed.
- `SELECT datetime(modified_at) IS NOT NULL` is true for a written row, and
  for a row's defaulted `added_at`.
- Deleting a book removes its `book_files` and `book_authors` rows and
  leaves the `authors` row alone. Verified as achievable with this schema:
  inline `ON DELETE CASCADE` under the project's DSN cascades to both child
  tables and leaves `authors` intact.
- Deleting a book with a plain-`REFERENCES` child would fail — no test for
  this, since after this step no such child exists; it is recorded in the
  Context above as the reason the step is needed.
- Inserting two authors with the same name violates the unique index.
- `TestOpenIsIdempotent` must still pass. The earlier draft also asked for
  an old-schema fixture to prove a rebuild didn't drop rows; there is no
  rebuild and no old schema, so that test is dropped.

## CLAUDE.md

Update the schema paragraph: `book_files`/`book_authors` cascade from
`books`, `authors.name` is unique, and timestamps are stored as fixed-width
UTC RFC 3339 text so SQLite's date functions and text ordering both work.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Delete any local `data/library.db` first — see the premise section.
- Manual: start the server against a small library, then confirm the schema
  is what was intended rather than what was inherited:
  `sqlite3 data/library.db '.schema book_files'` shows the cascade,
  `PRAGMA foreign_key_check` is empty, and
  `SELECT datetime(modified_at), datetime(added_at) FROM book_files` returns
  non-NULL for both columns.
