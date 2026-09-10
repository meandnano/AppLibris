# Plan: the library changes underneath the index

## Position in the sequence

Independent of the enrichment and first-run plans written beside it. It
replaces three backlog items, deleted in the same change, and adds one
finding made while re-validating them (Decision 5):
`docs/backlog/2026090901-forget-missing-location.md`,
`docs/backlog/2026090714-in-place-rewrite-loses-edits.md` and
`docs/backlog/2026090902-unmounted-library-reads-as-file-gone.md`. Touches
`internal/storage`, `internal/scanner`, `internal/sender`, `internal/service`,
`internal/web` and `cmd/server`.

## Context

Three ordinary things happen to a library directory after the index has
learned it, and the index handles each one worse than it needs to.

**A top-level folder is renamed.** `reconcileMissing`
(`internal/scanner/scanner.go:270-340`) refuses to prune a `book_files` row
whose top-level directory yielded no book files this sweep, because that is
what an offline sub-mount looks like. Rename `Fiction/` to `Novels/` and
every book inside gains a live `Moved` row and keeps its old row marked
missing; `Fiction/` never regains a book, so the refusal repeats on every
sweep for the life of the deployment. Every one of those books shows
"2 paths" on the grid and a dead path annotated "missing" on the detail
page, and `directories yielded no files, refusing to prune their rows`
fires at Warn every fifteen minutes and on every filesystem event. Nothing
in the UI can say "that path is gone for good"; the only remedies are
recreating the old name with a book in it and waiting out `MISSING_GRACE`,
or editing `book_files` by hand.

**A file is rewritten in place.** Content hash is identity. When a known
path's content changes, `scanFile` finds no book for the new hash and calls
`CreateBookWithFile` (`internal/storage/books.go:566`), which reassigns the
path's row to the new book and, via `pruneOrphanIfEmptyTx`, deletes the old
book in the same transaction once it has no locations left. `book_authors`,
`field_sources` and `enrichment_jobs` cascade; `send_log.book_id` goes
`NULL`. Every manual edit and every provenance row on the old book is gone.
That is right for a different book placed at the same filename and wrong
for Calibre's metadata write-back, `ebook-polish`, `kepubify`, and a sync
tool re-downloading a re-zipped file, all of which rewrite a file without
changing which book it is. From one path's perspective the two are
indistinguishable.

**A volume under the library goes away.** `internal/sender.failFileError`
(`internal/sender/sender.go:287`) maps `fs.ErrNotExist` alone to "the file
is no longer in the library" and every other error to "could not read the
file — try again". A mount that errors (`ESTALE`, `EIO`) gets the honest
sentence. A mount whose mountpoint now reads as an empty directory, the
more ordinary shape, fails every stat under it with `ENOENT`, so every send
of every book on that volume records the false statement, and the history
page keeps it for a month. The scanner already treats exactly this shape as
an unknown rather than evidence; the sender sees one path and one errno.

**A folder or file is renamed by case only.** On the case-insensitive
filesystems an SMB share and macOS present, renaming `Books/` to `books/`
walks `books/x.epub` (a new path, known content, so a `Moved` row) and then
`os.Lstat`s the unseen `Books/x.epub` row (`internal/scanner/scanner.go:293`),
which **succeeds**, because the filesystem matches the old spelling to the
new name. The row is neither marked nor cleared. The book keeps two live
rows for good, "2 paths" on every card, no annotation on the detail page
and no log line, and `internal/sender` will happily send from the stale
spelling. The backlog described this as the same residue as a real rename;
it is a quieter and less recoverable one, since Decision 1 only offers to
forget a row that is marked.

## Scope

In scope:

- A "forget" affordance on the detail page's locations list, for a row
  currently marked missing.
- The unconfirmed-directory Warn dropping to Info once it repeats.
- Same-path replacement carrying `manual` fields, their provenance and the
  book's send history onto the replacement book.
- The sender applying the scanner's top-level-directory test before it
  believes an `ENOENT`.
- An unseen row whose `Lstat` succeeds being marked missing rather than
  left alone.

Out of scope, with reasons:

- **Forgetting a live location.** A path that is there is not something to
  forget; deleting its row would only make the next sweep re-add it.
- **A title-similarity heuristic for inheritance.** See Decision 3.
- **Inheriting provider-sourced fields.** A Fetch recreates them from the
  same catalogues, and a provider's guess about the old file is not a fact
  about the new one.
- **Inheritance across paths.** Two different files with different hashes
  are two books; the duplicate and mover story depends on that.
- **Storing `st_dev` per row** to make the offline-volume test exact. The
  index deliberately stores neither inode nor device (`docs/notes/scanner.md`,
  Mounts and the mover), and the top-level rule already gives the guarantee.

## Decision 1: forgetting a location is a `POST` under the book, for a missing row only

`POST /books/{id}/locations/forget` with one form field, `file`, the
`book_files` id. It is registered beside the other state-changing routes in
`internal/web/web.go:38-43` and wrapped in `sameSiteOnly` like every one of
them. The detail page renders a small "forget" button beside each location
whose `Missing` is true, inside the existing `<details class="locations">`
list in `book.html:32-38`, as a `<form>` carrying both `action` and
`hx-post`, the one-markup-path rule every other control follows.

The check that the row belongs to this book and is currently marked
missing is made **inside the delete's own transaction**, not in a read
before it. `PruneMissingFiles` (`books.go:821`) deletes exactly the ids it
is given and verifies nothing, by design; reusing it would put the
membership and missing checks in a separate read, and a sweep clearing the
mark between that read and the delete would forget a path that has just
come back. So storage gains one method, `ForgetMissingFile(ctx, bookID,
fileID) (forgotten, bookDeleted bool, err error)`: a single `DELETE FROM
book_files WHERE id = ? AND book_id = ? AND missing_since IS NOT NULL
RETURNING book_id`, followed by `pruneOrphanedBookTx` for that book, in one
`DB.Write`. A row that does not match returns `(false, false, nil)`: a
double click, or a row a sweep has since cleared, is a slip and not an
error, the same contract `DeleteRecipient` holds.

Forgetting a book's last location prunes the book through the same
`pruneOrphanedBookTx` every other path uses, so a bookless book cannot be
created by accident. The response then has nowhere to go: a non-htmx
request is redirected `303` to `/`, and an htmx request gets `HX-Redirect:
/`, since a fragment cannot be swapped into a page whose subject no longer
exists. Otherwise a non-htmx request is redirected `303` back to the book,
and an htmx request gets the re-rendered locations fragment. That needs the
locations `<dd>` to become a swap target with a stable id (`#locations`)
and the list to move into a named partial, `book-locations`, that the full
page and the fragment both render, the `send-control` pattern.

`service.FileLocation` (`internal/service/service.go:278`) gains `ID int64`
so the template has something to put in the form. `service.ForgetLocation
(ctx, bookID, fileID)` is a thin pass-through returning storage's two
bools; the handler branches on them.

The rejected alternative is a "forget all missing locations of this book"
button. It is one click fewer for the renamed-folder case and wrong for a
book with two missing rows for two different reasons, which is exactly the
state a person is looking at the list to disambiguate.

## Decision 2: the unconfirmed-directory Warn is a Warn once, then Info

The Warn exists to point at residue nothing else surfaces. Once Decision 1
gives a person a way to clear it, a directory that has been unconfirmed for
forty consecutive sweeps is not news, and a log that repeats itself every
fifteen minutes is a log nobody reads.

`Scan` stays stateless. `Result` gains `UnconfirmedDirs map[string]int`
(the per-directory row counts `reconcileMissing` already computes into its
local `unconfirmed` map), and the log line moves out of `reconcileMissing`
and into `cmd/server.runScan`, which is the one caller and already emits
the per-sweep summary. `periodicScan` keeps a `map[string]bool` of the
directories the previous sweep reported, across iterations of its loop. A
directory absent from that set logs at Warn with its row count; one present
logs at Info. The set is replaced with this sweep's after logging, so a
directory that recovers and later empties again is Warned again.

The memory is a loop variable in the process, not a column: it costs
nothing to lose on restart (the first sweep after a restart Warns once,
which is right), and it is invisible to `Scan`'s tests, which keep
asserting `Result.Unconfirmed` and now `Result.UnconfirmedDirs`.

The rejected alternative, a `Scanner` struct carrying the set, would make
`Scan` a method and change every call in the tests and in `cmd/server` to
add state that one caller wants for one log line.

## Decision 3: a same-path replacement inherits `manual` fields, provenance and send history

In `CreateBookWithFile`, after `upsertBookFileTx` has moved the path and
before `pruneOrphanIfEmptyTx` deletes the previous owner, when the previous
owner is a different book that now has zero locations:

- Every `field_sources` row of the old book whose `source` is `manual` is
  applied to the new book: for the six scalar fields, the old book's column
  value is written through `updateBookColumnTx` and `setFieldSourceTx(…,
  "manual")`; for `authors`, the old book's author names in position order
  are written through `updateBookAuthorsTx(…, "manual", …)`. The
  `modified_at` stamp is `b.ModifiedAt`, the instant the scanner already
  stamps on the new row.
- An **empty** `manual` value is inherited too. A cleared field stays
  `manual` (`CLAUDE.md`, Storage invariants); a person who cleared a wrong
  publisher does not want the file's wrong publisher back.
- `send_log` rows pointing at the old book are re-pointed to the new one
  (`UPDATE send_log SET book_id = ? WHERE book_id = ?`) before the delete,
  so the detail page's status box and the "did I already send this?" answer
  survive the rewrite. Today they go `NULL` and the history page keeps the
  denormalised title, which is the right fallback for a pruned book and the
  wrong outcome for one that is still on the shelf under a new hash.
- `enrichment_jobs` still cascade. A pending intention about the old
  content is meaningless for the new.
- `syncBookFTSTx` runs once, at the end, after inheritance has settled the
  title, description, ISBN and authors.

Provider-sourced fields and the `cover` row are not inherited (Scope). The
new file's embedded cover is extracted as for any new book; the old cover
file, named by the old content hash, is left for the covers directory to
carry, as today.

**No similarity heuristic.** The obvious guard is "inherit only when the
new file's embedded title matches the old book's", and it is wrong in both
directions: Calibre write-back is exactly the case where the title in the
file has just changed to match the edit, and a re-zipped download has the
same title as before. The population that hits this path is
overwhelmingly the same book rewritten; a different book dropped at
precisely the same filename is rare, and when it happens the inherited
edits are visible on the detail page with an edit affordance beside each,
where a lost edit is gone. Empty is recoverable; wrong is editable; lost is
neither.

`CreateBookWithFile` returns `inherited []MetadataField` beside the
orphaned id and title, so `scanner.logOrphan` can say at Info which fields
moved. The rule never applies across paths: `ReassignFileAndPruneOrphan`
(known content at a new path) does not inherit, since the book being
orphaned there is a different book that happened to lose its last copy.

## Decision 4: the sender asks the scanner's question before believing `ENOENT`

`internal/scanner` exports one helper:

```go
// TopLevelDirHasBooks reports whether the top-level directory under
// libraryDir that relPath sits in exists and holds at least one supported
// book file at any depth. A root-level relPath has no such directory and
// reports true.
func TopLevelDirHasBooks(libraryDir, relPath string) (bool, error)
```

It uses `topLevelDir` and `matchedSuffix` and walks with `filepath.WalkDir`,
returning through a sentinel `fs.SkipAll` at the first match, so on a
populated directory it costs a handful of stats. A missing directory is
`(false, nil)`; any other walk error is returned, since an unknown is not
evidence.

`internal/sender` imports `internal/scanner` (which imports `cover`, `epub`,
`fb2`, `storage`, none of which import `sender`; no cycle). `resolveFile`
returns the relative path alongside the absolute one, and `failFileError`
takes it: on `fs.ErrNotExist` it calls `TopLevelDirHasBooks`; `true` keeps
`fileGoneReason`, `false` or an error records `fileUnreadableReason` and
logs at Warn naming the directory, since that is what an offline volume
looks like and "try again" is the sentence that is true of it. Root-level
files keep `fileGoneReason`, as the scanner's own rule does.

The rejected alternative is a second copy of the top-level rule inside
`internal/sender`. Two copies of a rule about the same directory drift, and
the scanner's is the one with tests.

## Decision 5: an unseen row whose `Lstat` succeeds is marked, not left alone

`reconcileMissing` today treats a successful `Lstat` on an unseen row as
"couldn't tell" and leaves the row untouched. It is the wrong reading. The
walk visited that row's directory cleanly (rows under `skippedDirs` were
already excluded) and did not report that exact byte sequence as a name, so
a successful `Lstat` means one of two things: the filesystem matched the
recorded spelling to a file the walk recorded under another one, which is
the case-only rename above; or the file appeared between the walk and this
check, which the next sweep sees and clears. Neither is a reason to keep
the row live under its recorded spelling.

So the branch changes from `continue` to the same treatment an
`ErrNotExist` gets: the row is marked if unmarked, and a marked row is
eligible for pruning under exactly the rules of the paragraph below it,
the top-level guard included. That guard is what keeps this safe in the one
place a successful `Lstat` is legitimately ambiguous: a real directory later
replaced by a symlink, whose files the scanner declines to follow, resolves
every component but the leaf and so passes `Lstat`. Under this decision its
rows are now marked, which puts the honest annotation on the detail page,
and they are never pruned, because that top-level directory yielded no
files. A case-only rename of a top-level folder lands in the same state and
Decision 1's affordance is what clears it; a case-only rename of a nested
folder or a single file has a populated top-level directory and prunes
after `MISSING_GRACE`, which is right, since the surviving row is the same
file under its real name.

A non-`ErrNotExist` error from `Lstat` keeps its Warn and its `continue`:
an error is still an unknown, and this decision is only about success.

The rejected alternative is a case-insensitive comparison of the row's path
against the walk's `seen` set. It identifies the case-alias shape precisely
but says nothing about the between-walk-and-check file, needs a rule for
which spelling is canonical, and adds a second normalisation of paths to a
package whose identity story is "the path is what the walk said". Marking
is reversible and needs no such rule.

## Changes

- `internal/storage/books.go`: `ForgetMissingFile`; `CreateBookWithFile`
  gains the inheritance block and the `inherited` return; a small
  `manualFieldSourcesTx(ctx, tx, bookID)` reader and a
  `repointSendLogTx(ctx, tx, from, to)` writer beside it.
- `internal/storage/metadata.go`: no signature changes; `updateBookColumnTx`
  and `updateBookAuthorsTx` are reused as they are.
- `internal/scanner/scanner.go`: `Result.UnconfirmedDirs`; the Warn line
  removed from `reconcileMissing`; the successful-`Lstat` branch falls
  through to marking instead of `continue`; `logOrphan` names inherited
  fields; `TopLevelDirHasBooks` exported.
- `cmd/server/main.go`: `runScan` logs unconfirmed directories at Warn or
  Info against a set `periodicScan` keeps between sweeps.
- `internal/sender/sender.go`: `resolveFile` returns the relative path;
  `failFileError` consults `scanner.TopLevelDirHasBooks`.
- `internal/service/service.go`: `FileLocation.ID`; `ForgetLocation`.
- `internal/web/web.go`: the route, wrapped in `sameSiteOnly`.
- `internal/web/book.go` (or a new `locations.go`): `forgetLocationHandler`
  with the `303`/fragment/`HX-Redirect` split.
- `internal/web/templates/book.html`, `partials.html`: the `book-locations`
  partial with `id="locations"`, the per-row forget form.
- `internal/web/static/css/app.css`: a `.locations__forget` rule in the
  `.button--tertiary` idiom, sized `--sm` if `2026090702`'s restyle has
  landed and otherwise inline.

## Tests

`internal/storage`:

- `TestForgetMissingFileDeletesOnlyAMarkedRowOfThatBook`: a marked row is
  forgotten; an unmarked row of the same book, and a marked row of another
  book, return `(false, false, nil)` and stay.
- `TestForgetMissingFileLastLocationPrunesBook`: the book row, its
  `book_authors` and `field_sources` are gone; `send_log.book_id` is `NULL`
  and `book_title` retained.
- `TestCreateBookWithFileInheritsManualFields`: old book with a manual
  title, a manual empty publisher and manual authors; new content at the
  same path; the new book carries all three values, each with source
  `manual`, and the FTS row finds the inherited title.
- `TestCreateBookWithFileDoesNotInheritEmbeddedOrProviderFields`: an
  `embedded` description and an `openlibrary` language stay the new file's
  own.
- `TestCreateBookWithFileRepointsSendLog`: a delivered send for the old
  book now names the new book's id.
- `TestReassignFileAndPruneOrphanDoesNotInherit`: known content at a new
  path orphaning a book with manual edits leaves the surviving book
  untouched.
- `TestCreateBookWithFileNoInheritanceWhenPreviousOwnerKeepsALocation`: a
  two-location book losing one path passes nothing on.

`internal/scanner`:

- `TestTopLevelDirHasBooks`: populated directory, empty directory, absent
  directory, directory with only sidecars, root-level path, unreadable
  directory (error returned).
- `TestReconcileMissingReportsUnconfirmedDirs`: `Result.UnconfirmedDirs`
  carries the per-directory counts the removed log line used to; the
  existing `Unconfirmed` assertions keep passing.
- `TestScanInPlaceRewriteKeepsManualEdits`: end to end through `Scan`, a
  file rewritten with different bytes keeps its manual title.
- `TestReconcileMarksUnseenRowWhoseLstatSucceeds`: a `book_files` row whose
  path names an existing *directory* (which the walk never records as
  seen, so `Lstat` succeeds on every filesystem) is marked on the first
  sweep and pruned after grace when its top-level directory holds a book;
  a second case with the directory empty stays marked and unpruned.
- `TestReconcileLstatErrorStillLeavesRowAlone`: a row whose `Lstat` fails
  with `EACCES` (a parent directory made unreadable) is neither marked nor
  pruned, pinning that Decision 5 is about success only.
- `TestScanCaseOnlyRenameMarksOldSpelling`: rename `Books/` to `books/`
  in the fixture; skipped with `t.Skip` when a probe shows the temp
  filesystem is case-sensitive, since the branch is then reached through
  `ErrNotExist` and the previous test already covers it.

`cmd/server`:

- `TestUnconfirmedDirLogLevel`: a table test over a pure function
  `unconfirmedLevel(prev map[string]bool, dir string) slog.Level`, Warn
  when new, Info when repeated.

`internal/sender`:

- `TestSendUnderEmptyTopLevelDirRecordsUnreadable`: the book's only row
  sits under `vol/`, the directory exists and is empty; the job fails with
  `fileUnreadableReason`.
- `TestSendUnderPopulatedTopLevelDirRecordsGone`: a sibling book file is
  present; the job fails with `fileGoneReason`.
- `TestSendRootLevelMissingFileRecordsGone`.

`internal/web`:

- `TestForgetLocationRoutePostOnlyAndSameSite`: `GET` is 405; a
  cross-site `POST` is refused.
- `TestForgetLocationFragmentRendersLocations`: htmx `POST` returns the
  `book-locations` fragment with one row fewer.
- `TestForgetLocationRedirectsWhenBookPruned`: last location; `303` to `/`
  without htmx, `HX-Redirect: /` with it.
- `TestForgetButtonOnlyBesideMissingRows`: the full page renders the form
  for a marked row and not for a live one.
- The button-class guard in `web_test.go` keeps passing with the new class.

## docs/notes/scanner.md

- *Sweep and identity*: add that a same-path replacement carries the old
  book's `manual` fields, their provenance and its send history onto the
  new book inside the reassigning transaction, with the reasoning of
  Decision 3, and that reassignment across paths never inherits.
- *Missing files*: replace the sentences describing the residue of a
  renamed folder as permanent with: the row stays until a person forgets it
  from the detail page, and the per-directory Warn fires once and then
  repeats at Info. State the rule for what the forget affordance is offered
  on and why the check is inside the delete's transaction.
- *Missing files*: add that `TopLevelDirHasBooks` is the exported form of
  the top-level rule and that `internal/sender` is its second caller.
- *Missing files*: replace the sentence that a successful `Lstat` leaves a
  row untouched with Decision 5's rule and its reason: the walk is the
  authority on names, so a successful `Lstat` on an unseen row is a
  spelling mismatch, not an unknown. Fold the symlinked-directory paragraph
  into it, since its rows are now marked and protected by the top-level
  guard rather than silently live.

## docs/notes/storage.md

- *Schema*: `ForgetMissingFile` beside `PruneMissingFiles`, with the reason
  the check is atomic with the delete.
- *Provenance*: the inheritance rule, including that an empty `manual`
  value is inherited.
- *Recipients and send log*: `send_log.book_id` is re-pointed on a same-path
  replacement, and goes `NULL` only when the book is actually pruned.

## docs/notes/sending.md

- *The queue worker*: `failFileError`'s three reasons gain the fourth
  condition, an `ENOENT` under an empty or absent top-level directory
  recording `fileUnreadableReason`, and why the sender borrows the
  scanner's rule rather than copying it.

## docs/notes/web.md

- *Book detail and editing*: the forget route, its `sameSiteOnly` wrapper,
  the `#locations` swap target, and the `HX-Redirect` when the book is
  pruned.

## CLAUDE.md

- Code map, `internal/web`: add `POST /books/{id}/locations/forget`.
- Scanner invariants: the "An unknown is not evidence" bullet gains "a
  successful `Lstat` on an unseen row is not an unknown: the walk did not
  see that spelling, so the row is marked"; the top-level prune refusal
  sentence gains "until a person forgets the row"; add "A same-path replacement inherits `manual`
  fields, provenance and `send_log` rows; reassignment across paths never
  does."
- Sending invariants: the `fs.ErrNotExist` sentence gains "under a
  populated top-level directory".

## README.md

- *Features*, the "Keeps up with the directory" bullet: one clause that a
  path that is gone for good can be forgotten from the book's page, and
  that rewriting a file in place keeps your edits.

## Verification

- Rename a top-level folder under a running server. After the sweep every
  book shows "2 paths"; the log Warns once and then reports at Info. Forget
  the dead path on one book: the badge drops, the row is gone, the Warn
  count drops by one on the next sweep.
- Forget the only location of a book whose file was deleted: the browser
  lands on the library grid and the book is gone.
- Edit a title by hand, then rewrite the file with Calibre (or re-zip the
  EPUB). After the sweep the title is still the edited one, its marker is
  absent (manual renders nothing), and the send status box still shows the
  last send.
- On an SMB share or a macOS checkout, rename a folder by case only. After
  the sweep every book under it shows one live and one missing row, the
  missing one under the old spelling; forget it and the badge drops.
- Unmount a volume under the library so its mountpoint is an empty
  directory, then send a book on it. The status box reads "could not read
  the file — try again", and the retry succeeds once the volume is back.
