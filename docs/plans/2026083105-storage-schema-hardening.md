# Step: Storage schema hardening

## Context

Three defects in the schema as it stands after `2026083002-book-file-locations`.
They are grouped into one step because two of them require rebuilding the same
table, and doing that once is much cheaper than twice.

**1. No `ON DELETE CASCADE` anywhere.** `book_files.book_id`,
`book_authors.book_id` and `book_authors.author_id` are plain `REFERENCES`
clauses, and `storage.Open` turns `foreign_keys` ON. Nothing deletes a book
today, so nothing has failed yet — but **deleting a book is currently
impossible** without hand-written multi-table cleanup at every call site.
Two planned steps need exactly that:
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

## Scope

In scope: the migrations and the `internal/storage` changes needed to make
them correct. Out of scope: actually deleting anything (the two plans named
above own that), and the `books.sort_title` problem, which was a separate
step and is already done — see
`docs/plans/completed/2026083106-sort-title-normalisation.md`.

## Migrations

Per the convention in CLAUDE.md, **one statement per file**, applied in
filename order. A SQLite table rebuild is four statements, so it is four
files.

The usual 12-step `ALTER TABLE` dance requires `PRAGMA foreign_keys=OFF`,
which is a no-op inside a transaction — and `applyMigration` wraps every
file in one. **That is fine here**: the standard procedure needs the pragma
only so that *other* tables' FK clauses aren't rewritten when the target is
renamed, and so a `DROP` doesn't trip a parent-side check. `book_files` and
`book_authors` are pure child tables — nothing references either — so both
can be rebuilt with foreign keys left ON, inside the per-file transaction
the runner already gives us. Do not add pragma-toggling to the runner.

**`book_files` rebuild** (cascade + timestamp normalisation together):

- `2026083101_create_book_files_new.sql` — same columns, but
  `book_id INTEGER NOT NULL REFERENCES books (id) ON DELETE CASCADE`.
- `2026083102_copy_book_files.sql` — `INSERT INTO book_files_new SELECT ...`,
  converting `modified_at` (see **Backfill** below).
- `2026083103_drop_book_files.sql`
- `2026083104_rename_book_files_new.sql`
- Then recreate the two indexes the drop takes with it:
  `2026083105_create_book_files_file_path_index.sql` (UNIQUE on `file_path`)
  and `2026083106_create_book_files_book_id_index.sql`.

**`book_authors` rebuild** (cascade only) — same four-file shape, with both
FKs gaining `ON DELETE CASCADE`, plus recreating
`book_authors_author_id_idx`. `book_authors` has no timestamps, so nothing
to backfill.

**`authors.name`** — `2026083113_create_authors_name_index.sql`:
`CREATE UNIQUE INDEX authors_name_idx ON authors (name)`.

> If the existing library already contains two `authors` rows with the same
> name, this index creation fails and the migration aborts (correctly —
> better than silently merging). `findOrCreateAuthor`'s
> SELECT-then-INSERT on a single-connection write pool makes duplicates
> very unlikely, but check with
> `SELECT name, COUNT(*) FROM authors GROUP BY name HAVING COUNT(*) > 1`
> before running, and dedupe by hand if it returns rows.

Renumber if these collide with migrations added by a step that lands first;
the numbers above assume this is the next migration batch.

## Timestamp format

Standardise on **UTC, fixed-width RFC 3339**, layout
`2006-01-02T15:04:05.000000000Z07:00`, e.g.
`2026-08-31T12:34:56.123456789Z`. Verified against the real driver: it
round-trips exactly into `time.Time` (the driver parses TEXT back when the
column's declared type is `TIMESTAMP`), `datetime()` reads it, and
lexicographic `ORDER BY` matches chronological order.

Explicitly **not** `time.RFC3339Nano`: it strips trailing zeros from the
fraction, so `…56.1Z` and `…56.10000001Z` (and a whole-second `…56Z`
against `…56.5Z`) compare in the wrong order as text. The instant still
round-trips, but a plain `ORDER BY` on the column would be subtly wrong,
and that is exactly what a send-history view will want to do.

Add to `internal/storage`:

```go
// sqliteTimeLayout is the one format this package writes timestamps in:
// UTC, fixed-width so text ordering matches chronological ordering, and
// parseable by SQLite's own date functions.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(sqliteTimeLayout) }
```

`UpsertBookFile` and `UpdateBookFileStat` bind `formatTime(mtime)` instead
of the raw `time.Time`. Reads are unchanged — scanning into `time.Time`
keeps working.

Change the `books.added_at` / `books.modified_at` and `book_files.added_at`
defaults to match, so a row's timestamps are all one shape: replace
`DEFAULT CURRENT_TIMESTAMP` with
`DEFAULT (strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'))` in the rebuilt
`book_files`. `books` is not being rebuilt in this step, so leave its
defaults alone and note the inconsistency — `books.added_at` is not read by
anything yet, and rebuilding `books` means rewriting the FK from
`book_files` and `book_authors` too, which is a much larger change for no
current benefit. Fold it into whichever later step first needs to sort by
`books.added_at`.

## Backfill

The `INSERT INTO book_files_new ... SELECT` must convert existing
`modified_at` values. **The obvious SQL is wrong**: the stored text carries
whatever zone the scanning process was in (verified — the driver does *not*
normalise to UTC before formatting; a `+02:00` time is stored as
`… +0200 CEST`), so `substr(modified_at, 1, 19)` yields the *local wall
time*, silently shifting every timestamp by the machine's offset. On a UTC
host it happens to be right, which is exactly what makes it a trap.

Rather than reimplementing Go's zone parsing in SQL, exploit the fact that
**a wrong `modified_at` is self-healing**: it only feeds the scanner's
path+size+mtime cheap check, and a mismatch there costs one re-hash of that
file and a corrected row. So copy the column as:

```sql
INSERT INTO book_files_new (id, book_id, file_path, file_size, modified_at, added_at)
SELECT id, book_id, file_path, file_size, '', added_at FROM book_files;
```

— i.e. deliberately blank `modified_at`, letting the next sweep re-stat
every file and write it in the new format. The one-time cost is a full
re-hash of the library on the first scan after upgrading; the benefit is
that no timestamp is silently shifted by an hour or two. Say so in the
migration file's comment.

This requires `modified_at` to tolerate `''` for the gap between the
migration and the next sweep. It is `NOT NULL` with no default, and `''`
satisfies that. `scanBookFile` scanning `''` into a `time.Time` will fail,
so `FindFileByPath` must handle it — scan `modified_at` into a
`sql.NullString`-like intermediate and treat unparseable as "no known
mtime", which the cheap check already handles correctly (mismatch → re-hash).
Add a small helper rather than spreading the parse:

```go
func parseTime(s string) time.Time { t, _ := time.Parse(sqliteTimeLayout, s); return t }
```

A zero `time.Time` never `.Equal`s a real mtime, so the cheap check falls
through to hashing on its own, with no extra branch in the scanner.

## `internal/storage` changes

- `formatTime` / `parseTime` / `sqliteTimeLayout` as above.
- `UpsertBookFile`, `UpdateBookFileStat`: bind formatted strings.
- `scanBookFile` and any `SELECT`-into-`BookFile` path: read `modified_at`
  as TEXT and run it through `parseTime`, so a blank or legacy-format value
  degrades to the zero time instead of erroring the whole query. `added_at`
  likewise, for rows written before this step.

## Tests

- `UpsertBookFile` → `FindFileByPath` returns the same instant, and the raw
  stored text matches `sqliteTimeLayout` (assert on
  `CAST(modified_at AS TEXT)`, not just the round-trip — the round-trip
  passes today with the broken format, which is why this went unnoticed).
- `SELECT datetime(modified_at) IS NOT NULL` is true for a written row.
- A row with `modified_at = ''` reads back as a zero `time.Time` rather
  than erroring, and a scan over that file re-hashes and rewrites it.
- Deleting a book removes its `book_files` and `book_authors` rows and
  leaves the `authors` rows alone (cascade reaches the join table, not the
  author).
- Inserting two authors with the same name violates the unique index.
- `TestOpenIsIdempotent` already covers migrations re-applying cleanly;
  make sure it still passes against a DB created *before* this batch (open
  an old-schema fixture, migrate, assert the rows survived) — that is the
  only test that would catch a rebuild dropping data.

## CLAUDE.md

Update the schema paragraph: `book_files`/`book_authors` cascade from
`books`, `authors.name` is unique, and timestamps are stored as fixed-width
UTC RFC 3339 text so SQLite's date functions and text ordering both work.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: run against a copy of a real pre-migration `library.db`, confirm
  `PRAGMA foreign_key_check` is empty, row counts in `book_files` and
  `book_authors` are unchanged, and the first sweep afterwards reports every
  file as `New`/`Moved`-free but re-hashed (blank mtimes), with the second
  sweep reporting them all `Unchanged`.
