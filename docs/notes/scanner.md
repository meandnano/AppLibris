# Scanner

Rules for `internal/scanner` and for the scan loop, the startup mount
checks and the directory resolution in `cmd/server`.

## Sweep and identity

- **`Scan` is the one sweep, woken by the `SCAN_INTERVAL` ticker or a
  watcher poke, and two sweeps never overlap.** The rescan is the
  mechanism and the watcher only buys latency, so correctness never
  depends on an event arriving.
- **The startup sweep runs in the background.** The server answers
  `/healthz` while a large library is still indexing.
- **A file whose path, size and mtime match its `book_files` row is
  skipped; only a mismatch pays for the hash and the parse.** That is what
  keeps a rescan of a large library fast.
- **Identity is the content hash; the path is an attribute. Known content
  at a new path is a second `book_files` row, never a new book.** A moved
  file and a duplicate copy look the same from any one path, and one book
  for both carries edits through a reorganisation. The grid flags several
  locations and never merges or deletes.
- **`file_path` is relative to `LIBRARY_DIR` and slash-separated.** The
  index survives the library mounted at another absolute path.
- **`scanFile` reads by content hash on the read pool and inserts later;
  when two callers both insert, the loser re-reads and attaches its path
  to the winner's book.** `books.content_hash` is UNIQUE, and this is what
  makes a sweep and an import converge on one book.
- **New content at a known path reassigns the row and, in the same
  transaction, deletes a book left with no locations, logged at Info.**
  `ReassignFileAndPruneOrphan` and `CreateBookWithFile` both reassign
  unconditionally, so either can orphan the previous owner.
- **A same-path replacement inherits the old book's `manual` fields, their
  provenance and its `send_log` rows (`inheritFromReplacedBookTx`).
  Reassignment across paths never inherits.** A rewrite of the same book
  and a different book at the same name are indistinguishable from one
  path, and no title guard separates them, since a write-back is exactly
  the case where the title just changed. An inherited wrong value is
  editable where a lost one is gone.
- **Only what a person authored moves: provider values are not inherited,
  `enrichment_jobs` cascade, the new file's cover is extracted afresh, and
  an empty `manual` value is inherited like any other.** A Fetch recreates
  a provider's value, and a cleared field stays `manual`. The inherited
  fields are named at Info, since `manual` renders no marker on the page.
- **Supported names are matched on suffix (`MatchedSuffix`), not
  `filepath.Ext`, and `.fb2` and `.fb2.zip` both record `format` as `fb2`
  (`BookFormat`).** `.fb2.zip` is two extensions, and packaging is not
  something the format badge surfaces.
- **`Scan` checks `ctx.Err()` before the walk and on every entry.** A
  shutdown stops the walk instead of failing each remaining database call.
  `cmd/server` runs the loop on `scanCtx` and waits for it
  (`waitForBackground`) after the HTTP server stops and before the
  database closes.
- **A directory the walk cannot read costs its subtree and counts an
  error; it does not abort the sweep.**

## Embedded metadata

- **`capMetadata` bounds embedded metadata in `createBook` to what the
  editor accepts: every field but description is collapsed onto one line,
  then truncated on a rune boundary through `storage.CapField`, the author
  list is cut at `storage.MaxAuthors`, and each cut is logged at Info. The
  file is never rejected and the field stays `embedded`.** A value the
  editor refuses must never be stored, or it cannot be saved unchanged
  again. Provenance says where a value came from, not whether it arrived
  whole.
- **The collapse runs before the cut.** Cutting first would let a
  truncation boundary decide whether a break survives. The collapse is the
  same `strings.Fields` join as `internal/enrich`'s `sanitizeValue`,
  because `normalizeField` rejects a line break in every field but
  description and a pretty-printed XML element reaches the parser with its
  break intact.
- **Description keeps its line breaks; `storage.CapField` caps its blank
  runs and trims the edges instead of collapsing it.** A description is
  the one field whose breaks carry meaning, and the cap bounds a run
  neither parser can hand over.

## Paths and symlinks

- **`LIBRARY_DIR` must exist and may be read-only: `requireExistingDir`
  stats it, never creates it, and an absent one fails startup naming
  `LIBRARY_DIR`.** Creating it is the one call that turns a read-only
  mount into a startup failure, and creating it with a warning hides a
  wrong volume under an empty grid.
- **`COVERS_DIR` and `DB_PATH`'s directory are created (`resolveDir`), and
  a refused `MkdirAll` names both the uid the process runs as and the owner
  of the nearest existing ancestor, as an absolute path.** `permission
  denied` alone names neither side of the mismatch, and a relative
  `COVERS_DIR` would otherwise name an ancestor such as `data` that nobody
  wrote. `ownerUID` is build-tagged `unix`, not `linux`, because the test
  asserting the message runs on non-Linux machines.
- **Both `requireExistingDir` and `resolveDir` finish with `EvalSymlinks`.** `filepath.WalkDir` `Lstat`s
  its root and never follows a link, so an unresolved symlinked root
  reports an empty library with no error. Only `DB_PATH`'s directory is
  resolved, because the file does not exist before the first run.
- **A dangling link in any component of a configured path fails startup
  naming link and target. `brokenLink` runs before both resolution halves
  and walks every component; a merely absent component is not a broken
  link.** `MkdirAll` fails on a dangling link too but names only the link,
  and the missing target is the whole question when a volume did not
  mount. The link is as likely to be the mount point as the directory
  under it.
- **Inside the library a symlinked directory is not followed; it is logged
  at Warn with its target and counted in `Result.Errors` on every sweep.
  Symlinked files are indexed like any other.** `WalkDir` delivers the
  link as a non-directory entry the suffix filter would drop in silence.
  Following it needs a cycle guard, and a target outside the library has
  no relative `file_path`.
- **The branch turns on `os.Stat` with three answers: a directory is
  refused, a `Stat` error other than `fs.ErrNotExist` is reported the same
  way, and only `ErrNotExist` or a file takes the ordinary route.** An
  unknown is not evidence. A dangling link is a per-file error only under
  a supported suffix.
- **Rows under an unfollowed link (`linkedDirs`) are marked missing and
  never pruned.** `reconcileMissing`'s `Lstat` resolves through the link
  and succeeds, so without the guard the grace period deletes a book whose
  file is readable. The mark is true: the walk does not name that path.

## Covers and regeneration

- **A recorded cover whose file is missing or zero bytes is re-extracted
  and its path refreshed; any other stat failure warns without
  re-parsing.** That is what makes `COVERS_DIR` disposable.
- **`readEmbeddedCover` runs first; `field_sources` is consulted only after
  re-extraction returns empty.** A book can hold an embedded cover and a
  provider row at once, when `cover.Store` failed at first sight and
  enrichment then supplied one. A re-extracted cover goes through
  `UpdateBookCoverPath`, which drops the `cover` row in the same
  transaction.
- **Only a book that yields nothing has its cover forgotten
  (`storage.ClearProviderCover`), and the provider test is that a
  `field_sources` row for `cover` exists, never a comparison against
  `embedded`.** A scanner-extracted cover has no provenance row, so a
  string compare matches nothing while reading as correct. Forgetting
  returns the book to enrichment's missing set.
- **An unknown is not evidence: a failed provenance read, a failed
  `readEmbeddedCover` and a stat failing with anything but `fs.ErrNotExist`
  (`coverFileDefinitelyGone`) each leave the book untouched.** Clearing is
  permanent, since an empty `cover_path` returns at
  `maybeRegenerateCover`'s first guard on every later sweep.
- **`cover_retry` marks a transient store failure only; it is retried on
  later sweeps and skips the stat while set. A decode failure
  (`cover.ErrUnsupportedCover`) is recorded as no cover, an empty
  `cover_path` with no marker, and never retried.** A decode failure fails
  identically forever, and a marker there would re-parse the book on every
  sweep. `createBook` splits `cover.Store`'s error this way, and
  `maybeRegenerateCover` gets the same split through
  `storage.RecordUnusableCover`, which also removes a stale provider row.
- **`recordUnusableCover` refuses to write while a stored cover path is
  present unless `coverFileDefinitelyGone` confirms it.** With the marker
  set the stat was skipped, so the sweep has no evidence about that file.
- **Both forgetting writes are guarded on the `cover_path` this sweep
  observed.** The sweep decides from a snapshot and then stats, parses and
  reads provenance before the write lands; an enrichment run finishing
  inside that window would otherwise lose its fresh cover.

## Missing files

- **A gone path is reconciled in two phases: marked (`SetFilesMissing`,
  `missing_since`), then deleted (`PruneMissingFiles`, taking a book with
  no other location) once missing past `MISSING_GRACE`. A row seen again
  has its mark cleared.** Marking is reversible where pruning is not, so an
  unmounted disk does not delete edits.
- **`PruneMissingFiles` filters nothing; every guard lives in the scanner
  and decides the id list.** The scanner is the only place with live
  filesystem state.
- **A row is eligible when `os.Lstat` fails with `fs.ErrNotExist`
  specifically or succeeds; any other error only warns. The check runs
  fresh every sweep, marked rows included.** A row whose failure mode
  changes is never deleted on a stale confirmation.
- **A successful `Lstat` on a row the walk did not see is not an unknown;
  it is marked like an absence.** The walk read that directory cleanly and
  did not report that spelling, so the row is a case-only alias or a file
  that arrived between walk and check, which the next sweep clears.
  Marking beats a case-insensitive comparison against `seen` because it
  needs no rule for which spelling is canonical.
- **A row under a directory the sweep could not read this sweep is left
  untouched at both phases (`skippedDirs`).** It is a negative list because
  `WalkDir` reports a read failure only as a second, error-bearing
  callback; a positive "cleanly read" list is not obtainable.
- **A row whose top-level directory yielded no book files this sweep is
  marked but never pruned, counted in `Result.Unconfirmed` and per
  directory in `Result.UnconfirmedDirs`.** That is what an offline
  sub-mount looks like: the directory reads cleanly and every row under it
  fails `Lstat` with `ErrNotExist`, so without this a weekend rebuild
  prunes a disk's edits and provenance.
- **Such a row is still marked.** The mark annotates the page and keeps
  `internal/sender` off the path: `resolveFile` takes the first location
  with a `NULL` `missing_since` and fails on a stat error without trying
  the next.
- **The test is the row's top-level directory, not its own. Only a mount
  directly under the library root is protected, and a root-level row is
  covered by the zero-files guard alone.** A nested directory is under its
  top-level ancestor too, so emptying `a/novels/` while `a/` keeps files
  still prunes. A device id per row would be the precise test and is the
  column the mover rule forbids.
- **The accepted cost: the last book file deleted from a top-level
  directory stays a phantom location marked missing until the directory
  gains a book again or a person forgets the row.** A phantom card is
  recoverable where a pruned book's edits are not.
- **Reconciliation is skipped entirely, at Warn, when the sweep visited
  zero files.** An unmounted volume can present as an empty directory.
- **Forgetting (`storage.ForgetMissingFile`) is offered only beside a
  location currently marked missing, one location at a time, and its
  guards are clauses on the `DELETE`, never a read before it.** A sweep
  clearing the mark between a read and the delete would forget a path that
  just came back, and forgetting is not reversible. `PruneMissingFiles` is
  not reused because it verifies nothing and a click has confirmed no
  absence. A row matching neither condition answers `(false, false, nil)`.
  One at a time because two missing rows for two reasons is the state a
  person opens the list to disambiguate.
- **`cmd/server` logs one line per unconfirmed directory: Warn the first
  sweep it appears, Info while it stays (`unconfirmedLevel`, over a set
  `periodicScan` carries between iterations). A sweep that reconciled
  nothing leaves the set alone.** Repeating a warning a person can clear
  on every sweep is how a log stops being read. The set is a loop
  variable, not a column, so `Scan` stays stateless. A sweep that reported
  on no directory must not re-Warn about everything next time.
- **`TopLevelDirHasBooks` is the exported one-path form of the same rule,
  for `internal/sender`.** See `docs/notes/sending.md`.

## Indexing one path

- **`IndexFile` is `scanFile` with a fresh `Result`, returning the book id
  the path now belongs to and whether it created one. There is no second
  way into the index; every guard a new book needs lives in
  `createBook`.** A second path would drift from the first.
- **`internal/importer` calls it in the request that copied the file,
  which is safe because `scanFile` is idempotent.** A later sweep sees
  matching path, size and mtime and does nothing; a racing one converges
  on the same content hash.
- **`MatchedSuffix`, `BookFormat` and `ExtractMetadata` are exported
  because the importer's preview must say what a file is, what its format
  is called and what it holds exactly as a sweep would.**
  `ExtractMetadata` takes the fallback title and returns the parse error,
  the two things the callers differ on: a sweep names the walked path, an
  import names the browser's filename.
- **A `.part` file and `.applibris-write-probe` are invisible to a sweep by
  suffix. Nothing else may acquire a supported suffix before it is
  whole.** `.part` is what an import writes before publishing, and the
  probe is what `cmd/server` creates and removes to learn whether the
  library is writable. See `docs/notes/import.md`.

## Watcher

- **`watcher.go` is a trigger, not a second index path: it never reads,
  hashes or parses a file, and pokes a capacity-1 channel the one scan
  goroutine selects on beside its ticker.** It is reached through
  `watchSet` so tests can drive its debounce.
- **Events are debounced over `WATCH_SETTLE` with one timer, and
  `watchMaxDelay` (60s) pokes on the first event past the cap.** An event
  says something changed, not that it finished changing, and a bulk import
  must not hold the debounce open forever. The debounce is quality, not
  correctness: a partial file indexed now is re-hashed by the next sweep.
- **Only a supported suffix, any remove or rename, or a new directory
  qualifies.** A removed name may already be gone and a book leaving is
  what reconciliation wants; a `.part` growing during a download provokes
  nothing until it is renamed into place.
- **`Refresh` re-registers the watch set after every sweep, comparing each
  directory's `(dev, ino)` against `registered` and checking `WatchList()`
  membership; neither test alone suffices.** inotify is not recursive,
  drops a watch silently when its directory is deleted, and a directory
  moved out of the library keeps its descendants' watches under their old
  names. A filesystem may reuse a deleted directory's inode number, which
  reads as unchanged for a watch the kernel has dropped.
- **A superseded watch is removed before it is re-added.** fsnotify's own
  replacement drops the descriptor from its map but never calls
  `inotify_rm_watch`, spending one slot of the per-user budget per
  replacement. inotify allocates descriptors cyclically, so the queued
  `IN_IGNORED` cannot land on the fresh watch.
- **`Run` carries a `watchRecheckInterval` (30s) ticker that rebuilds the
  set once `WatchList()` is empty and pokes a sweep when it succeeds.**
  `Refresh` runs only after a poke, and a watcher with no watches can never
  poke again.
- **`WATCH_ENABLED=false` is exactly the ticker-only behaviour, and a
  watcher that fails to start is a Warn, not a failed startup.**

## Mounts and the mover

- **`mountFor` names the filesystem backing `LIBRARY_DIR` from
  `/proc/self/mountinfo` and warns for `fuse.*`, `nfs`, `nfs4`, `cifs`, `smb3` and `9p`;
  an absent mountinfo is a Debug line.** On those filesystems changes
  routinely happen behind the mount and produce no event, and nothing
  later than startup can observe that.
- **A delivery probe creates a file in the library root and waits for its
  own event; silence is one Warn, and a read-only library is an Info
  skip.** `fsnotify.Add` on such a mount returns no error, so a dead watch
  is indistinguishable from an idle one.
- **The probe file is made with `os.CreateTemp`, never at a pid-derived
  name; its name is excluded from `qualifies`; only that one file is
  removed, on the timeout path too.** `os.Create` truncates whatever sits
  at the path and follows a symlink, and pid 1 in a container makes a
  derived name guessable. A diagnostic must not be able to destroy a book
  or trigger the work it tests.
- **Do not add inode or device-id tracking.** The mover preserves path,
  size and mtime, so the cheap check skips its files; the inode and disk
  it changes are not stored, and that is the whole of its invisibility.
