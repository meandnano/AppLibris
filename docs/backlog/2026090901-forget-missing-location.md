# Backlog: a renamed top-level folder leaves every book with a dead location forever

## Problem

Since plan `2026090703`, `reconcileMissing` refuses to prune a `book_files`
row whose top-level directory yielded no book files this sweep. That is the
right call for an offline sub-mount, and it has a permanent side effect for
the most ordinary way of hitting it: renaming or moving a top-level folder.

Rename `Fiction/` to `Novels/`. Every book under it gets a new live row
(`Moved`) and keeps its old `Fiction/...` row marked missing. `Fiction/`
never regains a book, so the prune is refused on every sweep for the life
of the deployment. The result is not a phantom card but a phantom
*location* on every live book in the folder: the grid shows "2 paths" for
all of them, the detail page lists the dead path annotated "missing"
indefinitely, and the `directories yielded no files` Warn fires every
fifteen minutes and on every filesystem event. A case-only rename
(`Books/` to `books/`) on a case-insensitive filesystem, which is the SMB
and macOS shape, does the same, since the populated set compares bytes.

The only recovery today is recreating the old folder name with a book in
it and waiting out `MISSING_GRACE`, or hand-editing `book_files`.

## Why this is backlog, not a plan

Nothing is lost: the live row is correct, sends resolve to it because the
dead row is marked, and the book's edits and provenance are intact. The
residue is cosmetic and a log line. The alternative, pruning, is the data
loss the guard exists to prevent, so the guard stays as it is and what is
missing is a way for a person to say "that path is gone for good".

## Re-validate before acting

- Whether `reconcileMissing` still refuses the prune on a top-level
  directory with no seen book files, and whether anything else since has
  given the detail page a way to drop a location.
- Whether the "2 paths" badge and the missing annotation still render as
  described in CLAUDE.md's `internal/web` section.

## Sketch

- A "forget this location" affordance beside each location the detail page
  already lists with its missing annotation, `POST` under the book's id and
  wrapped in `sameSiteOnly` like every other state-changing route, deleting
  exactly that `book_files` row through the same orphan-pruning path
  `PruneMissingFiles` uses so a book's last location cannot be forgotten
  into a bookless book by accident. Offered only for a row that is
  currently marked missing, since a live path is not something to forget.
- Possibly quieter logging for a directory that has been unconfirmed for
  many sweeps in a row, once the affordance exists and the Warn is no
  longer the only thing pointing at the residue.
