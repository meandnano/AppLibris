# Step: import a book by uploading it

## Position in the sequence

First of three steps that together let a person add a book through the web
UI without touching the library directory by hand. This step builds the
whole import machinery and drives it from a plain file input. The two that
follow reuse it unchanged:

- **URL import** (next plan): `importer.Download` fetches a public URL into
  the same `Stage`, under the same size cap plus a configurable deadline,
  with the private-address and redirect guards the cover fetcher already
  has. Adds the URL form and the in-progress indicator to the import page.
- **Drag and drop** (after that): an inline script on the grid and detail
  pages turns the window into a drop target that submits the same form.

A programmatic API is deferred and is not part of the sequence. Its one
prerequisite is recorded in Decision 7 so the plan that eventually builds
it does not rediscover it.

## Context

Today a book enters the library only by a file appearing under
`LIBRARY_DIR`, which the README tells people to mount read-only. Getting a
freshly bought EPUB from a laptop into the library means a file share, an
SSH copy or a NAS upload page, then waiting for the scanner. The brainstorm
that produced this plan settled the shape:

- Imports land in the **library root** under the file's original name.
  The app writes into the same flat pile a person manages by hand, so the
  `:ro` deployment advice becomes "read-only works, import is then
  disabled".
- **Preview, then confirm.** A dropped file is parsed where it was staged
  and its title, authors, format, size, cover and any duplicate verdict are
  shown before anything is written into the library.
- A byte-identical duplicate is **refused with a link** to the existing
  book. Copying it would only add a second location to the same book.
- Staged files live under **`os.TempDir()/applibris-imports`**, separate
  from `/data`, redirected with the standard `TMPDIR` when needed, and are
  wiped at startup.
- The size cap is **configurable, 64 MiB by default**.
- The file is indexed **in the confirming request** through the scanner's
  own per-file path, so the response can redirect to the new book.

Two facts about the code make the last point cheap. `scanFile` is already
idempotent: a sweep over a path the import just indexed sees a matching
path, size and mtime and does nothing, and if both race they key on the
same content hash and converge on one book with one location. And every
guard a new book needs — `capMetadata`, the cover store split on
`ErrUnsupportedCover`, the orphan logging — lives in `createBook`, which
`scanFile` calls. Importing needs no second way into the index.

## Scope

In scope: `internal/importer`; `scanner.IndexFile`; the import service
methods; `GET /import` with a file input, the preview fragment and its
three verdicts, confirm and discard; the writability probe;
`MAX_IMPORT_SIZE`; the "Import" nav link; README, CLAUDE.md and a new
`docs/notes/import.md`.

Out of scope, with reasons:

- **URL import and drag and drop.** Own steps, above.
- **Near-duplicate detection beyond a title match.** The preview flags a
  staged file whose title equals an existing book's, case-insensitively on
  `sort_title`, and stops there. Author and ISBN comparison is the
  suggestion feature `docs/notes/design.md` defers.
- **Deleting a book from the UI.** Discard removes a staged file only. A
  confirmed import is a library file like any other and is removed the way
  any other is.
- **Cleaning up a `.part` file left by a crash mid-copy.** See Decision 5.
- **Editing metadata in the preview.** The detail page already does that,
  one field at a time, and the redirect lands there.

## Decision 1: `internal/importer` is its own package

The importer knows about temp files, format sniffing, the size cap, the
library directory and the scanner. `internal/service` knows about none of
those and should not start to: it is the layer a second transport calls,
and the storage, formats and scanner packages stay beneath it. The
service grows three thin methods that hand a reader or an id to the
importer and shape the result for the transport, the same relation it has
to `internal/sender` and `internal/enrich`.

`Stager` is constructed in `cmd/server` with the library directory, the
covers directory, the size cap, the database and a clock, and handed to
`service.New` through a new option, since `New` today takes only the
database. Nil means import is not configured, the same convention as
`Notify`.

## Decision 2: staged imports are in memory, on a temp file, and expire

A staged import is an id, a temp path, the parsed preview, the content
hash, the verdict and a created-at instant, in a map behind a mutex. There
is no table: a stage nobody has confirmed after a restart is a file nobody
asked for, and wiping the directory at startup is the whole recovery.

Expiry is a fixed 30 minutes from creation, checked by a janitor goroutine
on the scan context every minute, and rechecked on confirm so a click on a
stale tab gets "this import has expired, stage it again" rather than a
move of a file the janitor deleted a moment ago. Thirty minutes is long
enough to read a preview and short enough that a forgotten tab does not
hold 64 MiB for a day. Not configurable: nothing about a deployment
changes how long a person takes to press a button.

The id is 128 random bits, base32 without padding, so a stage cannot be
guessed from another tab. Same-site is still enforced on every POST; the
id is not a credential, it is what keeps two people's stages apart.

## Decision 3: format by content, extension by format

The client's filename and `Content-Type` are hints and nothing more: a
browser sends `application/octet-stream` for an FB2 and a person renames
files. The importer sniffs:

- `PK\x03\x04` and a zip whose entries include `META-INF/container.xml` is
  `.epub`;
- a zip whose entries include exactly one name ending in `.fb2` and no
  `META-INF/container.xml` is `.fb2.zip`;
- a body beginning, after an optional BOM and whitespace, with `<?xml` or
  `<FictionBook` is `.fb2`;
- anything else is refused as "not an EPUB or FB2 file".

The temp file is then renamed to `<id><suffix>`, because `epub.ReadMetadata`
and `fb2.ReadMetadata` both take a path and decide by suffix, and the
final library name gets the same suffix regardless of what the upload was
called. A file called `book.epub` that is really an FB2 is written as
`book.fb2`, and the preview says so.

Sniffing reads the zip central directory through `archive/zip` on the
staged file, so it costs one open and no inflation. The parse that follows
is the same one the scanner runs, with the same caps, so a hostile archive
is bounded exactly as it would be arriving through the directory.

## Decision 4: the verdicts

`Stage` looks the content hash up before it returns, and the preview
carries one of three verdicts:

- **`New`.** Import is offered.
- **`Exists`**, with the existing book's id and title. Only Discard is
  offered, beside a link to the book. The temp file is deleted at once,
  since there is nothing to confirm; the record stays until discarded or
  expired so the page can still render.
- **`TitleMatch`**, with the id and title of one existing book whose
  `sort_title` equals the staged title's. Import is offered under a warning
  line. Different editions of one book are a legitimate thing to own, and
  the person is the only one who knows which this is.

The check is a snapshot: a sweep can index the same bytes between preview
and confirm. Confirm therefore does not trust the verdict. It calls
`IndexFile`, and if the book it returns already had another location, the
copy just written is a second location of a known book, which the detail
page shows as "2 paths". That is the correct outcome for the race and
needs no special case, but the redirect lands on the existing book rather
than a new one, and the log line says so at Info.

## Decision 5: confirm writes the library in an order that cannot half-fail

1. **Name.** Sanitise the original basename: take `filepath.Base`, strip
   control characters and `/`, `\`, `:` and a leading `.`, collapse
   whitespace, cut on a rune boundary to 200 bytes, replace the extension
   with the sniffed suffix, and fall back to the staged title or the id
   when nothing survives. If `LIBRARY_DIR/<name>` exists, append ` (2)`,
   ` (3)` and so on before the suffix until it does not. Existence is
   tested with `O_EXCL` on the `.part` path below, so two confirms cannot
   pick the same name.
2. **Copy.** Write to `LIBRARY_DIR/<name>.part` with `O_CREATE|O_EXCL`,
   `io.Copy` from the temp file, `Sync`, close, then `Rename` to `<name>`.
   Neither the scanner nor the watcher acts on a `.part` suffix, so a
   half-written file is never indexed. The copy is a copy and not a
   rename from temp because `/tmp` and `/library` are different
   filesystems in every deployment that matters.
3. **Index.** `scanner.IndexFile(ctx, db, libraryDir, path, coversDir)`.
   On error the file **stays in the library** and the response says
   "imported, but indexing failed; the next scan will pick it up", at
   Warn in the log. Deleting it would discard the bytes over a database
   error, and the next sweep is the recovery the scanner already promises.
   The stage record is removed either way, since the library now owns the
   file.
4. **Clean up.** Delete the temp file and the record.

A crash between steps 2 and 3 leaves a `<name>.part` in the library.
Startup does not remove it: it cannot know the file is ours rather than a
download a person is running into the same directory, and `.part` is the
suffix downloaders use. The note documents it as the one manual tidy-up.

Confirm on an id that is mid-confirm or already confirmed answers the same
redirect as the first call, so a double click is a slip and not an error.
The record carries the resulting book id once known for exactly that.

## Decision 6: writability is probed once, at startup

`cmd/server` creates and removes `LIBRARY_DIR/.applibris-write-probe` after
resolving the directory. On failure import is disabled for the run, at
Warn, naming the uid it runs as and the directory's owner through the
existing `mkdirError` shape, and `importEnabled` is threaded into
`web.Routes` beside `sendEnabled` and `enrichEnabled`. The routes stay
registered so a stale tab gets "the library is read-only, import is
disabled" rather than a 404, and the nav link is not rendered.

A probe rather than a permission-bit check because a read-only mount, an
ACL and a uid mismatch all fail the same way at the same call and none of
them shows in the mode bits. Once rather than per request because the
answer does not change while the process runs, and a confirm that fails
anyway reports its own error.

The probe file begins with `.` and carries no supported suffix, so a sweep
that overlaps it ignores it.

## Decision 7: the API stays deferred, and why it is not free

`design.md`'s deferred list gains the specific obstacle: every
state-changing route refuses a request without `Sec-Fetch-Site`, and a
non-browser client never sends one, so an API needs its own credential
(a bearer token from an `API_TOKEN` variable) and its routes must bypass
`sameSiteOnly` on the strength of it. The service methods this step adds
take a reader and return a stage, or take an id and return a book, so a
one-shot API import is `Stage` then `Confirm` in one handler.

## Decision 8: the upload route extends its own read deadline

`cmd/server`'s `http.Server` sets `ReadTimeout: 30s`, and that timeout
covers the body. Sixty-four megabytes over Wi-Fi to a NAS routinely takes
longer. The upload handler calls
`http.NewResponseController(w).SetReadDeadline` with a deadline sized from
the cap at a floor of 1 MiB/s, so the global timeout is untouched for
every other route and a stalled upload still ends. The body is wrapped in
`http.MaxBytesReader` at the cap plus form overhead, and the importer's
own cap is the second guard, since the multipart part is what it actually
counts.

## Changes

- `cmd/server/main.go`: parse `MAX_IMPORT_SIZE` (a byte count with an
  optional `K`, `M`, `G` or `Ki`, `Mi`, `Gi` suffix; refuse zero or
  negative); the write probe; construct `importer.New`; wipe and create
  the temp directory; start the janitor on `scanCtx`; thread
  `importEnabled` into `web.Routes`; log the cap and the temp directory on
  the listening line.
- `internal/importer/importer.go`: `Stager`, `Staged`, `Verdict`, `New`,
  `Stage`, `Confirm`, `Discard`, `Get`, `RunJanitor`, `ErrTooLarge`,
  `ErrUnsupportedFormat`, `ErrExpired`, `ErrLibraryNotWritable`.
- `internal/importer/sniff.go`: `detectSuffix(path) (string, error)`.
- `internal/importer/name.go`: `libraryName(original, title, suffix
  string) string` and the collision loop.
- `internal/scanner/scanner.go`: `IndexFile`, exported, wrapping
  `scanFile` with a fresh `Result` and returning the book id the path now
  belongs to; `scanFile` gains the id in its return so the wrapper does
  not query again. The `.part` and leading-`.` exclusions already hold by
  suffix and need no change, but the test below pins them.

  **Correction, found while implementing.** This closes an import loop the
  plan did not anticipate: `internal/importer` imports `internal/scanner`
  and `internal/service` imports `internal/importer`, so
  `internal/scanner`'s in-package test files may no longer import
  `internal/service` — and `capmetadata_test.go` does, to assert that
  nothing `capValue` stores is a value `normalizeField` would then refuse.

  Three fixes were weighed. Handing the importer an index function instead
  of letting it call the scanner makes a hard dependency nilable to serve a
  test. Keeping the importer out of `internal/service` contradicts Decision
  1. Duplicating the fixtures into a second test file leaves two copies of
  `writeTestEPUBWithOPF` to drift.

  What was done instead is the ordinary Go answer: `capmetadata_test.go`
  became `package scanner_test`, and `internal/scanner/export_test.go`
  re-exports the four in-package identifiers it needs (`capValue`,
  `openTestDB`, `writeTestEPUBWithOPF`, `testMissingGrace`) so both suites
  still share one set of fixtures.
- `internal/service/import.go`: `StageImport(ctx, name, r) (*ImportPreview,
  error)`, `ConfirmImport(ctx, id) (bookID int64, indexed bool, err
  error)`, `DiscardImport(ctx, id) error`, `ImportPreview(ctx, id)`;
  `ImportPreview` shaped like `BookDetail`'s header fields plus `Verdict`,
  `ExistingID`, `ExistingTitle`, `Size`, `Format`, `CoverURL`.

  **Correction, found while implementing.** `ImportPreview` cannot be both
  the type and the method: Go has one identifier space per package. The
  reader is named `StagedImport(ctx, id) (*ImportPreview, error)` and the
  type keeps the name, since the type is what the transport and any later
  transport both spell.

  `CoverURL` moved with it, to `HasCover bool`. A staged book has no entry
  in `COVERS_DIR` and must not acquire one before anybody has said to keep
  it, so its cover is served from the stage by a route this layer does not
  name — and `BookDetail` already carries `CoverPath` and lets
  `internal/web`'s `coverURL` compose the URL, which is the split this
  follows. A third method, `StagedCover(id) []byte`, hands that route the
  bytes.
- `internal/service/service.go`: `New` takes functional options;
  `WithImporter(*importer.Stager)`.
- `internal/web/import.go`: `GET /import`, `POST /import/file`,
  `GET /import/{id}`, `POST /import/{id}/confirm`,
  `POST /import/{id}/discard`, `GET /import/{id}/cover`; every POST inside
  `sameSiteOnly`; fragment versus full page through the existing
  `wantsFragment` split with `Vary` set; failure sentences composed in
  the handler (`importFailureLine`).
- `internal/web/templates/import.html` and partials: the page, the
  `import-preview` fragment with its three verdict blocks, the
  `import-form` fragment the discard answers. The file input's form
  carries both `action` and `hx-post`, `hx-encoding="multipart/form-data"`
  and `hx-indicator` to a status line reading "Uploading…" through
  `.htmx-request`. The Import button disables itself through the same
  class.
- `internal/web/web.go`: the nav gains "Import" when `importEnabled`;
  `Routes` takes the flag.
- `internal/web/static/css/app.css`: the import page, preview card and
  indicator, on the existing tokens and button variants.
- `README.md`: Features gains the import bullet; "Which user to run as"
  says write access to `/library` enables import and read-only disables
  it, and drops the `:ro` from the example; Configuration gains
  `MAX_IMPORT_SIZE`; a short "Importing a book" subsection under Running
  it names the temp directory and `TMPDIR`.
- `CLAUDE.md`: Code map gains `internal/importer` and the import routes;
  Invariants gains an Import section (below); Storage's "no backfill"
  and Scanner sections unchanged.
- `docs/notes/import.md`: new, carrying the reasoning above in present
  tense; `docs/notes/design.md`: the library-directory section says the
  app writes exactly one kind of path into the library and how; the
  deferred list's API entry gains Decision 7's obstacle.

Invariants for CLAUDE.md, as rules:

- Format is decided by content in `detectSuffix`; the client's filename
  and `Content-Type` never choose the parser or the suffix written.
- Confirm never trusts the preview's verdict; `IndexFile`'s answer is the
  truth, and an `Exists` outcome at confirm is a second location, not an
  error.
- A failed `IndexFile` leaves the library file in place. Never delete a
  file the library already holds over an index error.
- The library is written only as `<name>.part` then `Rename`; nothing
  creates a supported suffix in the library directly.
- Staged state is in memory and on `os.TempDir()`; nothing about a stage
  is written to the database, and startup wipes the temp directory.
- The write probe runs once at startup; a per-request check would answer
  differently only when confirm is about to report its own error.

## Tests

- `internal/importer`:
  - `detectSuffix` over hand-built fixtures: a minimal EPUB zip, a zip
    with one `.fb2` entry, a plain FB2 with and without BOM, a zip with
    neither, a PDF header, an empty file.
  - `Stage` refuses at exactly cap + 1 bytes with `ErrTooLarge` and
    leaves no file; accepts exactly cap bytes.
  - `Stage` on a hash the database knows answers `Exists` and has deleted
    the temp file; on a matching `sort_title` answers `TitleMatch`.
  - `Confirm` writes `<name>` and no `.part` remains; the book is in the
    index with `file_path` equal to the name; a second `Confirm` on the
    same id returns the same book id.
  - Name collision: an existing `Dune.epub` yields `Dune (2).epub`, and a
    concurrent pair of confirms of different bytes with the same original
    name get two different files.
  - Sanitisation table: `../etc/passwd`, `C:\books\x.epub`, a name of only
    dots, a 300-byte Cyrillic name cut on a rune boundary, `book.epub`
    that sniffs as FB2 becoming `book.fb2`.
  - Expiry through an injected clock: a stage is gone after 30 minutes,
    its file with it, and `Confirm` on it returns `ErrExpired`.
  - `IndexFile` failing (an injected closed database) leaves the library
    file and returns `indexed == false` with the error.
- `internal/scanner`: `IndexFile` on a fresh path creates a book; on a
  path a sweep already indexed returns the same id and counts
  `Unchanged`; a `.part` file and a `.applibris-write-probe` in the
  library are not indexed by `Scan`.
- `internal/web`: each verdict renders its block and only its buttons; the
  failure sentence for too large, unsupported, expired, disabled; a
  fragment request gets the fragment and a full-page request the page,
  both with `Vary`; every POST refuses a cross-site `Sec-Fetch-Site`;
  confirm answers `HX-Redirect` to a fragment request and 303 otherwise;
  the nav link is absent when import is disabled and `GET /import` then
  renders the explanation.
- `cmd/server`: the write probe against a `0o555` directory disables
  import and logs the owner line; against a writable one leaves no file
  behind. `MAX_IMPORT_SIZE` parsing table, including `64MiB`, `100M`,
  `0`, `-1`, `abc`.

## Verification

- Run against `./library` with `make run`, open `/import`, choose an EPUB
  from the desktop, confirm the preview shows its cover and metadata,
  press Import and land on the new book's page with one location.
- Import the same file again and see the existing-book verdict with the
  link and no Import button.
- Import a different EPUB of a book already in the library and see the
  title warning above an Import button.
- Rename an FB2 to `.epub` on the desktop and import it: the preview says
  FB2 and the library file ends in `.fb2`.
- Import a file over the cap and see the sentence naming the cap.
- With JavaScript disabled, do the first flow again: full pages, a 303 at
  the end.
- `chmod 555 ./library`, start, see the Warn line and the disabled page;
  `chmod 755` and restart, see it enabled.
- Watch the scan log after a confirm: the next sweep reports the file as
  `Unchanged`, never `New`.
- `ls /tmp/applibris-imports` after a discard and after a restart with a
  stage pending: empty both times.
