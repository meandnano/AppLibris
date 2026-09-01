# Step: Stop the scanner orphaning book rows

## Context

Two paths in the scanner produce a `books` row with **zero** `book_files`
rows — a card in the library grid backed by no file on disk, which can
never be opened, never be sent to a Kindle, and is never cleaned up.

**1. Replacing content at a known path.** `UpsertBookFile`'s
`ON CONFLICT(file_path) DO UPDATE SET book_id = excluded.book_id` reassigns
the path to the new book. Whatever book previously owned that path keeps its
`books` row and loses its only location. Verified: after writing content
`old` at `/lib/x.epub` and then content `new` at the same path, the "Old"
book has 0 file rows and `ListBooks` still returns both books.

The trigger is mundane — re-downloading a book over the same filename, or a
library tool rewriting a file's metadata in place. The `ON CONFLICT` clause
itself is right (its doc comment correctly explains why a path must be able
to move between books); what's missing is cleaning up behind it.

**2. A crash between `CreateBook` and `UpsertBookFile`.** `scanFile` calls
them as two separate transactions, so a process death or a write error
between the two leaves the book with no location. This one self-heals — the
next sweep re-hashes, finds the book by content hash, and attaches the path
— but the window is real and the fix is free once
`2026083107-write-transaction-reentrancy` has landed.

## Depends on

`2026083105-storage-schema-hardening` (deleting a book needs
`ON DELETE CASCADE` on `book_files` and `book_authors`) and
`2026083107-write-transaction-reentrancy` (both fixes are multi-statement
atomic writes). Do not start this one before both are in.

## Scope

In scope: making path reassignment and book creation atomic, and deleting a
book left with no locations *as a direct result of an operation in this
sweep*.

Out of scope: books whose files have disappeared from disk — that is
`2026083110-missing-file-reconciliation`, and it is a genuinely different
problem (it needs to distinguish "deleted" from "unmounted", which this
step does not, because here we have positive evidence that a specific path
now belongs to different content).

## `internal/storage`

Two new exported methods, each one `Write` wrapping existing `…Tx` helpers.

**`CreateBookWithFile(ctx, b Book, authorNames []string, path string, size int64, mtime time.Time) (id int64, orphanedID int64, orphanedTitle string, err error)`**

`createBookTx` + `upsertBookFileTx` in one transaction. Replaces the
`CreateBook` + `UpsertBookFile` pair in `scanFile`. `CreateBook` stays
exported — the tests use it, and it is the right primitive for a future
conversion step that creates a derived book before writing its file.

> **Revised during implementation.** The signature above was originally
> `(int64, error)` — just the book+file insert, no orphan handling, on the
> theory that only `ReassignFileAndPruneOrphan`'s branch (known content
> landing on an already-occupied path) could orphan a book.
>
> That's wrong: `upsertBookFileTx`'s `ON CONFLICT(file_path) DO UPDATE`
> reassigns a path unconditionally, regardless of whether the *new* owner
> is a brand-new book (`CreateBookWithFile`) or an existing one
> (`ReassignFileAndPruneOrphan`) — the two are indistinguishable to that
> SQL statement. An end-to-end scanner test built exactly as the Tests
> section below describes (write file A, overwrite the same path with
> different, never-seen content, scan again) caught this directly: the
> overwrite's content doesn't match any existing book, so the scanner
> takes the *create* branch, and the old code left Book A's row behind
> with zero locations — the very bug this step exists to fix, just via the
> other branch than originally assumed.
>
> Fixed by giving `CreateBookWithFile` the same prune step, sharing it with
> `ReassignFileAndPruneOrphan` via two unexported helpers:
> `previousFileOwnerTx` (read the path's current owner, before the upsert
> overwrites it) and `pruneOrphanIfEmptyTx` (the "still has rows, and is it
> even a different book" check and delete, after it). Both storage methods
> now return `(orphanedID, orphanedTitle)` on the same terms — see the
> revision under `ReassignFileAndPruneOrphan` below for why the title has
> to travel with the id rather than being looked up afterward.

**`ReassignFileAndPruneOrphan(ctx, bookID int64, path string, size int64, mtime time.Time) (fileID int64, orphanedID int64, orphanedTitle string, err error)`**

In one transaction:

1. Read the current owner of `path`, if any (`SELECT book_id FROM
   book_files WHERE file_path = ?`) — `previousFileOwnerTx`.
2. `upsertBookFileTx` as today.
3. If there was a previous owner and it differs from `bookID`, and it now
   has no `book_files` rows, delete it — `pruneOrphanIfEmptyTx`, shared
   with `CreateBookWithFile` (see its revision note above). The cascade
   from step `2026083105` takes `book_authors` with it; `authors` rows are
   left alone, which is correct — an author with no books is not itself
   wrong, and reference-counting authors is a separate concern.
4. Return the id **and title** of any book deleted, so the scanner can log
   and count it.

The "now has no rows" check must be `NOT EXISTS (SELECT 1 FROM book_files
WHERE book_id = ?)` evaluated *after* the upsert, not a count taken before.
A book with two known locations that loses one is not an orphan, and this
is the case a before-check gets wrong.

> **Revised during implementation.** The signature above originally
> returned only `(fileID, orphanedID, error)` — the Storage section said
> "return the id," full stop. But the `internal/scanner` section (unchanged
> below) always specified logging the orphan's *title* too, and by the
> time a caller has `orphanedID` back, the row is already deleted — a
> second lookup can't find it. Fetching the title before the `DELETE`,
> inside the same transaction, and returning it alongside the id was the
> only way to satisfy both sections; the exact 3-value signature quoted
> above didn't leave room for it and has been corrected to 4.

**Do not delete the cover file.** The book row is going away but the
thumbnail on disk is keyed by content hash, and that content may well still
exist at another path (or reappear). The covers directory is disposable and
regenerable by design; leaving a stray thumbnail costs ~40KB and removing it
risks deleting a cover another book row is using. Sweeping unreferenced
covers is a separate maintenance step, if it is ever worth writing.

## `internal/scanner`

- `Result` gains `Orphaned int` — books deleted because they lost their last
  location. It belongs in the sweep summary: silently deleting a book row
  is exactly the kind of thing that should be visible in the logs when
  someone asks "where did that book go".
- `scanFile`'s new-book branch calls `CreateBookWithFile`; its known-content
  branch calls `ReassignFileAndPruneOrphan`. Both can now report an orphan
  (see the revision note above), so both route through one shared
  `logOrphan` helper: when `orphanedID != 0`, it increments
  `result.Orphaned` and logs at **Info** with `path`, `orphaned_book_id`
  and the orphan's title. Info, not Warn: this is correct, expected
  behaviour when a file is replaced, not a problem — but it is a
  destructive act on the user's index and should be greppable after the
  fact.
- `runScan` in `cmd/server` adds `orphaned` to its summary attrs.

## Tests

`internal/storage`:

- `CreateBookWithFile` creates both rows; a forced failure leaves **no**
  `books` row (the atomicity property — assert the table is empty, not
  just that an error came back). In practice this can't be forced at the
  file insert specifically: `upsertBookFileTx`'s only real per-row
  constraint (`file_path` uniqueness) is absorbed by its own
  `ON CONFLICT DO UPDATE`, so no schema-legal input makes it fail. A
  duplicate author name forces a real failure instead (both occurrences
  resolve to the same author id, so the second `book_authors` insert hits
  its primary key) — inside `createBookTx`, before the file insert even
  runs, which still proves the property that matters: the whole operation
  is one transaction, not "insert the book, then maybe the file."
- `ReassignFileAndPruneOrphan` deletes a previous owner that had exactly
  one location, and returns its id **and title**.
- It does **not** delete a previous owner that had two locations — assert
  the book survives with one remaining `book_files` row. This is the case a
  naive implementation gets wrong.
- It does not delete anything when the path's owner is unchanged (a plain
  stat refresh).
- Deleting the orphan takes its `book_authors` rows with it and leaves the
  `authors` row (cascade behaviour, worth pinning here rather than only in
  the schema step's tests, since this is the first caller that relies on it).

`internal/scanner`, as end-to-end regressions for the reported bug: write
file A, scan, overwrite the same path with different content, scan again,
assert exactly one book exists, that it is B, and that `Result.Orphaned` is
1. Today that assertion fails with two books.

Two variants, added during implementation once it turned out both scanner
branches can orphan a book (see the revision note under
`CreateBookWithFile` above): one where the overwrite's content is
never-before-seen (takes the create-a-book branch), one where it matches
an already-existing third book elsewhere in the library (takes the
known-content branch). Both assert the same shape — one book left, the
orphan's row gone.

## CLAUDE.md

Update the `internal/scanner` bullet: content replaced at a known path
reassigns the location and deletes the book left with no locations, in one
transaction; new books are created together with their first location.
Leave the "missing-file handling is not implemented" sentence — this step
does not address it, and conflating the two is what makes the deleted-vs-
unmounted distinction easy to lose.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: with the server running, overwrite a book in `LIBRARY_DIR` with a
  different EPUB under the same filename, wait for the next sweep (or
  restart), and confirm the grid shows one card, the new one, and the log
  carries the Info line naming the removed book.
