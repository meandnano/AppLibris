# Step: an empty subdirectory is not evidence its books are gone

## Position in the sequence

Independent of every other plan in this batch. It touches
`internal/scanner` only, plus one storage query if Decision 2's second
option is taken. Placed first because it is the one finding in the review
that loses data a person typed: manual edits, provenance and enrichment
results, all gone permanently.

## Context

Found in the 2026-09-07 review. `reconcileMissing` in
`internal/scanner/scanner.go` guards against an unmounted volume at the
**root** only:

```go
if result.Scanned == 0 {
	slog.Warn("library appeared empty, skipping missing-file reconciliation", ...)
	return
}
```

CLAUDE.md records the reasoning: "an unmounted volume can present as an
empty directory, so seeing nothing is not evidence that everything is
gone." That reasoning applies to every directory in the tree, and the
code applies it to one.

The NAS deployment this project targets makes a sub-mount the ordinary
shape, not an edge case:

- Docker with two bind mounts, `-v /mnt/disk1:/library/a -v
  /mnt/disk2:/library/b`.
- An NFS or SMB share mounted into a subfolder of the library.
- Unraid with a disk offline for a rebuild, where `/mnt/user/books/disk2`
  reads as present and empty.

In each case `WalkDir` reads the directory cleanly, so nothing lands in
`skippedDirs`. Every `book_files` row under it fails `os.Lstat` with
`fs.ErrNotExist`, which is exactly the signal the two-phase reconciliation
treats as confirmation. After `MISSING_GRACE` (default 24h)
`PruneMissingFiles` deletes those rows and, for a book whose only location
was there, the book with them: `book_authors`, `field_sources` and
`enrichment_jobs` cascade, `send_log.book_id` goes `NULL`. When the disk
returns, every book is re-indexed as new with embedded metadata only.

A weekend rebuild is longer than the grace period. This is the one place
the scanner destroys something the scanner cannot rebuild.

## Scope

In scope: refusing to mark or prune rows under a directory that this sweep
saw as contributing nothing, using the same posture `skippedDirs` already
takes.

Out of scope, with reasons:

- **Storing a device id per row and comparing `st_dev`.** It is the
  precise test, but it adds a column the mover story in CLAUDE.md
  explicitly argues against ("the inode and physical disk it does change
  are not things this index stores"), and it needs a migration plus
  backfill for a guarantee the cheaper rule below already gives.
- **Lengthening `MISSING_GRACE`.** A longer window narrows the failure
  without removing it, and the default exists for the case it handles
  well: a file genuinely deleted.
- **Detecting mount points via `/proc/self/mountinfo`.** `mountFor`
  already reads it for the startup check, but it is absent on macOS and
  the directory that vanishes may not be a mount point at all (an Unraid
  disk share is a directory inside a FUSE mount).

## Decision 1: the rule is "a directory that owns rows and contributed no files is unconfirmed"

`reconcileMissing` already has `seen` (every relative path visited) and
`all` (every `book_files` row). From those two it can derive, per row, the
question that matters: did this sweep see **any** file under the row's
directory?

Concretely: build a set of directories that had at least one seen file
(every ancestor of every seen path, up to and including the root). For a
row not seen this sweep, walk up its directory path; if **no** ancestor
directory below the root had a seen file, the row's whole subtree looked
empty and the row is left untouched, exactly as a `skippedDirs` row is.
Only when some ancestor did produce files this sweep is the `Lstat`
confirmation trusted.

Why "some ancestor" rather than "its immediate directory": a book moved
from `a/novels/x.epub` to `a/x.epub` leaves `a/novels/` empty and that is
a real deletion, but `a/` still has files, so the row is confirmed. A
disk mounted at `a/` going offline leaves `a/` with no files at any depth,
so nothing under it is confirmed. The rule separates the two cases
without a device id.

The cost is an honest one: deleting the **last** file in a top-level
directory (or the whole directory) is never pruned while that directory
stays empty or absent. The row stays marked missing indefinitely, the
detail page shows its "missing" annotation, and the grid keeps the card.
Recorded as the trade: a phantom card is recoverable by deleting the
book's other traces or by the directory gaining a file; a pruned book's
edits are not recoverable at all. The `Scanned == 0` root guard has made
this exact trade for the whole library since it was written.

**Correction, found while implementing.** "Left untouched" above, and
"neither marked nor pruned" in the Tests section, were built first: the row
was skipped before `Lstat`, so it was never given a `missing_since`. That
contradicts this Decision's own next paragraph ("stays marked missing … the
detail page shows its 'missing' annotation") and the Verification bullet
("the marks clear on the next sweep"), and review found it regresses
sending: `internal/sender`'s `resolveFile` takes the first `ListBookFiles`
row with a `NULL` `missing_since`, in `file_path` order, and `process` fails
the job on a stat error without trying the next copy, so an unmarked dead
row that sorts first fails every send of its book — which renaming a
top-level folder would do to every book in it — and the detail page shows
the dead path as a live location. What shipped marks the row exactly as
before and refuses only the prune, whatever the mark's age. The Tests
bullet reads accordingly as "marked, never pruned", and the offline-mount
test gained the remount step from Verification: the same bytes back at the
same path clear the mark on the next sweep.

## Decision 2: the unconfirmed set is logged once per sweep, not per row

A warning per row would be `N` lines per sweep for a disk that is offline
for a weekend, every fifteen minutes. One Warn per sweep naming the
directories left unconfirmed and how many rows sit under them is enough
to explain the phantom cards, and matches how `skippedDirs` is logged.

## Decision 3: the `Scanned == 0` guard stays

It is now the degenerate case of the new rule (the root itself contributed
nothing), but it costs nothing to keep, its Warn message is specific and
already documented, and removing it would mean the new rule carries a
guarantee alone that a reader has to reason about. Keep both.

## Changes

- `internal/scanner/scanner.go`: `reconcileMissing` builds the
  populated-directory set from `seen`, and skips mark and prune for a row
  whose ancestor chain has no populated directory. One Warn per sweep
  listing the unconfirmed top-level directories and their row count.
- `Result` gains an `Unconfirmed` count so `runScan`'s summary line in
  `cmd/server` can show it beside `missing` and `pruned`.

## Tests

`internal/scanner`, alongside the existing reconciliation tests:

- Two subdirectories, each with indexed books. Empty one of them
  completely (simulate the offline mount by removing every file). Sweep:
  its rows are **neither marked nor pruned**, the other directory's rows
  are untouched, `Unconfirmed` counts the rows. Sweep again past the
  grace period: still nothing pruned.
- The same setup but one file removed from a directory that keeps
  another: the removed row **is** marked, and pruned after grace. This
  pins the distinction Decision 1 draws.
- A file moved from `a/novels/` to `a/`, leaving `a/novels/` empty: the
  old row is marked (ancestor `a/` has files), so a real reorganisation
  still reconciles.
- A single-directory library where the only file is deleted: not pruned,
  since the root then has `Scanned == 0`. Confirms the two guards agree.
- A mutation check worth doing by hand: replace "some ancestor" with
  "immediate directory" and confirm the moved-file test fails.

## CLAUDE.md

`internal/scanner`'s reconciliation paragraph gains the rule: a row under
a directory that contributed no files this sweep is left untouched, the
same posture `skippedDirs` takes, and the reason the `st_dev` alternative
was declined. The sentence about the `Scanned == 0` guard is kept and
described as the root case of the same rule.

## Verification

- Compose file with two bind mounts into the library. Index both. Stop
  the container, unmount one, start it: the log shows one Warn naming the
  directory, the grid keeps the cards with "missing" on the detail page,
  and after `MISSING_GRACE` nothing is pruned. Remount: the marks clear on
  the next sweep and edits are intact.
- Delete a single book from a directory that keeps others: it is marked,
  and pruned after grace, as before.
