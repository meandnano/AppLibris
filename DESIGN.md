# Self-hosted Ebook Server — Design

## Purpose

A personal ebook library server. The primary problem it solves: see what books I
have, and send one to a Kindle by email. Everything else is secondary to that.

Single user (plus wife as a send target), internal network only, sole maintainer.
Design decisions favour simplicity over generality throughout.

## Constraints

- Written in Go.
- Ships as a **single container** with **no external process dependencies** — no
  separate database server, no sidecar, no message broker.
- Embedded, file-backed storage.
- No JS build step, no `node_modules`.

## Implementation status

This document is the design, not a progress report — but it has outrun the
code far enough that reading it without a map is misleading. Each section
below carries a **Status** note. This table is the summary.

Legend: **Built** — done and in use. **Partial** — some of it exists, the
note says which. **Not built** — designed here, no code yet.

| Area | Status |
|---|---|
| SQLite storage, WAL, single writer, migrations | Built |
| FTS5 index | Not built — planned, see `docs/plans/2026090106-full-text-search.md` |
| Library directory, covers directory | Built |
| Scanner: startup sweep, periodic rescan, cheap check, hash identity | Built |
| Scanner: filesystem watcher | Not built — the periodic rescan is the only live-update mechanism |
| Scanner: missing-file handling | Built — mark, grace period, then prune |
| Duplicate detection (byte-identical) | Partial — the data model holds multiple locations; the UI doesn't flag them |
| Embedded metadata | Built — EPUB and FB2 (including `.fb2.zip`), all schema columns populated |
| Covers | Built — extracted, stored atomically, regenerated when missing |
| Metadata providers, chain, provenance | Not built |
| Books / authors / book_files schema | Built |
| Recipients, send log, field sources schema | Not built |
| Send to Kindle: Resend transport + size limit | Built, but nothing calls it |
| Send to Kindle: job model, recipient picker | Not built |
| Web UI: server-side templates, embedded CSS, service layer | Built |
| Web UI: library grid | Built |
| Web UI: htmx, search, book detail, inline editing, send control | Not built — search planned (`docs/plans/2026090106`) |
| Format conversion, near-duplicate detection, programmatic API | Not built — deferred by design |
| Authentication | Not built, by design |

Known defects in what *is* built are tracked as step plans under
`docs/plans/`, and lower-priority ones under `docs/backlog/`. Where a
section below says a piece is built but flawed, the plan is named.
Completed plans move to `docs/plans/completed/` and are referenced by
that path where a status note cites one as history.

## Storage engine

**SQLite**, via `modernc.org/sqlite` (pure-Go port).

Chosen over Bolt because FTS5 gives full-text search out of the box, which is
most of what a library server needs. A KV store would mean hand-rolling every
index.

The pure-Go driver means `CGO_ENABLED=0`, trivial cross-compilation, and a
scratch-based image containing little more than the binary. It is a transpiled
port and slower than the C driver under heavy concurrent writes, which is
irrelevant here: writes come in scan bursts, reads dominate.

- Run in WAL mode.
- Single writer goroutine.

**Status: Built, except FTS5.** `internal/storage` opens the database in WAL
mode with foreign keys on, and serialises writes through a write pool
pinned to a single connection rather than a hand-rolled goroutine and
channel — same guarantee, less machinery. The read pool is bounded (8
connections) so read concurrency reuses pooled connections instead of
churning fresh ones. Schema changes are embedded SQL migration files
applied in filename order, each in its own transaction.

The FTS5 index does not exist yet, but is now planned
(`docs/plans/2026090106-full-text-search.md`) — FTS5 support in the
pure-Go driver has been verified, so the reason SQLite was chosen over
Bolt is about to stop running on credit.

`CGO_ENABLED=0` holds and the image is a single static binary. The image
base is `distroless/static-debian12:nonroot` rather than literal
`scratch`: it adds exactly what scratch lacks for this app — CA
certificates (the outbound HTTPS call to Resend needs a root store),
tzdata, a writable `/tmp`, and a non-root uid — while still shipping no
shell and no package manager, so the static-binary property survives.

## Filesystem layout

**One library directory.** Writable by the application.

The originals are never modified. The rule to enforce in code: **writes only ever
create new paths.** Conversions and other derived files land in the same
directory as new files.

Because the app writes into the directory it scans, conversion output must be
registered in the index as part of the conversion transaction. The next sweep
then sees a known hash and skips it, rather than treating it as a mystery new
arrival.

**Covers** live in a separate derived directory, keyed by content hash, with only
the path stored in the database. A missing or corrupt cover is regenerable from
the source file, so this directory is fully disposable.

The library is a flat, unorganised pile of files. No folder conventions, no
directory-as-metadata heuristics. Current contents are EPUB (some FB2).

**Status: Built.** The library and covers directories work as described,
covers are keyed by content hash with only the path in the database, and
the disposability claim is now true: a sweep re-extracts any recorded
cover whose file is missing or truncated, so deleting the covers
directory costs one rescan, not the covers
(`docs/plans/completed/2026083111-cover-regeneration.md`).

The "writes only ever create new paths" rule is not yet exercised: nothing
writes into the library directory, because conversion doesn't exist.

## Scanner

Two entry points sharing one code path:

1. **Full sweep** at startup.
2. **Filesystem watcher** for live changes.

The watcher is an optimisation, not the mechanism — it is unreliable across
Docker volume mounts and network shares. A **periodic rescan** is the safety net
and is always on.

**Change detection.** Cheap check first: path + size + mtime. If all three match
the index, skip the file entirely. Only on a mismatch do we hash contents and
re-parse metadata. This keeps rescans of a large library fast.

**Identity is the content hash, not the path.** Path is a mutable attribute. This
means reorganising folders does not look like a mass delete plus mass add, and
enriched metadata survives a move.

**Status: Built, except the watcher.** The full sweep, the periodic
rescan, the cheap path+size+mtime check and content-hash identity all
work as described. One refinement over the description above: the startup
sweep no longer blocks serving — the server comes up immediately and the
first sweep runs in the background, so a large library delays its own
completeness rather than the health check.

Missing-file handling is built, and errs deliberately toward keeping
rows: a file that disappears is first *marked* missing, then deleted only
after it has stayed missing past a grace period (`MISSING_GRACE`, default
24h), with its book pruned if that was its last location. The
deleted-file-versus-unmounted-volume problem is handled by layered
guards — only a fresh per-sweep `ENOENT` counts as "gone", a sweep that
sees zero files reconciles nothing, and an unreadable subtree exempts its
rows — see `docs/plans/completed/2026083110-missing-file-reconciliation.md`.
Paths are stored relative to the library root, so the index survives the
library mounting at a different absolute path; an unreadable directory
costs its subtree, not the sweep.

Not built: **the filesystem watcher.** The periodic rescan — described
here as the safety net — is the only live-update mechanism, so a new file
takes up to `SCAN_INTERVAL` (default 15m) to appear. This remains the
right order to have built them in.

## Duplicate detection

**v1: byte-identical only.** Same content hash at two paths is one entry with
multiple file locations. The UI flags the entry as having multiple files.

Near-duplicates — the same book re-downloaded with different compression, or
different editions — are deferred. Matching those requires normalised title +
author + ISBN comparison and should surface as a suggestion rather than an
automatic merge, since false positives (omnibus editions, different translations)
are annoying to undo.

**Status: Partial.** The data model does this correctly — one `books` row,
one `book_files` row per location. The UI does not flag it; the library grid
has no multi-location indicator yet.

The flag was deliberately blocked on scanner correctness: stale rows and
absolute paths would have made the location count lie. Both blockers are
now fixed — missing files are reconciled and paths are stored relative —
so the badge is buildable whenever a UI step picks it up.

## Metadata

### Sources, in order

1. **Embedded** — the OPF package inside the EPUB gives title, author, language,
   sometimes ISBN, and often a cover. Many books will need nothing further.
2. **Providers**, chained.

Enrichment is **optional and never blocks a book from appearing in the library.**
The scanner and index are the source of truth; enrichment is a background job
queue running against existing records.

**Status: Embedded is built; providers are not.**

`internal/epub` reads title, authors, language, ISBN, description,
publisher and publication date from the OPF package, and extracts the
declared cover. ISBN is recognised in its EPUB 2 `opf:scheme` form, the
`urn:isbn:` form EPUB 3 favours, and bare ISBN-shaped identifiers, and is
normalised on the way in — it's the lookup key a future provider chain
needs. `internal/fb2` mirrors the same field set for FB2 documents,
including covers, `.fb2.zip` archives, and declared-but-wrong XML
encodings. Author order as the source file lists it is preserved through
to display.

Everything below in this section — the provider chain, the provider
interface, compile-time registration, the resolver, and field provenance —
is **design only. None of it is built.** Provenance in
particular gates two other features: without it, re-running enrichment
would clobber hand-edited values, which is the failure this design calls
out and the reason inline metadata editing shouldn't ship before it.

### Provider chain

Providers are walked in configured order — Open Library first, then Google Books.

Merging is **field-level, not all-or-nothing.** Each provider is asked only for
the fields still missing. If Open Library returns an ISBN and publication year
but no cover, that result is kept and the cover stays in the "still missing" set
for the next provider. When the set empties, the chain stops early and saves the
API calls.

### Provider interface

Deliberately tiny:

- fetch by ISBN
- search by title + author
- a name, for logging

Rate limiting, caching, and retries are **decorators wrapping providers**, not
baked into each implementation.

### Registration

Compile-time. A map from provider name to constructor, in one file. Config lists
names in order; the resolver looks them up.

No dynamic loading, no plugin API contract, no versioning. Adding a provider
means writing the file and adding one line to the map. The interface can stay
unexported.

The **resolver logic is kept separate** from the providers so ordering and
merging are testable without any real provider.

### Provenance

Each metadata field records where it came from: `embedded`, a specific provider
name, or `manual`. This is what makes it safe to re-run enrichment without
clobbering a hand-fixed value.

## Data model (v1)

**Book**

| Field | Notes |
|---|---|
| `id` | generated |
| `content_hash` | identity |
| `title` | |
| `sort_title` | |
| `publisher` | |
| `published_date` | |
| `language` | |
| `isbn` | |
| `description` | |
| `cover_path` | into the derived directory |
| ~~`file_path`~~ | **moved** — see below |
| `format` | |
| ~~`file_size`~~ | **moved** — see below |
| `added_at` | |
| `modified_at` | |
| `derived_from` | nullable FK to another book |

**Authors** — separate table with a join. Not a comma-separated string, so
"browse by author" and correcting a misspelling both stay cheap.

**Field sources** — per-field provenance, as above.

**Recipients** — `address`, `label`, `last_used_at`.

**Send log** — book, recipient address **stored as a string**, status, timestamps,
failure reason. Storing the address as a string rather than an FK means deleting
a recipient never orphans or rewrites history.

**Status: books and authors are built; the rest is not.**

`books`, `authors` and the `book_authors` join table exist as described,
with two columns the table above doesn't show: `books.cover_retry` (a
transient initial cover-store failure, retried by later sweeps, kept
distinct from "this book has no cover") and a `position` on
`book_authors`, so a book's authors keep the order its source file
credited them in. Every column in the Book table is populated by the
scanner for both supported formats.

**`file_path` and `file_size` are no longer columns on Book.** They moved to
a separate `book_files` table, one row per physical location, keyed by
`book_id` — which is what makes the v1 duplicate rule above ("one entry with
multiple file locations") representable at all. A single mutable
`file_path` column could only ever hold one of them. The table carries
`file_path` (relative to the library root), `file_size`, `modified_at`,
`added_at` and `missing_since` — the mark half of the scanner's two-phase
missing-file handling — per location.

`derived_from` exists as a column but is unused, since conversion doesn't
exist yet.

**Field sources**, **Recipients** and **Send log** are not built. All three
are prerequisites for features described later in this document, and none of
those features can start without them.

### Deferred

- **Series.** The one that hurts most to retrofit, since it is a real relation
  rather than a flag. Acceptable to defer given a mostly-standalone library.
- **Tags.**

## Covers

Extracted from the EPUB, resized to ~400px on the long edge, JPEG, written to the
derived directory keyed by content hash.

Storing thumbnails rather than originals keeps each around 30–60KB instead of
500KB–2MB. Full-resolution covers, if ever wanted, are extracted from the source
file on demand — nothing is lost by not storing them.

**Status: Built.** `internal/cover` resizes to ~400px on the long edge
(never upscaling), encodes JPEG, and writes to the derived directory
keyed by content hash, exactly as described — and, since the design
leans on it, the "regenerable from the source file" property now holds:
writes go through a same-directory temp file and atomic rename, so no
reader ever sees a truncated cover, and a sweep re-extracts any recorded
cover whose file has gone missing or zero-byte. FB2 covers are extracted
too.

## Conversion

Not required for the primary flow (Amazon accepts EPUB directly), so it is a
"someday" feature. But the model accommodates it now:

A converted file is a **separate book entity** with `derived_from` pointing at the
source. It **skips enrichment** and copies metadata from its parent. The UI shows
both rows with a format label, so the same book in two formats appears twice.

This avoids the failure mode where the two entities get independently enriched
into disagreeing metadata, or where fixing an author on one leaves the other
wrong. If the rows should ever collapse into one, the link is already there.

**Status: Not built**, as intended — this is a "someday" feature. The
`derived_from` column exists and is unused, which is the whole point of
accommodating it in the model now.

## Send to Kindle

The primary action. Not a synchronous request.

**Transport:** Resend (already in use).

**Amazon requirement:** the sending address must be on the Approved Personal
Document Email List. Resend sends from a verified domain, so the verified domain
and the approved from-address must be configured in agreement with each other.

**Size limits.** Amazon's cap is 200MB. Resend's is 40MB for the entire message,
including the base64-encoded attachment. **Resend is the binding constraint.**
Base64 inflates by ~33%, so 40MB of message allows roughly 30MB of raw file;
enforce ~28MB to leave headroom for headers and body. This should be a named
constant with the arithmetic in a comment, since the number looks arbitrary
otherwise.

Check size **before** attempting the send and surface a clear error, rather than
letting it fail inside SMTP.

**Job model.** Sends are queued jobs with persisted state:
`queued → sending → delivered | failed(reason)`.

This gives a retry button and a visible send history. The history is
independently useful — it answers "did I already put this on the Kindle?"

**Recipient selection.** A dropdown of saved addresses with labels, defaulting to
the most recently used. Adding a new address is possible inline from the same
control but is deliberately secondary — with two addresses in practice, the
common path should be two clicks. No separate management UI.

**Status: the transport is built; the feature is not.**

`internal/resend` sends one attachment through Resend's API and enforces the
~28MB limit derived above, before attempting the send, with the arithmetic
in a comment as this document asks. **Nothing calls it.** There is no
`RESEND_API_KEY`/`RESEND_FROM` wiring in the server, no recipients or
send-log schema, no job queue, and no UI — so the primary action this whole
project exists for is, today, not reachable from the app.

Three separate prerequisites stand between here and a working send, and
they are independent of each other: the `recipients`/`send_log` schema, the
queued-job model with its `queued → sending → delivered | failed` states,
and the recipient-picker UI.

The packaging blocker is gone: the image moved from `scratch` to
`distroless/static`, which carries the CA bundle the HTTPS call to Resend
needs (`docs/plans/completed/2026083113-runtime-hardening.md`).

## Web UI

**Server-side rendered Go templates + htmx.** No JSON API for the UI, no
JS framework. Templates, htmx, and CSS embedded via `go:embed` — no build step,
nothing node-shaped in the container.

htmx is used only where dynamism is actually needed:

- search-as-you-type against the FTS5 index
- the send-to-Kindle button swapping into a status indicator that polls the job
- inline metadata editing on a book row

**Status: Partial — the server-rendered shell is built, the dynamic parts
are not.**

Built: `internal/web` serves the library grid at `GET /`, with templates,
CSS and a theme script embedded via `go:embed` and no build step, translated
from the mockups in `UI.md`. Cover thumbnails are served from the covers
directory; both the covers and static mounts refuse to generate directory
listings and carry cache validators. Templates render into a buffer
before anything is written, so a template error is a clean 500 rather
than a truncated page.

The catch-all caveat this section used to carry is fixed: the library
page matches only `/` exactly and the mux owns its 404s, so a
`/books/{id}` route can exist without a stale link silently rendering
the whole library
(`docs/plans/completed/2026083112-web-transport-correctness.md`).

Not built: **htmx itself is not vendored** — none of the three interactions
above exists. Search is the first with no unbuilt prerequisite and is
planned, htmx vendoring included
(`docs/plans/2026090106-full-text-search.md`); the send control remains
blocked on the job model and inline editing on field provenance. Book
detail and the multi-location badge are likewise unbuilt, though nothing
blocks either any more.

### Layering for a future API

A programmatic API is expected later (**not** OPDS — specific features TBD).

Because HTML and JSON responses differ, the shared layer is **not** the HTTP
handlers — it is a **service layer beneath them**. Handlers stay thin: parse
request, call service method, render. A future `/api/v1` is then a second thin
transport over the same service calls, and no business logic ends up trapped
inside a template handler.

**Status: Built.** `internal/service` sits beneath `internal/web` as
described, and the handlers are thin over it. It has one method so far
(`ListBooks`), which is as much as the single existing page needs. The
`/api/v1` transport itself remains deferred.

## Authentication

None. Internal network only. Bind it and trust the network.

**Status: as designed** — there is no auth, and nothing to build.

Worth stating explicitly, since it is the assumption several other
decisions rest on: there is no rate limiting and no request logging, and
that is acceptable *only* under this assumption. (Directory listings on
the covers and static routes, which earlier revisions of this note also
leaned on the trusted network to excuse, are now suppressed outright —
not because of a threat model change, but because a listing of every
content hash in the library was surface nobody asked for.) If this
server is ever exposed beyond a trusted network, this section is the
first thing that has to change, and several others follow it.

## Deferred list

- Series
- Tags
- Format conversion
- Near-duplicate detection
- Programmatic API
- Authentication / user management

**Status: all six remain deferred, and none has been started.**

Worth keeping distinct from the rest of this document: *deferred* is not the
same as *not yet built*. Everything in this list was consciously ruled out
of scope. The filesystem watcher, the FTS5 index, the send job model,
provider enrichment and the book detail page are all unbuilt but **not**
deferred — they are in scope and simply haven't been reached. Only the six
items above are decisions rather than backlog.
