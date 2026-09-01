# Step: Missing-file reconciliation

## Context

The scanner is additive only. A `book_files` row whose path no longer exists
on disk is left untouched forever — the known gap already flagged in
CLAUDE.md. Consequences, in increasing order of annoyance:

- The library grid shows books that were deleted months ago.
- A send-to-Kindle on one of them will fail at read time, after the user has
  picked a recipient and clicked — the worst place to discover it.
- The multi-location duplicate flag UI.md asks for counts phantom paths, so
  a book that was moved once looks like a duplicate forever.

**The reason this is not a five-line fix.** "Delete rows whose path is
gone" turns a temporarily unmounted volume, a typo'd `LIBRARY_DIR`, or a
network share that was slow to come up into a wiped library. DESIGN.md is
explicit that the watcher is unreliable across Docker volume mounts and
network shares and that the periodic rescan is the safety net — so the
rescan is exactly the thing that must not treat "I can't see it" as "it's
gone".

The whole design problem here is telling *deleted* from *invisible*.

## Depends on

`2026083105-storage-schema-hardening` for `ON DELETE CASCADE`,
`2026083107-write-transaction-reentrancy` for atomic multi-row deletes, and
`2026083109-scanner-sweep-resilience` — which is what makes "did we
successfully walk the directory this path is in?" a question with an
answer. Without that step a sweep aborted halfway looks identical to a
sweep that found nothing, and pruning on the strength of it would be the
data-loss bug in its purest form.

## The rule

Prune a `book_files` row only when **all** of these hold:

1. The sweep walked the directory containing that path **without error**.
   Not "the sweep completed" — a sweep that skipped one subtree per
   `2026083109` must not prune anything in that subtree.
2. `os.Lstat` on the path returns `fs.ErrNotExist` specifically. Any other
   error (`EACCES`, `EIO`, a timeout) means "couldn't look", not "isn't
   there", and is a Warn, not a delete.
3. The sweep saw at least one file overall. A sweep that walks a
   successfully-mounted-but-empty directory is indistinguishable from a
   volume that mounted empty, and the cost asymmetry is enormous: refusing
   to prune costs a stale row until the next sweep, pruning wrongly costs
   the entire index. Refuse, and log at Warn that the library appeared
   empty so nothing was pruned.

Implement 1 by having `Scan` collect the set of directories it walked
cleanly, and prune only paths under those. With paths stored relative to
the root (per `2026083109`) this is a prefix match on the stored value, no
filesystem access needed.

## Two-phase deletion

Even with the rule above, prefer marking over deleting for one sweep.

Add `missing_since TIMESTAMP` (nullable) to `book_files`. A row that fails
the existence check gets `missing_since` set on the first sweep that
notices; a row that is seen again gets it cleared. Rows are only actually
deleted once `missing_since` is older than a grace period —
`MISSING_GRACE`, default `24h`, alongside the other `cmd/server` env vars.

This costs one extra column and buys back the entire class of transient
failures: a volume that is unmounted for an afternoon, a rescan that races
a large file copy, a NAS that reboots. The book stays in the index, the
user never notices, and the row is cleared on the next successful sweep.

The UI can use the column too — a book whose only file is missing renders
greyed with "file not found" rather than vanishing, which is far more
useful than silence when the user is trying to work out what happened. That
is a later UI step; this one just makes the state available.

Adding a nullable column is a plain `ALTER TABLE ADD COLUMN`, so it is one
migration file, no rebuild.

## Deleting the book

When a `book_files` row is deleted and its book has no remaining locations,
delete the book, exactly as `2026083108-scanner-orphan-books` does — reuse
that step's orphan check rather than writing a second one. Both should end
up calling one unexported `pruneOrphanedBookTx(ctx, tx, bookID)`.

As there, leave the cover file on disk and leave `authors` rows alone.

## `internal/storage`

- `SetFilesMissing(ctx, fileIDs []int64, at time.Time) error` and
  `ClearFilesMissing(ctx, fileIDs []int64) error` — batched, one
  transaction each, since a sweep may touch thousands.
- `ListFilesUnder(ctx, prefix string) ([]BookFile, error)` — the candidate
  set for a cleanly-walked directory.
- `PruneMissingFiles(ctx, before time.Time, prefixes []string) (files int, books int, err error)`
  — one transaction: delete qualifying rows, then delete the books left
  with no locations, returning both counts.

## `internal/scanner`

- `Scan` accumulates seen paths and cleanly-walked directory prefixes
  during the walk.
- After the walk, and only if the guards hold, it reconciles: rows under a
  clean prefix that weren't seen get `missing_since` set; rows that were
  seen get it cleared; then `PruneMissingFiles` runs for anything past the
  grace period.
- `Result` gains `Missing int` (newly marked) and `Pruned int` (rows
  deleted), and `runScan`'s summary carries both. A sweep that prunes
  anything logs at Info naming the counts — deletion from the user's index
  should never be silent.

## Tests

- A file deleted between two sweeps is marked, not deleted, on the sweep
  that notices, and deleted once the grace period has passed (inject the
  clock; do not sleep).
- A file that reappears before the grace period expires has
  `missing_since` cleared and is never deleted.
- **An empty library directory prunes nothing** — the single most important
  test here. Scan a populated library, then point the scanner at an empty
  directory, and assert every row survives.
- **An unreadable subdirectory prunes nothing under it** — scan, then
  `chmod 000` the subdirectory and scan again, and assert the rows under it
  survive. Skip as root.
- A path that `Lstat`s with `EACCES` (not `ErrNotExist`) is not marked
  missing.
- Deleting the last location deletes the book; deleting one of two does
  not.

## CLAUDE.md

Replace the "Missing-file handling ... is not implemented — such a row is
simply left stale" sentence with what it does now: rows under a
cleanly-walked directory that no longer exist are marked `missing_since`
and deleted after `MISSING_GRACE` (default 24h); a book losing its last
location is deleted with it; a sweep that saw nothing, or that skipped a
subtree, prunes nothing. Add `MISSING_GRACE` to `cmd/server`'s env list.
Drop missing-file handling from the "still missing" paragraph.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual, the one that matters: run against a real library, then stop the
  server, rename `LIBRARY_DIR` aside so it comes up empty, start again, let
  a sweep run, and confirm the database still has every row and the log
  says the library appeared empty. Restore the directory and confirm the
  next sweep reports everything `Unchanged`.
