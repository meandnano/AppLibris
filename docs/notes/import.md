# Importing a book

Rationale for `internal/importer`, `scanner.IndexFile`, the import surface
of `internal/service` and the `/import` routes. The package map and the
invariants that must hold are in CLAUDE.md.

## Where an import lands

An import lands in the **library root**, under a name derived from the file
the person offered. It is a library file like every other one from that
moment: the scanner owns it, a later sweep reconciles it, and removing it is
removing a file from the library.

That is the whole reason the feature can exist at all without a second store
to keep in step. The library directory is the one source of truth about what
the library holds, so an import that wrote anywhere else would be a book the
directory does not have, and the next sweep would be right to disagree.

It also decides the deployment advice. `LIBRARY_DIR` is read only as far as
the scanner is concerned, so a read-only mount is a legitimate
configuration — and one where importing cannot work. Rather than making the
mount a startup question, the probe below answers it and importing is either
offered or explained.

The flat root, and not a subdirectory of its own, because the library is a
flat unorganised pile by design (`docs/notes/design.md`): a folder named for
where a file came from is a convention this app does not have and would then
have to keep.

## Preview, then confirm

A dropped file is staged outside the library, parsed where it lies, and
shown — title, authors, format, size, cover, the name it will take, and
what the index already knows about it — before anything is written into the
library. The decision it asks for is one a person can only make with the
book in front of them: whether this file is the one they meant, and whether
the library already has it.

The staged file lives under `os.TempDir()/applibris-imports`, which is the
standard `TMPDIR` and is separate from `/data`. It holds nothing worth
keeping and nothing worth backing up, so it is not `/data`'s problem; a
deployment that wants it elsewhere moves it the way every other program's
temporary space is moved.

A stage is an id, a path, the parsed preview, the content hash, the verdict
and an instant, in a map behind a mutex. There is no table. A stage nobody
confirmed after a restart is a file nobody asked for, so startup wipes the
directory and that is the entire recovery story. Writing a stage to the
database would create rows whose only purpose is to be cleaned up.

Stages expire thirty minutes after they are made, swept by a janitor on the
scan context and rechecked on confirm — so a click on a tab left open since
lunch gets "this import has expired" rather than acting on a file the
janitor deleted a moment ago. Thirty minutes is long enough to read a
preview and short enough that a forgotten tab does not hold the size cap for
a day, and it is not configurable: nothing about a deployment changes how
long a person takes to press a button.

The id is 128 random bits so one tab cannot reach another person's stage by
guessing the number next to its own. It is not a credential — every
state-changing route is same-site-only regardless — it is what keeps two
people's imports apart on a server that has no idea who either of them is.

## Format by content, extension by format

`detectSuffix` decides what a file is from its bytes. The client's filename
and `Content-Type` are hints and nothing more: a browser sends
`application/octet-stream` for an FB2, and people rename files.

A zip is read through `archive/zip`, which costs one open and inflates
nothing: `META-INF/container.xml` makes it an EPUB, exactly one `.fb2` entry
and no container makes it an `.fb2.zip`. A plain file beginning, after an
optional byte-order mark and whitespace, with `<?xml` or `<FictionBook` is
an `.fb2`. Anything else is refused.

The staged file is then named `<id><suffix>`, because `epub.ReadMetadata`
and `fb2.ReadMetadata` are picked by the suffix and read the path they are
given — the staged file has to be named the way the library file will be for
the same parse to happen twice. The library name takes the same suffix, so
an FB2 offered as `book.epub` is written `book.fb2` and the preview says so.

The parse that follows is the scanner's own, with the same caps, so a
hostile archive is bounded exactly as it would be arriving through the
directory. The preview runs a second, separate parse purely to have
something to show; what is actually stored comes from the scanner during
indexing.

## The three verdicts

`Stage` looks the content hash up before it returns, so the preview carries
one of three answers:

- **New.** Import is offered.
- **Exists** — byte-identical content the library already holds. Only
  Discard is offered, beside a link to the book. Copying it would add a
  second location to one book and nothing else. The staged file is deleted
  at once, since there is nothing left to confirm; the record survives so
  the page still renders.
- **Title match** — different content filed under a `sort_title` the
  library already holds. Import is offered under a warning. Different
  editions of one book are a legitimate thing to own, and the person is the
  only one who knows which this is.

The check is a snapshot, and confirm does not trust it: a sweep can index
the same bytes between preview and confirm. `IndexFile`'s answer is the
truth. If the book it names already had another location, the copy just
written is a second location of a known book — which the detail page renders
as "2 paths" — and the redirect lands on that book. That is the correct
outcome for the race and needs no special case.

Anything beyond a title match is out of scope here and stays where
`docs/notes/design.md` put it: comparing authors and ISBNs to guess at
near-duplicates belongs to the suggestion feature that note defers, because
false positives are annoying to undo.

## Confirm writes in an order that cannot half-fail

1. **Name.** The offered name is reduced to a base name on either
   separator, stripped of control characters, `/`, `\`, `:` and leading
   dots, collapsed onto single spaces and cut to 200 bytes on a rune
   boundary; the sniffed suffix replaces whatever extension it carried. When
   nothing survives, the staged title is tried the same way and then the
   stage id, which cannot be empty. If the name is taken, ` (2)`, ` (3)` and
   so on go before the suffix.
2. **Copy.** The bytes go to `<name>.part`, opened `O_CREATE|O_EXCL`, then
   `Sync`, close and `Rename` onto `<name>`. Neither the scanner nor the
   watcher acts on a `.part` suffix, so a half-written file is never
   indexed, and the rename is what publishes it. The claim is two-sided: an
   `Lstat` rules out a name the library already holds, and the `O_EXCL`
   rules out a name another confirm is copying into right now.
3. **Index.** `scanner.IndexFile` in the confirming request, so the
   response can redirect to the book.
4. **Clean up.** The staged file and the record go.

A copy and not a rename out of staging, because `/tmp` and `/library` are
different filesystems in every deployment that matters and `os.Rename`
across them fails.

A failed index write leaves the library file **in place**. Deleting it would
discard the bytes over a database error, and the next sweep is the recovery
the scanner already promises; the response says so, and the log carries a
Warn.

A crash between the copy and the rename leaves a `<name>.part` in the
library. Startup does not remove it: it cannot know the file is this app's
rather than a download someone is running into the same directory, and
`.part` is the suffix downloaders use. It is the one manual tidy-up this
feature asks for.

A repeated confirm answers the first call's book id rather than copying the
file in again, so a double click is a slip and not a second import. The
record is what carries that answer, which is why it survives the confirm and
is left to expire rather than being deleted on the spot.

## Indexing through the scanner

`scanner.IndexFile` is the scanner's own per-file path with a fresh `Result`
and the book id returned. Importing needs no second way into the index, and
having one would be a second place for every guard to drift: `capMetadata`,
the cover store's `ErrUnsupportedCover` split and the orphan logging all
live in `createBook`, which `scanFile` calls.

`scanFile` is already idempotent, which is what makes indexing in the
request safe. A sweep over a path the import just indexed sees a matching
path, size and mtime and does nothing; if a sweep and a confirm race, they
key on the same content hash and converge on one book with one location.

`internal/importer` therefore depends on `internal/scanner`, and
`internal/service` on `internal/importer`. That makes `internal/scanner`'s
own in-package tests unable to import `internal/service`, which one of them
needs; it uses the external `scanner_test` package and an `export_test.go`
instead, which is the ordinary Go answer and keeps both suites on one set of
fixtures.

## Writability is probed once, at startup

`cmd/server` creates and removes `LIBRARY_DIR/.applibris-write-probe` after
resolving the directory. On failure importing is off for the run, at Warn,
naming the uid the process runs as and the uid that owns the directory — the
same two facts `mkdirError` names, and for the same reason.

A probe and not a look at the mode bits, because a read-only mount, an ACL
and a uid mismatch all fail at the same call and none of them shows in the
mode. Once and not per request, because the answer does not change while the
process runs and a confirm that fails anyway reports its own error.

The probe name begins with a dot and carries no supported suffix, so a sweep
that overlaps it walks past it.

The routes stay registered when importing is off, so a tab open since before
a restart gets an explanation rather than a 404. What the flag withholds is
the nav link: a masthead entry pointing at a page that can only say no is
not worth the space.

## The upload route extends its own read deadline

`cmd/server`'s `http.Server` sets a 30-second `ReadTimeout`, which covers
the body. Sixty-four megabytes over Wi-Fi to a NAS routinely takes longer.
The upload handler extends its own read deadline through
`http.NewResponseController`, sized from the cap at a floor of 1 MiB/s, so
every other route keeps the tight timeout and a stalled upload still ends. A
server that does not support the control is left alone: the global timeout
then applies, which is what this replaces.

The body is wrapped in `http.MaxBytesReader` at the cap plus multipart
overhead, and the importer's own count of the part's bytes is the cap a
refusal actually names — the multipart part is what is being measured, and
only the importer sees it. The body streams through `r.MultipartReader`
rather than `ParseMultipartForm`, which would spool the whole file to a
second temporary copy before the handler saw a byte of it.

`MAX_IMPORT_SIZE` defaults to 64 MiB. Zero and negative are refused rather
than read as "no limit": the cap is what bounds an upload into temporary
space, and a deployment that means to disable importing takes away write
access to the library, which is the thing the app actually checks.

## What is deliberately absent

- **Importing from a URL, and drag and drop.** Both reuse this machinery
  unchanged and are their own steps.
- **A programmatic API.** Still deferred, and `docs/notes/design.md` now
  records the specific obstacle: every state-changing route refuses a
  request carrying no `Sec-Fetch-Site`, which a non-browser client never
  sends.
- **Deleting a book from the UI.** Discard removes a staged file only. A
  confirmed import is a library file like any other and goes the way any
  other does.
- **Editing metadata in the preview.** The detail page already does that,
  one field at a time, and the redirect lands there.
