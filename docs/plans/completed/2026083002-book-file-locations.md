# Step: Track multiple file locations per book

## Context

DESIGN.md's Duplicate detection section: "v1: byte-identical only. Same
content hash at two paths is one entry with multiple file locations." The
current schema can't represent that — `books.file_path` is a single column
([internal/storage/books.go](../../internal/storage/books.go)), so when the
scanner ([internal/scanner/scanner.go:98-104](../../internal/scanner/scanner.go))
finds known content at a path it hasn't seen before, it overwrites the
book's one `file_path`. Two files with identical content sitting at two
real, simultaneously-existing paths get their row's `file_path` flipped
between them on every sweep, instead of both locations being tracked — not
data loss (the row always points at *a* real file), but not what DESIGN.md
specifies, and it's a full rewrite every sweep for anyone with even one
real duplicate.

Fixing this also resolves a second thing flagged during step 2: whether
`books.modified_at` means "the file's mtime" or "last time this record was
edited" was ambiguous, because the schema had nowhere else to put a file
mtime. Once file mtime moves to its own table (as it must, being per
location, not per book), `modified_at` on `books` is free to mean what it
naturally reads as — last edit to the record — with nothing scanner-owned
writing to it going forward.

**Recommended sequencing:** do this before the covers step
(`docs/plans/2026083001-covers.md`) — covers touches the same "genuinely new
content" branch of `scanFile` that this step restructures, and covers
builds more cleanly on the corrected shape than the current one. Not a hard
requirement, just less rework either way.

## Schema change

New table, replacing the location fields currently on `books`:

**`book_files`** — `id, book_id (FK -> books.id), file_path, file_size,
modified_at, added_at (default CURRENT_TIMESTAMP)`. `file_path` is unique
across the whole table (a filesystem path can only ever host one file at a
time, regardless of which book it currently belongs to) — enforced by a
separate `CREATE UNIQUE INDEX`, matching this repo's existing convention of
never inlining uniqueness into `CREATE TABLE` (see how `books.content_hash`
is indexed). A second index on `book_id` supports "does this book have more
than one file" lookups later (the actual UI flag DESIGN.md describes is out
of scope here — this step is the data model and scanner correctness, not
the UI).

**Migrations**, one statement per file per this repo's convention, in this
order (existing data is migrated forward, not discarded, in case a real
`library.db` already has scanned rows):
1. `CREATE TABLE book_files (...)`
2. `CREATE UNIQUE INDEX` on `book_files.file_path`
3. `CREATE INDEX` on `book_files.book_id`
4. `INSERT INTO book_files (book_id, file_path, file_size, modified_at) SELECT id, file_path, file_size, modified_at FROM books` — carries forward any existing rows before the source columns disappear
5. `DROP INDEX books_file_path_idx` (from migration `2026083006`) — dropped before the column, since a column that's still indexed can't be dropped
6. `ALTER TABLE books DROP COLUMN file_path`
7. `ALTER TABLE books DROP COLUMN file_size`

`books.modified_at` itself is untouched by these migrations — it stays,
just stops being scanner-owned (see Context above).

## `internal/storage` changes

- `Book` struct: remove `FilePath`, `FileSize`. Keep everything else.
- New `BookFile` struct: `ID, BookID, FilePath, FileSize, ModifiedAt, AddedAt`.
- Remove `FindBookByPath`, `UpdateBookFileLocation`. Replace with:
  - `FindFileByPath(ctx, path) (*BookFile, error)` — `nil, nil` on no match.
  - `UpsertBookFile(ctx, bookID int64, path string, size int64, mtime time.Time) (fileID int64, error)` — one statement, `INSERT INTO book_files (...) VALUES (...) ON CONFLICT(file_path) DO UPDATE SET book_id = excluded.book_id, file_size = excluded.file_size, modified_at = excluded.modified_at`. This single upsert correctly handles every case that used to need separate handling: a genuinely new location, a moved/renamed file (old path's row goes stale, new path's row is inserted), a second location for already-known content, and even a path whose content got replaced in-place with different-but-already-known content (rare, but the conflict path re-points it instead of erroring on the UNIQUE constraint).
- `UpdateBookFileStat(ctx, fileID int64, size int64, mtime time.Time) error` — same purpose as before, now keyed on a `book_files.id` instead of a `books.id`.
- `CreateBook(ctx, b Book, authorNames []string) (int64, error)` — drops the file-location parameters entirely; only inserts the book row and links authors, same as before minus the location column. Callers always follow it with `UpsertBookFile` — see below for why that's fine to do as two separate writes instead of one transaction.

## `internal/scanner` changes

`scanFile` ([internal/scanner/scanner.go:64-126](../../internal/scanner/scanner.go))
collapses the four-way branch (unchanged / touched / moved / new) into a
simpler shape now that "attach this path to this book" is one idempotent
operation regardless of which case it turns out to be:

```
bf := FindFileByPath(path)
if bf != nil && bf.FileSize == size && bf.ModifiedAt.Equal(mtime):
    return Unchanged   // cheap check, no hash needed — same as today

hash := hashFile(path)
book := FindBookByContentHash(hash)

if book != nil && bf != nil && bf.BookID == book.ID:
    UpdateBookFileStat(bf.ID, size, mtime)   // same book, same path, just touched
    return Unchanged

if book == nil:
    book.ID = CreateBook(metadata..., authorNames)   // genuinely new content
    UpsertBookFile(book.ID, path, size, mtime)
    return New

UpsertBookFile(book.ID, path, size, mtime)   // known content, new/changed/duplicate location
return Moved
```

Deliberately **not** distinguishing "moved" from "new duplicate location" —
from a single path's perspective mid-walk, they look identical (known
content, no `book_files` row at this exact path yet), and the walk can't
tell whether some *other* path holding the same content still exists on
disk without extra bookkeeping this step doesn't need. Both land in
`Moved`, which stays as the `Result` field name (doc comment updated) to
avoid also renaming a field for a distinction the design doesn't act on.

Creating the book and attaching its first location are two separate
`db.Write` calls rather than one wrapped in a bigger transaction: if the
process died in between, the book row would exist with zero locations
until the next scan — but that state self-heals on its own, because the
next time that path is scanned, `FindBookByContentHash` finds the
already-created book and takes the `Moved`/upsert branch, attaching the
location. No orphaned state survives a second sweep.

**`scanner_test.go`** — update existing tests for the new signatures
(`FindFileByPath` instead of `FindBookByPath`), and add: two fixture files
with identical content at two different paths, scanned together, produce
one `books` row and two `book_files` rows (the actual bug this step fixes)
— run the scan twice to confirm neither path's `file_path` ever gets
dropped or flips.

## `internal/storage/books_test.go` changes

Update for the new `Book`/`BookFile` split: `CreateBook` no longer takes
location fields; add tests for `FindFileByPath`, `UpsertBookFile` (insert
case and conflict/update case), and `UpdateBookFileStat`.

## CLAUDE.md

Update "Current implementation": `books` schema description drops
`file_path`, gains `book_files` as a real table (not a nullable/optional
add-on); scanner description updates to reflect that a book can now have
multiple recorded locations.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean, including the
  updated/added tests above.
- Manual: reuse the fixture-EPUB approach from steps 1–2's verification.
  Scan a library dir with two byte-identical files at different paths
  (copy, not move) — confirm via `sqlite3` one `books` row and two
  `book_files` rows. Rescan without touching the filesystem — confirm
  `Unchanged` on both, no rewritten rows. Then actually rename one of the
  two paths and rescan — confirm the old path's `book_files` row is simply
  stale (still there, pointing at a path that no longer exists — expected,
  since missing-file handling is separately deferred) and the new path got
  its own row, both still pointing at the same `book_id`.
