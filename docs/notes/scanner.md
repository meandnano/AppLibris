# Scanner and filesystem watcher

Rationale for `internal/scanner` and the scan loop in `cmd/server`. The
package map and the invariants that must hold are in CLAUDE.md.

## Sweep and identity

There is one code path into the index, `Scan`, and two things that wake it:
a ticker (`SCAN_INTERVAL`) and a poke from the watcher. A sweep is a sweep
however it was woken, two can never overlap, and nothing about the index's
correctness depends on an event arriving. The rescan is the mechanism; the
watcher only buys latency.

The startup sweep runs in the background so the server answers `/healthz`
immediately. A large library delays its own completeness rather than the
process coming up.

A cheap `path + size + mtime` comparison against `book_files` skips an
unchanged file entirely. Only a mismatch pays for a SHA-256 hash and a
metadata parse, which is what keeps a rescan of a large library fast.

**Identity is the content hash, not the path.** Known content at a new
path gets an additional `book_files` row rather than a new book. A moved
file and a genuine duplicate copy are indistinguishable from any single
path's point of view, so both are handled the same way, and enriched or
hand-edited metadata survives a reorganisation. The grid flags a book with
more than one location; it never merges or deletes, because the scanner
owns the library directory and its rule is that writes only ever create new
paths.

Content replacing a known path's previous content reassigns that row and,
in the same transaction, deletes whatever book is left with no locations.
Both `ReassignFileAndPruneOrphan` and `CreateBookWithFile` reassign a path
unconditionally, so either can orphan the previous owner; the deletion is
logged at Info.

Supported files are matched on filename *suffix*, not `filepath.Ext`,
because `.fb2.zip` is two extensions. `.fb2` and `.fb2.zip` both record
`format` as `fb2`: how a book is packaged on disk is not something the
format badge should surface.

`Scan` checks `ctx.Err()` before the walk and on every entry, so a shutdown
stops the walk outright and skips reconciliation instead of failing each
remaining file's database call one at a time.

## Paths and symlinks

`book_files.file_path` is stored relative to `LIBRARY_DIR`,
slash-separated, so the index survives the library being mounted at a
different absolute path (`./library` on a dev box, `/library` in a
container). A directory the walk cannot read costs its subtree and counts
an error; it does not abort the sweep.

`cmd/server` resolves `LIBRARY_DIR`, `COVERS_DIR` and `DB_PATH`'s directory
through `resolveDir` (create if absent, then `filepath.EvalSymlinks`), so
every consumer is handed one root that means the same thing whether or not
links are followed. `LIBRARY_DIR` is the one that needs it:
`filepath.WalkDir` `Lstat`s its root and never follows a link, so a
symlinked root (`~/Books -> /volume1/books`, the ordinary NAS shape) would
otherwise walk as a single non-directory entry and report an empty library
with no error. A **dangling** link anywhere in a configured path is a
startup error naming both the link and its target, checked before
`MkdirAll`: `MkdirAll` also fails on one, but its message names only the
link, never the missing target, which is the whole question when a volume
did not mount. `brokenLink` walks every component because the link is as
likely to be the mount point as the directory under it; a component that
is merely absent is `MkdirAll`'s job. `DB_PATH` cannot be resolved before
the first run (the file does not exist yet), so only its directory is.

Inside the library, **a symlinked directory is not followed**, and is
reported rather than passed over. `WalkDir` delivers it as a non-directory
entry with no supported suffix, which the ordinary filter would drop in
silence; the scanner instead logs it at Warn with its target and counts it
in `Result.Errors`, on every sweep, as pressure toward a bind mount.
Following it would need a `(dev, ino)` cycle guard, and a link pointing
outside the library would index files whose relative `file_path` cannot
say where they are.

The branch turns on `os.Stat`, and it has three answers, not two. A link
resolving to a directory is the refusal above. A link whose `Stat` fails
with anything but `fs.ErrNotExist` (`ELOOP`, `EACCES`) is reported the same
way: an unknown is not evidence, and nothing about the name says which of
the other two it would have been. Only `ErrNotExist` or a link to a file
takes the ordinary route, so a symlinked *file* is indexed like any other
(stat, open and hash all go through the link) and a dangling one is a
per-file error only if its name carries a supported suffix.

The cost of not following: a real directory later replaced by a link is
never re-indexed, and `reconcileMissing`'s `Lstat` follows it (only the
leaf is not resolved), so where the target holds the files the rows stay
live and are never re-hashed, and where it does not they are marked and
never pruned, since that directory yielded no files (see below).

## Covers and regeneration

A recorded cover whose file is missing or zero bytes is re-extracted from
the book and its path refreshed, which is what makes `COVERS_DIR`
disposable. Any other stat failure warns without re-parsing the source.

A cover a **provider** supplied has no original in the book to rebuild
from, so the sweep must not fail to re-extract it forever. The ordering is
the rule: `readEmbeddedCover` runs first, and `field_sources` is consulted
only once re-extraction has come back empty. A book can carry an embedded
cover *and* a provider row (`cover.Store` failed at first sight, leaving
`cover_retry` set and no `cover` row; enrichment then supplied one), so
"there is a provider row" never means "there is nothing to re-extract". A
re-extracted cover goes through `UpdateBookCoverPath`, which drops the
`cover` row in the same transaction, since the image is now the scanner's.

Only when the book yields nothing is the cover forgotten
(`storage.ClearProviderCover`): `cover_path` emptied, the row removed, the
book back in enrichment's missing set so the Fetch button repairs it, and
the grid showing an honest empty box rather than an `<img>` pointing at a
file that is gone. **The provider test is that a `field_sources` row for
`cover` exists**, not that it names a provider rather than `embedded`: a
scanner-extracted cover has no provenance row at all, so a string compare
against `embedded` matches nothing while reading as correct.

Three ambiguities resolve the same way, **an unknown is not evidence**: a
failed provenance read, a failed `readEmbeddedCover`, and a stat failing
with anything but `fs.ErrNotExist` (`coverFileDefinitelyGone`) each leave
the book untouched. The read-error case matters most, because clearing
there is permanent: an empty `cover_path` returns at
`maybeRegenerateCover`'s first guard on every later sweep, so the embedded
original would never be recovered even once the read works again.

Two empty states are kept apart. An empty `cover_path` with no marker
records that no usable embedded cover exists and is not retried.
`cover_retry` records a *transient* store failure and is retried on later
sweeps, skipping the stat check while set. `createBook` splits
`cover.Store`'s error on `cover.ErrUnsupportedCover`: an I/O failure sets
the marker, a cover that cannot decode (SVG, BMP, corrupt, over the caps)
leaves both empty with an Info line. A decode failure fails identically
forever, and a marker there would re-open and fully re-parse the book on
every sweep to Warn the same way again. `maybeRegenerateCover`'s own
`Store` call gets the same split through `storage.RecordUnusableCover`,
which also removes a stale provider row beside an undecodable embedded
original. `recordUnusableCover` refuses to write while a stored cover path
is present unless `coverFileDefinitelyGone` confirms it: with the marker
set the stat was skipped, so it has no evidence about that file.

Both forgetting writes are guarded on the `cover_path` this sweep observed.
The scanner decides from a snapshot, then stats, parses a whole book and
reads provenance before the write lands, and an enrichment run finishing
inside that window would otherwise have its fresh cover thrown away.

## Missing files

A row whose path is gone is reconciled in two phases: first *marked*
(`missing_since`, via `SetFilesMissing`), then deleted
(`PruneMissingFiles`, taking the book if it was the last location) once it
has stayed missing past `MISSING_GRACE`. A row seen again before then has
its mark cleared. `PruneMissingFiles` does no filtering of its own; every
guard lives in the scanner, the only place with live filesystem state, and
decides the id list.

The guards, each against reading a transient failure as a deletion:

- A row is eligible only if `os.Lstat` fails with `fs.ErrNotExist`
  specifically. Any other error only warns. The check runs fresh every
  sweep, including for a row already marked, so a row whose failure mode
  changes (`ErrNotExist` to `EACCES`, or a directory now at that path) is
  never deleted on a confirmation that has gone stale.
- A row under a directory the sweep could not read *this* sweep is left
  untouched at both mark and prune time. `Scan` tracks these as
  `skippedDirs`, a negative list, because `WalkDir` only ever reports a
  directory-read failure as a second, error-bearing callback; a positive
  "cleanly read" list is not obtainable from the API.
- A row whose **top-level directory yielded no book files this sweep** is
  marked but never pruned, counted in `Result.Unconfirmed` and named, with
  a per-directory row count, in one Warn per sweep. That is what an
  offline sub-mount looks like (a second bind mount, an NFS share in a
  subfolder, a disk mid-rebuild): the directory reads cleanly, so nothing
  lands in `skippedDirs`, and every row under it fails `Lstat` with
  `ErrNotExist`, the exact signal the two phases trust. Without this,
  past `MISSING_GRACE` a weekend rebuild would prune every book on that
  disk, edits, provenance and enrichment results included, the one thing
  the scanner destroys that it cannot rebuild.
- Reconciliation is skipped entirely when the sweep visited zero files,
  logged at Warn. An unmounted volume can present as an empty directory,
  so seeing nothing is not evidence that everything is gone.

The row is still *marked* in the third case because the mark is
reversible, it puts the "missing" annotation on the detail page, and it
keeps `internal/sender` off the path: `resolveFile` takes the first
location with a `NULL` `missing_since` and fails the job on a stat error
without trying the next copy, so an unmarked dead row that sorts first
fails every send of its book.

The test is the row's top-level directory, not its own. A file under a
nested directory is under its top-level ancestor too, so emptying
`a/novels/` while `a/` keeps files still prunes, where a disk mounted at
`a/` going offline leaves nothing under `a/` at any depth. Two limits
follow. Only a mount directly under the library root is protected:
`mnt/disk2` offline beside a populated `mnt/disk1` is pruned after grace.
And a root-level row has no top-level directory, so it is covered by the
zero-files guard alone; a root-level file is not a mount shape. Storing
`st_dev` per row would be the precise test, and is not done because it
adds the very column the mover section below argues against.

The accepted cost: the last book file deleted from a top-level directory
stays marked missing, a phantom card annotated "missing", until that
directory gains a book again. The most ordinary way to pay it is a renamed
top-level folder: every book under it gains a live row and keeps its old
one marked for good, with the Warn firing on every sweep. A phantom card is
recoverable where a pruned book's edits are not. The "forget this location"
affordance that would be the honest fix is planned in
`docs/plans/2026091001-library-changes-underneath-the-index.md`.

## Watcher

`internal/scanner/watcher.go` is a *trigger*, not a second index path. It
never reads, hashes or parses the file an event names; it pokes a
capacity-1 channel that `cmd/server`'s one scan goroutine selects on
beside its ticker.

Events are debounced (`WATCH_SETTLE`, default 5s) because an event says
something changed, not that it finished changing: a copy fires `CREATE`
long before its last byte lands. One timer covers both bounds: the settle
window handles a burst that ends, and `watchMaxDelay` (60s) pokes on the
first event past the cap so a bulk import cannot hold the debounce open
forever. The debounce is a quality measure, not a correctness one: a sweep
that catches a partial file indexes it, and the completing write's changed
size and mtime make the next sweep re-hash and orphan-prune it in one
transaction.

Only events worth a sweep qualify: a supported suffix, any remove or rename
(the name may already be gone, and a book leaving is what reconciliation
wants), or a new directory. A `.part` file growing during a download
provokes nothing until it is renamed into place.

`Refresh` re-registers the watch set after every sweep, because inotify is
not recursive (a new subdirectory needs its own watch), a watch is dropped
silently when its directory is deleted, and a directory that moves out of
the library leaves its descendants' watches live but filed under the names
they had inside it, reporting files created outside the library as though
they had arrived in it. That third case is why `Refresh` compares each
directory's `(dev, ino)` against a `registered` map *and* checks
`WatchList()` membership; neither test is sufficient alone, since a
filesystem may hand a recreated directory the inode number the deleted one
had (ext4 does), which reads as unchanged for a watch the kernel has
already dropped. Together they are exact, because an inode number only
becomes reusable once its inode is freed and freeing it drops the watch.

A superseded watch is released explicitly before the re-add. fsnotify's
`updatePath` drops the old descriptor from its own map, enough to stop
delivery, but never calls `inotify_rm_watch`, so the kernel keeps a watch
nothing can reach and one slot of a per-user budget is spent per
replacement. Removing first is safe because inotify allocates descriptors
cyclically, so the queued `IN_IGNORED` cannot land on the fresh watch.

`Refresh` cannot recover from losing *every* watch, when the library
directory itself is unmounted and remounted: a sweep only calls it after a
poke, and a watcher with no watches can never poke again. So `Run` carries
a `watchRecheckInterval` (30s) ticker that rebuilds the set once
`WatchList()` is empty (a length check while healthy, a walk only when
there is nothing left to lose) and pokes a sweep when it succeeds, since
whatever changed while it was deaf is still unindexed.

`WATCH_ENABLED=false` leaves exactly the ticker-only behaviour, which is
what a mount whose delivery probe reports silence wants. A watcher that
fails to start is a Warn, not a failed startup.

## Mounts and the mover

Two startup checks report what the watcher can actually see, because
neither is observable later. `mountFor` names the filesystem backing
`LIBRARY_DIR` from `/proc/self/mountinfo` (absent on macOS: a Debug line)
and warns for the types where changes routinely happen *behind* the mount
rather than through it (`fuse.*`, `nfs`, `cifs`, `9p`). On a FUSE
passthrough a file written through the mount produces `CREATE`+`WRITE`;
one written directly into the backing store produces nothing, though a
later `readdir` sees it. So an SMB copy to an Unraid `/mnt/user` share is
seen and the mover shuffling between `/mnt/cache` and `/mnt/diskN` is not.
Bind-mounting the disk path makes every change local.

Since `fsnotify.Add` on such a mount returns no error, a dead watch is
indistinguishable from an idle one, so a delivery probe creates a file in
the library root and waits for its own event; silence is one Warn. The
file is made with `os.CreateTemp`, never at a name derived from the pid:
`os.Create` would truncate whatever sits at that path and follow a symlink
to its target, and in a container the process is pid 1, so a derived name
is guessable. A diagnostic must not be able to destroy a book. The probe's
name is excluded from `qualifies` so it cannot trigger the work it tests,
and only that one file is removed, including on the timeout path. A
read-only library is an Info-level skip.

The mover needs no handling, and that is worth keeping true: it preserves
path, size and mtime, so the cheap check skips those files, and the inode
and physical disk it does change are not things this index stores. That is
the reason not to add inode tracking or a device-id column.
