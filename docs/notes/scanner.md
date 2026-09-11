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

**Embedded metadata is bounded where it is extracted** (`capMetadata`), to
the same rules a person's edit and a provider's answer meet — one set of
constants in `internal/storage`, below all three writers. Scalars are
truncated on a UTF-8 boundary and the author list is cut at
`storage.MaxAuthors`, with an Info line naming the path and the field: a
verbose file is worth knowing about and is not an error. Truncated, never
rejected, because a book whose description is too long is still a book and a
filename title is worse than prose cut at 64 KiB. A cut field is still
`embedded` in `field_sources`: provenance says where a value came from, not
whether it arrived whole.

Length is not the only thing the editor refuses. `normalizeField` rejects a
line break in every field but description, and a metadata element whose
text is wrapped across two lines in the source XML — legal, and what a
generator that pretty-prints produces — reaches the parsers with the break
intact, since `TrimSpace` removes only what sits at either end. So every
field but description is collapsed onto one line first, the same
`strings.Fields` join `internal/enrich`'s `sanitizeValue` applies to a
provider's answer, and description takes the edge trim alone, which is the
rest of what `normalizeField` would hand back. Both run before the length
cut: each can only shorten the value, and cutting first would let a
truncation boundary decide whether a break survives. A description's blank
lines are capped by whichever parser produced it, through
`storage.CapBlankLines`, rather than a third time here.

The point of both is that a value the editor refuses is never stored. A
10 MB `<dc:description>` in the column is a description that can no longer
be saved unchanged; a wrapped title is one an `<input type="text">` silently
rewrites on submit, flipping its provenance to `manual`, and a wrapped
author name is one the textarea's Save splits into two authors. Nothing
re-derives either afterwards.

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

**A same-path replacement carries the old book's `manual` fields, their
provenance and its `send_log` rows onto the new one**, in that same
transaction (`inheritFromReplacedBookTx`). One path sees two different
events as the same state — the same book rewritten (Calibre's metadata
write-back, `ebook-polish`, `kepubify`, a re-zipped re-download) and a
different book dropped at the same filename — and no test tells them apart.
A title-similarity guard is wrong in both directions: write-back is
precisely the case where the file's title has just changed to match the
edit, and a re-download's has not changed at all. The population reaching
this path is overwhelmingly the same book, and the asymmetry decides it:
an inherited value is visible on the detail page with an edit affordance
beside it, where a lost one is gone. Empty is recoverable, wrong is
editable, lost is neither.

Only what a person is the author of moves. A provider-sourced value is
recreated by a Fetch from the same catalogues, and a provider's guess about
the old file is not a fact about the new one; `enrichment_jobs` cascade,
since a pending intention about the old content is meaningless for the new;
the new file's embedded cover is extracted as for any other new book. An
*empty* `manual` value is inherited like any other, because a cleared field
stays `manual`. The fields that moved are named at Info beside the orphan
line, since a value on a book whose file was just rewritten is otherwise
unexplained — `manual` renders no marker, so the page cannot say it.

Reassignment across paths never inherits. The book `ReassignFileAndPruneOrphan`
can orphan is a different book that happened to lose its last copy, not
this one under new bytes.

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
so every consumer is handed one root that means the same thing whether or
not links are followed, and the two halves resolve differently.

**The library must exist and may be read-only** (`requireExistingDir`): it
is stat'd, never created, and an absent one is a startup error naming
`LIBRARY_DIR`. Creating it is the single call that would turn a
legitimately read-only mount into a startup failure — the scanner only
reads the library, and the watcher's delivery probe already treats an
unwritable root as an Info-level skip. Creating it and warning would hide
the misconfiguration under an empty grid, which is exactly what
`LIBRARY_DIR` pointing at the wrong volume already looks like, leaving the
log as the only place the mistake shows.

**The covers and database directories are created** (`resolveDir`), and a
permission failure names both uids: the uid the process runs as and the
owner of the nearest *existing* ancestor, since the target is what
`MkdirAll` could not make. `mkdir /data/covers: permission denied` names
neither side of the mismatch it reports, and both are needed to fix it —
the container runs as whatever uid it was given while a NAS bind mount is
owned by the share's user, an Unraid one by `nobody`, and a fresh named
volume by root. The ancestor is named as an absolute path: a relative
`COVERS_DIR`, which is what the development target passes, leaves the
ancestor of `./data/covers` reading back as `data`, which is neither the
path the person wrote nor one they can go and look at. The
owner comes from the platform's stat struct, so `ownerUID` is build-tagged
`unix` — not `linux`, though the image is: `syscall.Stat_t` carries `Uid` on
every unix, and the test asserting the message runs on the development
machine and on non-Linux CI runners, where a narrower tag would take the
fallback and fail — and reports nothing elsewhere.

`LIBRARY_DIR` is the resolution that matters most:
`filepath.WalkDir` `Lstat`s its root and never follows a link, so a
symlinked root (`~/Books -> /volume1/books`, the ordinary NAS shape) would
otherwise walk as a single non-directory entry and report an empty library
with no error. A **dangling** link anywhere in a configured path is a
startup error naming both the link and its target, and `brokenLink` runs
in front of both resolution halves: `MkdirAll` fails on a dangling link
too, but its message names only the link and never the missing target,
which is the whole question when a volume did not mount. It walks every
component, because the link is as likely to be the mount point as the
directory under it, and a merely absent component is not a broken link —
that one is `MkdirAll`'s job on the writable half and the `LIBRARY_DIR`
error on the other. `DB_PATH` cannot be resolved before the first run (the
file does not exist yet), so only its directory is.

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
never re-indexed. `reconcileMissing`'s `Lstat` follows the link (only the
leaf is not resolved), so those rows pass it and are marked missing, which
is the honest annotation for a path the walk no longer names. They are
never pruned: the walk records every unfollowed link in `linkedDirs`, and
a row under one is refused whatever its mark's age (see Missing files),
which is what keeps a book whose file is still readable through the link
from being deleted after the grace period.

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

The rules deciding eligibility, most of them guards against reading a
transient failure as a deletion:

- A row is eligible when `os.Lstat` either fails with `fs.ErrNotExist`
  specifically or succeeds. Any other error only warns. The check runs
  fresh every sweep, including for a row already marked, so a row whose
  failure mode changes (`ErrNotExist` to `EACCES`, say) is never deleted on
  a confirmation that has gone stale.
- The one rule running the other way: a **successful** `Lstat` on an unseen
  row is not an unknown, and is treated exactly as an absence — marked, and
  prunable under the guards below. The walk is the authority on names — it
  read that directory
  cleanly and did not report that exact byte sequence — so a success means
  either the filesystem matched the recorded spelling to a file the walk
  recorded under another one, the case-only rename an SMB share or macOS
  allows, or the file arrived between the walk and the check, which the
  next sweep clears if the row is still inside its grace period. Leaving
  the row live under a spelling the
  walk disagrees with is what a case-only rename would otherwise cost for
  good: two live rows, "2 paths" on every card, no annotation, no log line,
  and `internal/sender` free to send from the stale spelling. Marking is
  reversible, which is why it beats a case-insensitive comparison against
  the walk's `seen` set: that identifies the alias precisely, says nothing
  about the between-walk-and-check file, and needs a rule for which
  spelling is canonical in a package whose identity story is "the path is
  what the walk said".
- A row under a directory the sweep could not read *this* sweep is left
  untouched at both mark and prune time. `Scan` tracks these as
  `skippedDirs`, a negative list, because `WalkDir` only ever reports a
  directory-read failure as a second, error-bearing callback; a positive
  "cleanly read" list is not obtainable from the API.
- A row under a directory the walk declined to follow because it is a
  symlink (`linkedDirs`) is marked but never pruned. That directory yielded
  no files at any depth, exactly as an offline sub-mount does, and the
  top-level test below misses it wherever the link sits under a directory
  that still holds books. Its rows' `Lstat` resolves through the link and
  succeeds, so without this the rule above would mark them and the grace
  period would then delete a book whose file is present and readable. They
  are marked rather than left alone, since the annotation is true: the walk
  does not name that path any more.
- A row whose **top-level directory yielded no book files this sweep** is
  marked but never pruned, counted in `Result.Unconfirmed` and broken down
  per directory in `Result.UnconfirmedDirs`. That is what an
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
stays marked missing, a phantom location annotated "missing", until that
directory gains a book again or a person forgets the row. The most ordinary
way to pay it is a renamed top-level folder, where every book under it
gains a live row and keeps its old one marked. A phantom card is
recoverable where a pruned book's edits are not.

Forgetting is `POST /books/{id}/locations/forget` over
`storage.ForgetMissingFile`, offered on the detail page beside a location
**that is currently marked missing and no other**. A path that is there is
not something to forget; deleting its row would only make the next sweep
re-add it. The same rule is a condition on the `DELETE` rather than a read
taken before it, together with the row belonging to that book, because a
sweep clearing the mark inside the window between such a read and the
delete would forget a path that had just come back, and forgetting is not
reversible. `PruneMissingFiles` is not reused for the same reason: it
verifies nothing by design, on the understanding that its caller confirmed
each absence with a live `Lstat` this sweep, which a click has not. A row
matching neither condition is `(false, false, nil)` — a double click is a
slip, not an error. It is one location at a time rather than "forget all of
this book's missing locations", which is one click fewer for the renamed
folder and wrong for a book with two missing rows for two different
reasons, the state a person opens the list to disambiguate.

`cmd/server` logs one line per unconfirmed directory with its row count,
at Warn the first sweep that directory appears and Info while it stays
there (`unconfirmedLevel`, against a set `periodicScan` carries between
iterations). The Warn exists to point at residue nothing else surfaces;
since a person can clear it, repeating the same warning every fifteen
minutes for the life of a renamed folder is how a log stops being read.
The set is a loop variable, not a column: losing it on a restart Warns once
more, which is the right thing to say to someone who has just started the
server, and it keeps `Scan` stateless and its tests indifferent. A sweep
that reconciled nothing — an apparently empty library — reported on no
directory at all, so it leaves the set alone rather than replacing it with
its own emptiness and re-Warning about everything next time.

`TopLevelDirHasBooks` is the exported, one-path-at-a-time form of the same
rule, for a caller with no walk of its own; `internal/sender` is its second
caller (`sending.md`).

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
