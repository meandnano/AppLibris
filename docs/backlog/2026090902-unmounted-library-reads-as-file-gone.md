# Backlog: an unmounted volume under the library still reads as "the file is no longer in the library" to the sender

## Problem

Plan `2026090708` made `internal/sender` record "could not read the
file — try again" for every filesystem error but `fs.ErrNotExist`, naming
a dropped SMB mount among the NAS failures it set out to stop
misreporting. That holds for a mount that *errors* (`EHOSTDOWN`,
`ESTALE`, `EIO`). It does not hold for the more ordinary shape, where a
mount goes away and its mountpoint reads as an empty directory: every
path under it then fails `os.Stat` with `ENOENT`, which is
`fs.ErrNotExist`, so `failFileError` records `fileGoneReason` — the
false statement the plan removed for the other errors — for every send
of every book on that volume, and the history page keeps it for a month.

The scanner already treats this shape as an unknown rather than
evidence: `reconcileMissing` refuses to prune under a top-level directory
that yielded no book files this sweep, and skips reconciliation entirely
when the sweep saw zero files. The sender has no such context; it sees
one path and one errno.

## Why this is backlog, not a plan

The send still fails, and a retry once the mount is back succeeds, so
nothing is lost or duplicated. The row is marked missing by the next
sweep, after which `resolveFile` skips it and the job fails with the same
reason for a truer cause. What is wrong is one sentence in a status box
during the window between the mount dropping and the sweep marking the
rows.

## Re-validate before acting

- Whether `failFileError` still maps `fs.ErrNotExist` alone to
  `fileGoneReason`, and whether anything since has given the worker a
  view of the sweep's per-directory findings.
- Whether `book_files.missing_since` is still what `resolveFile` filters
  on, so a marked row is skipped rather than stat'd.

## Sketch

The sender could ask the same question the scanner does before believing
`ENOENT`: whether the row's top-level directory under `LIBRARY_DIR`
exists and is non-empty. An `ENOENT` under an empty or absent top-level
directory would record `fileUnreadableReason`, since that is what an
offline volume looks like, and only an `ENOENT` under a populated one
would keep `fileGoneReason`. That duplicates a rule the scanner owns, so
the honest version shares it — a helper in `internal/scanner` or a small
package both import — rather than a second copy that drifts.
