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

## What may be staged at once

Staging is bounded, in total bytes rather than in stages: what costs a small
machine something is what is held, and one budget means the same thing
whether it is one large upload or twenty small ones. `os.TempDir()` is tmpfs
in a container, so an unconfirmed upload is RAM on a machine whose whole job
is to serve a library. The budget is `stagingBudgetFactor` times the import
cap, and an upload past it is refused with `ErrStagingFull` and a sentence
telling the person to finish or discard what is already waiting.

The reservation is taken **at the cap, before the copy starts**, because the
body's size is not known until it has been written — a check made afterwards
is a budget that admits everything and reports later. It is corrected to
what the stage actually retains once the copy lands, and two uploads racing
the check therefore cannot both pass it and overshoot together.

What it charges is everything the record holds, not just the file: the cover
kept in memory for the preview, which each format package caps at
`cover.MaxCoverBytes`, and the metadata the preview renders. Charging the
file alone would let a ten-kilobyte upload hold eight megabytes of cover —
a compressible image inside a small archive — against a ten-kilobyte
reservation.

It is given back wherever what it charges goes: a discard, an expiry, a
confirm, and the duplicate verdict that deletes the file the moment it is
reached. The duplicate is the one partial release — its file goes at once
and its cover stays, because the preview still renders.

## Format by content, extension by format

`detectSuffix` decides what a file is from its bytes. The client's filename
and `Content-Type` are hints and nothing more: a browser sends
`application/octet-stream` for an FB2, and people rename files.

A zip is read through `archive/zip`, which costs one open and inflates
nothing: `META-INF/container.xml` makes it an EPUB, exactly one `.fb2` entry
and no container makes it an `.fb2.zip`. A plain file is an `.fb2` only when it
satisfies both halves: it must begin, after an optional byte-order mark and
whitespace, with `<?xml` or `<FictionBook`, **and** `<FictionBook` must
appear somewhere in the sniffed window. Anything else is refused.

Both halves, because a declaration says a file is XML and not which XML it
is: an SVG or a bare OPF passes the opening test and would be written into
the library as a `.fb2` and indexed as a book under its filename. The root
is searched for across the window rather than matched as a prefix, since a
real FB2 carries its declaration — and sometimes a DOCTYPE and a comment —
ahead of it, and the window is 4 KiB so none of that can push it out.

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

## A cover is not an image until something says so

Neither format reader validates what it hands back as a cover.
`internal/epub` returns whatever zip entry the manifest's `cover-image`
href names, without reading that item's declared `media-type`;
`internal/fb2` returns whatever a `<binary>` element decodes to, without
reading its `content-type`. That costs nothing on the way into
`cover.Store`, which decodes before it writes.

It is not free on the way to a browser. The preview serves its cover from
the stage, because a staged book has no entry in `COVERS_DIR` and must not
acquire one before anybody has said to keep it — so those bytes go out over
HTTP, and sniffing them would let an uploaded file choose the media type.
An EPUB whose manifest points `cover-image` at an HTML document would come
back as `text/html` from this app's own origin, which is the origin
`sameSiteOnly` admits.

So `Stage` runs the bytes through `cover.ContentType`, the same header read
`cover.Store` makes, and keeps the type the decoder named. A cover that
fails is dropped, which also makes the preview honest: `HasCover` means "a
cover this app would keep", so the page stops promising one the import would
then discard. The route serves that recorded type and never
`http.DetectContentType`, under `X-Content-Type-Options: nosniff` — which
every route serving bytes rather than a rendered template carries, since one
rule about media types is easier to hold than a per-route judgment about
which bytes are trusted.

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
   separator, stripped of control characters, `/`, `\`, `:`, the six
   characters Windows refuses (`<`, `>`, `"`, `|`, `?`, `*`) and leading
   dots, collapsed onto single spaces and cut to 200 bytes on a rune
   boundary; the sniffed suffix replaces whatever extension it carried. A
   mounted library is read from Windows often enough to be worth not
   creating a name it cannot open. When
   nothing survives, the staged title is tried the same way and then the
   stage id, which cannot be empty. If the name is taken, ` (2)`, ` (3)` and
   so on go before the suffix.
2. **Copy.** The bytes go to `<name>.part`, opened `O_CREATE|O_EXCL`, then
   `Sync` and close. Neither the scanner nor the watcher acts on a `.part`
   suffix, so a half-written file is never indexed.
3. **Publish.** `os.Link` the finished part onto the first free name, then
   unlink the part. The link is the test-and-set, and that is the whole
   reason it is a link: it fails `EEXIST` rather than replacing, where
   `os.Rename` silently destroys whatever is at the name. The rule for this
   directory is that writes only ever create new paths
   (`docs/notes/design.md`), and the seconds a large copy takes are long
   enough for something else to have created this one — a person dropping a
   file into the pile they manage by hand is the ordinary case, not an
   exotic one. A name taken since the claim costs one link attempt and
   nothing else: the part already holds the bytes, so the next candidate is
   linked from the same data rather than copied again.

   The `.part` name and the published name are therefore chosen separately,
   and need not match. A filesystem with no hard links at all — exFAT, some
   SMB mounts, a few volume drivers — falls back to `Lstat` then `Rename`,
   logged once, and there the window stays open; refusing to import would
   break a working deployment over a race that only opens when something
   else writes the same name mid-copy.
4. **Index.** `scanner.IndexFile` in the confirming request, so the
   response can redirect to the book.
5. **Clean up.** The staged file goes, and the cover held beside it. The
   record does not: it carries the book id, which is what lets a second
   confirm answer instead of copying the same bytes in again, so it is left
   for the janitor to expire.

A copy and not a rename out of staging, because `/tmp` and `/library` are
different filesystems in every deployment that matters and `os.Rename`
across them fails.

A failed index write leaves the library file **in place**. Deleting it would
discard the bytes over a database error, and the next sweep is the recovery
the scanner already promises; the response says so, and the log carries a
Warn.

A crash between the copy and the publish leaves a `<name>.part` in the
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

`cmd/server` clears, creates and removes
`LIBRARY_DIR/.applibris-write-probe` after resolving the directory. It is
cleared first because the create refuses a name that is already taken: a
probe left by a crash, or by the remove failing, would otherwise answer "not
writable" for every later start of a perfectly writable library. The create
keeps `O_EXCL` so it cannot follow a symlink left at that name, and
`os.Remove` unlinks such a link rather than its target, so neither call
reaches the far end. On failure importing is off for the run, at Warn,
naming the uid the process runs as and the uid that owns the directory — the
same two facts `mkdirError` names, and for the same reason.

A probe and not a look at the mode bits, because a read-only mount, an ACL
and a uid mismatch all fail at the same call and none of them shows in the
mode. Once and not per request, because the answer does not change while the
process runs and a confirm that fails anyway reports its own error.

The probe name begins with a dot and carries no supported suffix, so a sweep
that overlaps it walks past it.

The staging directory is the other precondition, decided in the same place.
`importer.New` wipes and creates `os.TempDir()/applibris-imports`, and a
hardened container — run `--read-only` with no tmpfs at `/tmp` — has no
such directory to give. That disables importing the way the probe does,
at Warn, rather than ending the run: the library can still be read, and
everything but the Import page works on one that can. The Warn names the
directory, so `TMPDIR` is the obvious remedy.

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
server that does not support the control is left alone, and the global
timeout applies.

The body is wrapped in `http.MaxBytesReader` at the cap plus multipart
overhead — before the read-only check, not after, since that refusal answers
a request whose body is still arriving too. The importer's own count of the
part's bytes is the cap a refusal actually names: the multipart part is what
is being measured, and only the importer sees it. The body streams through
`r.MultipartReader` rather than `ParseMultipartForm`, which would spool the
whole file to a second temporary copy before the handler saw a byte of it.

Every refusal drains what is left of the body before rendering, so the
connection is never left with a request body nobody consumed. It is hygiene
rather than a cure. The copy stops at the cap plus one byte of the file part
and reads no further, so an over-cap upload never trips `MaxBytesReader` at
all — which leaves at most the multipart overhead to drain, too little to
have blocked anyone. The case that could genuinely lose a refusal is a body
far past the limit, with megabytes still in flight; `MaxBytesReader` will not
hand those over, so nothing can drain them and nothing beats Go's lingering
close.

`MAX_IMPORT_SIZE` defaults to 64 MiB. Zero and negative are refused rather
than read as "no limit": the cap is what bounds an upload into temporary
space, and a deployment that means to disable importing takes away write
access to the library, which is the thing the app actually checks.

## Dropping a file on the library page

A file dropped anywhere on the library page is the upload form's own post.
`drop.js` puts it into a hidden plain form — `action="/import/file"`,
`enctype="multipart/form-data"`, no `hx-post` — and calls `form.submit()`,
so the route answers exactly as it answers with JavaScript off: a 303 to
`/import/{id}` whose page shows the verdict, or a 422 carrying the Import
page with the refusal and the file input. There is no second upload path,
no second preview and no second response shape to keep in step with the
first, and every bound above applies unchanged. A fetch or an htmx request
would need its own answer for each of those; a native submit needs none.

The script refuses two things before a byte is sent. Several files, or a
folder, because a stage holds one book and its page shows one verdict. And
a file over the cap, since a body far past the limit is the one case whose
422 can be lost while the rest of it is still in flight (see above). The
cap and both sentences come from the page: the too-large one is
`importFailureLine`'s, so a drop stopped early reads exactly as one the
importer stopped. The server's count remains the check; the script's only
spares the upload. The filename is not examined — `detectSuffix` decides,
and its refusal already says what the file is not.

The target is the library page alone, rendered only when a `Stager`
exists, and from `library.html` rather than the `book-grid` fragment a
search swaps in. On a book's own page a drop would read as replacing that
book's file, which is not what it does.

Both refusals are shown where the overlay they replace was: `drop__errors`
is fixed to the viewport, so a refusal reaches the person whatever the grid
has been scrolled to, and an announcement reaches a screen reader because
the live region is that wrapper rather than the sentences inside it — one
that appears is not reliably read, one whose contents change is. There is
nothing else to see: the drop is stopped before a request, so no page
arrives to carry the refusal the way the Import form's 422 does.

`uploading` locks out every later drag and holds the veil up, and two
things take it down. A bfcache `pageshow` covers Back from the preview. A
submit the person cancels covers the rest: aborting a navigation leaves the
document loaded and fires nothing at all, so Escape clears it, whether the
upload was stopped with Escape or with the browser's own Stop.

## What is deliberately absent

- **Importing from a URL.** It reuses this machinery unchanged and is its
  own step.
- **A programmatic API.** Deferred; the obstacle is in
  `docs/notes/design.md`. Every state-changing route refuses a request
  carrying no `Sec-Fetch-Site`, which a non-browser client never sends.
- **Deleting a book from the UI.** Discard removes a staged file only. A
  confirmed import is a library file like any other and goes the way any
  other does.
- **Editing metadata in the preview.** The detail page already does that,
  one field at a time, and the redirect lands there.
