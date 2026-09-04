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

This document is the design, not a progress report — but it once outran
the code far enough that reading it without a map was misleading, and the
map has been worth keeping since. Each section below carries a **Status**
note. This table is the summary.

The gap has closed completely. The primary flow this project exists for —
see the library, find a book, send it to a Kindle, confirm it arrived —
works end to end, and **every feature designed below is now built.**
Nothing in this document is in scope and unwritten; what remains is the
deferred list, which is deferred by design rather than by backlog.

Metadata provider enrichment was the last of it, and it landed in three
steps rather than one: the queue, worker and resolver
(`docs/plans/completed/2026090304-enrichment-queue-and-resolver.md`),
then the two providers and their decorators
(`.../2026090305-metadata-providers.md`), then the UI that makes it
something a person can ask for (`.../2026090306-enrichment-in-the-ui.md`).
Splitting it that way was deliberate and is recorded here because the
order mattered: a UI designed before the mechanism worked would have been
shaped around states that turned out not to exist.

That makes this table cheaper to read than it used to be, and it changes
what the Status notes are for. They started as a map over a document that
had outrun its code; what they mostly record now is where the design was
*wrong* and what the code does instead — which is the more useful half to
keep.

Legend: **Built** — done and in use. **Not built** — designed here, no
code yet, which now means only the deferred items. (There is no longer a
**Partial** row; `Metadata provenance` was the last one.)

| Area | Status |
|---|---|
| SQLite storage, WAL, single writer, migrations | Built |
| FTS5 index | Built |
| Library directory, covers directory | Built |
| Scanner: startup sweep, periodic rescan, cheap check, hash identity | Built |
| Scanner: filesystem watcher | Built — fsnotify, debounced, with startup mount and delivery checks |
| Scanner: missing-file handling | Built — mark, grace period, then prune |
| Duplicate detection (byte-identical) | Built — one entry per content hash, multiple locations, flagged on the grid |
| Embedded metadata | Built — EPUB and FB2 (including `.fb2.zip`), all schema columns populated |
| Covers | Built — extracted, stored atomically, regenerated when missing |
| Metadata providers, chain | Built — Open Library and Google Books, ordered by `METADATA_PROVIDERS` |
| Metadata provider decorators (rate limit, cache, retry) | Built — composed once at construction, cache outermost |
| Metadata provenance | Built — recorded on every create, edit and provider write; read by the resolver and shown on the detail page |
| Books / authors / book_files schema | Built |
| Recipients, send log, field sources schema | Built |
| Send to Kindle: Resend transport + size limit | Built |
| Send to Kindle: job model, recipient picker | Built |
| Send to Kindle: history view, recipient management | Built — `/history`; a saved address can be removed from the picker |
| Web UI: server-side templates, embedded CSS, service layer | Built |
| Web UI: library grid | Built |
| Web UI: htmx, search, book detail | Built |
| Web UI: inline metadata editing | Built |
| Web UI: send control | Built |
| Web UI: enrichment control, provenance markers | Built — per book, from the detail page |
| Format conversion, near-duplicate detection, programmatic API | Not built — deferred by design |
| Authentication | Not built, by design |
| Cross-site guard on state-changing routes | Built — `Sec-Fetch-Site`, see Authentication |

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

**Status: Built.** `internal/storage` opens the database in WAL
mode with foreign keys on, and serialises writes through a write pool
pinned to a single connection rather than a hand-rolled goroutine and
channel — same guarantee, less machinery. The read pool is bounded (8
connections) so read concurrency reuses pooled connections instead of
churning fresh ones. Schema changes are embedded SQL migration files
applied in filename order, each in its own transaction.

The FTS5 index exists now (`docs/plans/completed/2026090106-full-text-search.md`)
— a `books_fts` virtual table over title, authors, description and ISBN,
synced on every book create — so the reason SQLite was chosen over Bolt
is no longer running on credit.

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

**Status: Built.** The full sweep, the periodic
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

The **filesystem watcher** is built, and is exactly the trigger this
section describes rather than a second index path: it never reads, hashes
or parses the file an event names, it only wakes the one scan goroutine.
So a sweep is a sweep however it was woken, and the periodic rescan
remains the mechanism — a watcher that never fires costs latency, not
correctness. Events are debounced (`WATCH_SETTLE`, default 5s), because
an event says something changed, not that it finished changing.

The unreliability this section predicted is real and is now *reported*
rather than assumed, because neither fact is observable later: adding a
watch to a filesystem that will never deliver succeeds silently, so a
dead watcher is indistinguishable from an idle one. Startup names the
filesystem backing `LIBRARY_DIR` and warns for the types where changes
routinely land behind the mount rather than through it (`fuse.*`, `nfs`,
`cifs`, `9p`), then proves delivery by creating a file and waiting for
its own event. Measured against a FUSE passthrough, which is the Unraid
case: a file written *through* a `/mnt/user` share is seen, and the mover
shuffling between `/mnt/cache` and `/mnt/diskN` is not — so the fix is to
bind-mount the disk path. `WATCH_ENABLED=false` leaves exactly the
pre-watcher behaviour, which is what a mount whose probe reports silence
wants.

See `docs/plans/completed/2026090205-filesystem-watcher.md`, and CLAUDE.md
for the failure modes `Refresh` has to cover — inotify is not recursive,
a watch is dropped silently when its directory is deleted, and a
directory that moves out of the library leaves its descendants' watches
filed under names something else can take.

## Duplicate detection

**v1: byte-identical only.** Same content hash at two paths is one entry with
multiple file locations. The UI flags the entry as having multiple files.

Near-duplicates — the same book re-downloaded with different compression, or
different editions — are deferred. Matching those requires normalised title +
author + ISBN comparison and should surface as a suggestion rather than an
automatic merge, since false positives (omnibus editions, different translations)
are annoying to undo.

**Status: Built**, for the byte-identical half this section scopes.
One `books` row, one `book_files` row per location, and the library grid
marks any book with more than one — a dotted-underline "2 paths" beside
the format badge, reusing the accent affordance the detail page already
uses for the same thing. Search results share the grid's fragment, so
they carry the marker too. The count comes from a single `GROUP BY` over
`book_files` rather than a query per card
(`docs/plans/completed/2026090302-multi-location-badge.md`).

The grid flags; it does not act. There is no merge or delete-the-other-copy
button, and that is deliberate: the scanner owns the library directory,
whose rule is that writes only ever create new paths. Removing a book file
from a web request is a different feature with a different risk profile.
Flagging is useful alone — the duplicate gets fixed in a file manager.

The flag was blocked on scanner correctness rather than on effort, and
the blockers are worth remembering because they are what keeps the count
honest: stale rows would have made every deleted copy read as a permanent
duplicate, and absolute paths would have flagged the *entire library*
after a remount at a different prefix. Missing-file reconciliation and
relative paths fixed both before this was built.

**A missing location still counts.** A vanished path keeps its
`book_files` row until `MISSING_GRACE` elapses, and the detail page lists
it — annotated — for that whole window, so excluding it from the grid's
count would make two screens that link directly to each other disagree
about one book. The marker therefore persists for at most the grace
period after a duplicate is deleted, then the row is pruned and it
disappears on its own.

## Metadata

### Sources, in order

1. **Embedded** — the OPF package inside the EPUB gives title, author, language,
   sometimes ISBN, and often a cover. Many books will need nothing further.
2. **Providers**, chained.

Enrichment is **optional and never blocks a book from appearing in the library.**
The scanner and index are the source of truth; enrichment is a background job
queue running against existing records.

**Status: Built** — embedded, provenance, both providers, the chain and
the UI that reaches it.

`internal/epub` reads title, authors, language, ISBN, description,
publisher and publication date from the OPF package, and extracts the
declared cover. ISBN is recognised in its EPUB 2 `opf:scheme` form, the
`urn:isbn:` form EPUB 3 favours, and bare ISBN-shaped identifiers, and is
normalised on the way in — it is the lookup key the provider chain uses,
and normalising at both ends is what makes it round-trip.
`internal/fb2` mirrors the same field set for FB2 documents, including
covers, `.fb2.zip` archives, and declared-but-wrong XML encodings.
Author order as the source file lists it is preserved through to display.

Provenance shipped first precisely because this section said it had to:
it gates inline metadata editing, which is built on top of it. A
`field_sources` row records `embedded`, `manual` or a provider's name per
field, written inside the same transaction as the book it describes and
updated on every edit.

It was deliberately **write-only** for its first two steps, and that was
the point rather than an oversight: a book edited in the meantime had to
carry its marker by the time the enrichment step arrived, or that step
would have overwritten hand-fixed values with no way to tell they were
hand-fixed. Writing it from the start is what made every book edited
before the resolver existed already correct once it did. It now has two
readers — the resolver, which consults it to decide what to ask a provider
for, and the detail page, which marks provider-sourced values.

One column was added later than the rest: a fetched **cover** carries
provenance too, which needed `field_sources.field`'s CHECK constraint
widened by table rebuild. The step that fetches covers decided that, and
the reason is worth keeping — a book with no cover renders as a dashed box
and is the most visible gap in the grid, so enrichment that filled six
text fields and left the one visible hole would have looked broken.

The rule the resolver honours, and the one easiest to get backwards: **a
cleared field stays `manual`.** An empty value with a `manual` source is a
decision someone made, not metadata that is missing. A resolver inferring
provenance from emptiness would undo exactly the edits this table exists
to protect. It is one function (`isMissing`) with both halves in one
place — a field is worth asking about only when it is *both* empty *and*
not `manual` — because dropping either half breaks it in a different
direction: without the emptiness test, re-enrichment overwrites good
embedded metadata with a guess; without the `manual` test, a field
someone deliberately cleared gets silently refilled.

The same rule is enforced a second time at the write, not just at the
decision: `ApplyEnrichedFields` re-reads each field's current value and
provenance inside its own transaction and skips any that is no longer
missing. `Resolve`'s snapshot can be minutes stale by the time a provider
answers, and a concurrent manual edit has committed by then — so the
recheck is what makes a provider's late answer unable to clobber it.

There is no backfill migration and there never was one. The service has
not been deployed, so no database predates the table; provenance is
established at creation instead.

### Provider chain

Providers are walked in configured order — Open Library first, then Google Books.

Merging is **field-level, not all-or-nothing.** Each provider is asked only for
the fields still missing. If Open Library returns an ISBN and publication year
but no cover, that result is kept and the cover stays in the "still missing" set
for the next provider. When the set empties, the chain stops early and saves the
API calls.

**Status: Built as designed.** `internal/enrich.Resolve` takes no database
and no clock — the book's whole current state is passed in — which is what
makes ordering and merging testable against fakes, as the interface
section below asks for. A test asserts the un-called provider really is
never called once the missing set empties, rather than merely that its
answer goes unused.

One thing this section did not settle, and the providers had to: **what a
provider does when the network is unhelpful.** Four cases, deliberately
not collapsed into one:

| Case | Behaviour |
|---|---|
| 200 with no match | Zero result, no error. **Not** a failure — this is the common case. |
| 404 | Same. A missing record is an answer. |
| 429 / 5xx | An error, marked retryable. |
| Timeout / transport failure | An error, marked retryable. |

Folding the first two into the third is the failure mode to avoid: it
would turn "this book is obscure" into a logged error on most books, and
an error log that fires constantly is one nobody reads. A provider erroring
is logged and skipped, and the chain continues — it is not the resolver's
own failure to report.

### Provider interface

Deliberately tiny:

- fetch by ISBN
- search by title + author
- a name, for logging

Rate limiting, caching, and retries are **decorators wrapping providers**, not
baked into each implementation.

**Status: Built as designed**, and the interface stayed as tiny as this
section promised. Each decorator satisfies the same interface, so they
compose in any order and the resolver cannot tell they are there; each is
tested against a fake with no HTTP at all, which is the point of them
being decorators.

The **composition order** is fixed once at construction and is easy to get
backwards: `WithCache(WithRetry(WithRateLimit(p)))` — cache outermost, so
an answer already in memory costs neither a rate-limit token nor a retry
attempt; rate limit innermost, so every attempt a retry makes takes a
token of its own. Rate limiting on the outside would make a cached hit
wait a full interval for an answer it already has, and would leave retries
paced only by their own backoff — sending a provider that had just
answered 429 three requests inside one token, which is exactly when it
must not.

Two things the live APIs taught that this interface cannot express, both
found by asking them rather than reading their docs:

- Open Library's search endpoint answers about a **work**, not the
  edition an ISBN names. Its `language` field lists every language any
  edition was ever published in — 31 of them, beginning `bul`, for an
  English printing of *The Hobbit* — and its date is the work's first
  publication, 75 years off. So "fetch by ISBN" goes to an
  edition-scoped endpoint, and the title/author search deliberately
  returns **neither** language nor publication date: where the only
  available value describes the work, the honest answer is to leave the
  field empty. An empty field is one the chain offers to the next
  provider and a person can still fill; a wrong one reads as answered and
  is never reconsidered. That correction cost a whole step of its own
  (`docs/plans/completed/2026090401-openlibrary-field-fidelity.md`), and
  it was invisible to every test until someone made a real request.
- Google Books has **no live verification yet** — an API key was not
  available and the anonymous per-day quota was exhausted, so its parser
  is still trusted against its documentation, which is exactly the
  position Open Library was in when the above turned up. Tracked in
  `docs/backlog/2026090403-googlebooks-has-no-live-verification.md`
  rather than pretended away.

### Registration

Compile-time. A map from provider name to constructor, in one file. Config lists
names in order; the resolver looks them up.

No dynamic loading, no plugin API contract, no versioning. Adding a provider
means writing the file and adding one line to the map. The interface can stay
unexported.

The **resolver logic is kept separate** from the providers so ordering and
merging are testable without any real provider.

**Status: Built, in one place, with two corrections to the details.** The
map lives in `internal/providers` rather than beside the resolver as this
section implies: both provider packages import the resolver's package for
the interface and its result type, so a registry in that package
importing them back would be an import cycle. And the interface did *not*
stay unexported — `enrich.Provider` has to be named by both provider
packages, by the registry and by the worker's constructor, so it is
exported and simply lives under `internal/`, which is what actually keeps
it out of any public API.

`METADATA_PROVIDERS` (default `openlibrary,googlebooks`) lists names in
order. Two behaviours are worth stating because they differ from how the
send transport treats its own missing config:

- An **unknown name fails startup**, naming it and listing the valid
  ones — where a missing `RESEND_API_KEY` only warns. The difference is
  intent: an unset key means "I have not set this up", while a misspelled
  provider name means "I asked for something specific and did not get
  it", and silently running with fewer providers than requested is the
  kind of thing nobody notices for months.
- **`METADATA_PROVIDERS=` (empty) disables enrichment** and is the
  documented way to run with no outbound calls at all — worth having
  explicitly, since a LAN-only deployment is this project's assumed
  setting.

### Provenance

Each metadata field records where it came from: `embedded`, a specific provider
name, or `manual`. This is what makes it safe to re-run enrichment without
clobbering a hand-fixed value.

### Reaching it from the UI

Enrichment is **per book, on request**, from the detail page: a button
that queues a job, a pending state that polls, and a result naming the
fields it wrote. Nothing enriches automatically — not on scan, not on a
schedule, and there is no library-wide "enrich everything".

**Status: Built**, and it is the one part of this project that had **no
mockup** — the handoff's plates never covered it and the section you are
reading designs the mechanism without designing a surface. That absence
shaped the step rather than being papered over: the rule followed was to
add the smallest surface answering the questions enrichment actually
creates, reusing existing patterns instead of inventing new ones. There
are exactly three, one per question:

1. *Where did this value come from?* — a provenance marker.
2. *Can I fetch metadata for this book now?* — the trigger.
3. *Did it do anything?* — the result it swaps into.

Anything past those three would have been invention. So a library-wide
enrich has no honest progress display short of building one; an
enrichment history page would be a page nobody opens, since unlike a send
— an irreversible outbound act you may need to prove happened —
enrichment is repeatable and its result is visible in the fields
themselves; and provenance is not editable, because a source is a fact
about where a value came from rather than a setting.

Two decisions inside those three are worth recording:

- **Provenance is shown only where it is not obvious.** A marker appears
  for a provider's name and for nothing else. Every field has a source,
  and rendering all seven would double the metadata block's weight to say
  "embedded" seven times — the default, and therefore not information.
  The marker is a *caveat*: a value read out of a file is a fact about
  the file, a value someone typed is theirs, and a value a third-party
  API guessed at is the only one whose origin changes how much to trust
  it. `manual` renders nothing deliberately, even though it is the source
  the resolver cares most about — the person who typed it does not need
  telling. Editing a field clears its marker because a save sets the
  source to `manual` — a consequence of the rule, not a special case.
- **"Nothing to add" is a success.** It is the ordinary outcome for a
  book whose embedded metadata is already complete, and for any book no
  provider had an answer for. Rendering it as a failure would train
  people to distrust a feature working exactly as intended.

Making the result name the fields — rather than just saying "done" —
needed the job row to record what it wrote, which the queue's original
schema did not carry. It records what was *written*, not what the
resolver proposed: the two differ whenever the write's own recheck skips
a field a concurrent edit has since filled, and the line exists to say
what that run did.

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

**Status: Built, all of it.**

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

**Field sources**, **Recipients** and **Send log** are now built, and each
carries one decision worth recording here because it is not obvious from
the field list above.

`recipients.address` is `COLLATE NOCASE` with a unique index, so re-adding
an address that differs only in case returns the existing row rather than
failing — that is a user slip, not an error.

`send_log.book_id` is the schema's one non-cascading foreign key: `ON
DELETE SET NULL`, with `book_title` denormalised beside it. Every other
foreign key here cascades because those rows are meaningless without their
book, but a send log entry is *the record that a thing happened*, and the
scanner deletes books routinely. Cascading would erase the evidence a book
was ever sent, which defeats the history's whole purpose of answering "did
I already put this on the Kindle?". `status` carries a `CHECK` closing it
to the four states below, so a typo in a Go constant fails at the write
rather than producing a job no worker ever claims.

`field_sources` keys on `(book_id, field)` with a closed `CHECK` on
`field` and a deliberately open `source` column — `embedded` and `manual`
are the sources today, while provider names are compile-time registrations
that do not exist yet.

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

**Status: Built.** The primary action this project exists for is reachable
from the app.

`internal/resend` sends one attachment through Resend's API and enforces
the ~28MB limit derived above before attempting the send, with the
arithmetic in a comment as this document asks. `internal/sender` is the
queue worker: a single `Worker` claims `send_log` rows one at a time, in
queue order, and hands each to a transport. `cmd/server` wires it when
both `RESEND_API_KEY` and `RESEND_FROM` are set; either one missing logs a
warning naming it and disables sending, because browsing must still work
on a dev machine with neither.

One worker, deliberately. Concurrency buys nothing at a handful of sends a
week, and it is what keeps the attachment a bound on the whole process
rather than a per-request multiplier — the send body is held unstreamed in
memory, so a second in-flight send would double that.

**Resolution happens at send time, not enqueue time.** A queue is a
promise to act later and the library moves underneath it, so the worker
resolves the book to a file when it claims the job. No location left
(pruned, or every copy currently missing) fails the job; a failure to read
the *index* is reported separately, because a storage error says nothing
about whether the book is still there.

Two rules are easy to get backwards and are deliberate:

- **Interrupted jobs fail; they never requeue.** A job in flight when the
  process dies is left `sending`, and startup recovery fails it rather
  than putting it back on the queue. Which side of the request it died on
  is unknowable, and requeueing risks a silent duplicate delivery, while
  failing surfaces the ambiguity and leaves retry a click away.
- **Retry is a new row.** The retry button enqueues again rather than
  mutating the failed row back to `queued`, so the log keeps the fact that
  the first attempt failed — which is what makes it a history rather than
  a status field.

The line those two are drawn along is whether the outcome is *known*. A
send Resend accepted, and a failure decided locally, are both definite, so
the terminal write happens on a context detached from shutdown — losing a
verdict already reached would let recovery rewrite a delivered book as
failed and invite a duplicate send. Only the genuinely unknown case, a
transport call abandoned mid-flight, is left for startup recovery.

The recipient picker works as described: saved addresses ordered by
`last_used_at DESC`, so the default is simply the first option and no
ordering logic lives in the transport. `last_used_at` is bumped at
enqueue, not on delivery — "most recently used" means "the one I last
chose", and a failed send must not silently reset the default to a
different address. Adding an address inline is secondary as intended,
except with zero saved recipients, where it is the only thing shown.

The **history view** at `/history` answers the question this section says
the log exists for — "did I already put this on the Kindle?" — across the
library rather than per book. It reads `send_log`'s denormalised
`book_title` and `recipient_address` directly instead of joining `books`
and `recipients`, which is the whole reason those columns are
denormalised: a book the scanner has since pruned, or an address since
removed, must still appear in its own history, and a join would silently
drop exactly those rows.

Its window is a trailing 30 days capped at 500 rows, and **the scope line
names the cap when it bites** ("last 30 days · 500 most recent"). That is
not decoration. This page has one job, and its two wrong answers are not
symmetric: a false "yes" costs a moment's doubt, while a false "no" causes
a duplicate delivery — the exact failure the job model contorts itself to
avoid when it fails interrupted jobs rather than requeueing them. A fixed
"last 30 days" over a silently truncated list would reintroduce in the UI
what the queue was designed to prevent. At the volumes above, 500 rows is
roughly a decade of sends, so the second form should never appear; it
exists so that if the assumption is ever wrong, the page degrades into
telling the truth rather than into confidently omitting rows — and doubles
as the signal for when paging or filtering becomes worth building.

**Recipient management** was a subtler gap, and the resolution is worth
recording because the defect was in this design rather than in its
implementation. "No separate management UI" above is a decision and
remains the right one — but its consequence had not been thought through:
a mistyped address, once saved, was permanent and sat in the picker
forever, because saving happens as a side effect of sending and nothing
could remove a row. The fix is a remove control on the picker itself, not
the management screen this section rules out — so the decision stands and
its consequence is repaired.

There is deliberately no *edit*: removing a wrong address and adding the
right one is the same number of actions, and needs no second validation
path and no question about history rows written under the old spelling.
Removal never touches `send_log`, which the schema guarantees rather than
the code remembering to — `recipient_address` is a plain string, not a
foreign key, precisely so that forgetting an address cannot erase the
record that something was sent to it
(`docs/plans/completed/2026090303-send-history-and-recipient-removal.md`).

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
- inline metadata editing, per field, on the book detail page
- the fetch-metadata button doing the same as the send button, for an
  enrichment job

**Status: Built.** All four htmx interactions this section names now
exist — the fourth was added with the enrichment UI and is deliberately
the same shape as the second, because a queued background job against one
book is the same thing whichever queue it lands on.

Built: `internal/web` serves the library grid at `GET /`, a book detail
page at `GET /books/{id}` and the send history at `GET /history`, with
templates, CSS and a theme script
embedded via `go:embed` and no build step, translated from the mockups in
`UI.md`/`ui-handoff/`. Cover thumbnails are served from the covers
directory; both the covers and static mounts refuse to generate directory
listings and carry cache validators. Templates render into a buffer
before anything is written, so a template error is a clean 500 rather
than a truncated page.

The catch-all caveat this section used to carry is fixed: the library
page matches only `/` exactly and the mux owns its 404s
(`docs/plans/completed/2026083112-web-transport-correctness.md`), and
`/books/{id}` is now that route — a non-numeric or unknown id 404s rather
than silently rendering the whole library.

htmx is vendored (`docs/plans/completed/2026090106-full-text-search.md`).
Search-as-you-type narrows the grid through a partial swap on a `q`
parameter; the send control swaps into a status box that polls until the
job reaches a terminal state, then stops by construction because a
terminal fragment carries no trigger; the enrichment control does the
same, sharing that state machine rather than repeating it; and inline
editing swaps one field at a time between a read view and its editor.

Every one of the four degrades without JavaScript, and by the same
method: one markup path rather than a parallel no-JS path that drifts. A
read affordance is an `<a>` carrying both an `href` and an `hx-get`, an
editor is a `<form>` carrying both an `action` and an `hx-post`, and the
plain-navigation response is a whole page rather than a fragment.

Two corrections to what this section used to claim:

- The `Vary` header is `HX-Request, HX-History-Restore-Request`, not
  `HX-Request` alone. htmx sets both on the request it issues restoring a
  history entry that has fallen out of its cache, and swaps *that*
  response into the whole document body — so answering it with a fragment
  strips the page down to it. Both headers therefore decide the response
  and both have to be named.
- **A rejected inline edit answers 200, not 4xx.** The vendored htmx does
  not swap a 4xx, so an honest status on a fragment leaves the editor
  untouched and makes Save look like it did nothing. The full-page path
  keeps its 422, where nothing swallows it. This is the one place the UI
  trades an accurate status code for a working interaction, and it is
  worth stating rather than rediscovering.

The multi-location badge is built, so the grid and the detail page now
say the same thing at two altitudes: the card marks that a book sits at
more than one path, and the detail page enumerates which
(`docs/plans/completed/2026090107-book-detail-page.md`,
`docs/plans/completed/2026090302-multi-location-badge.md`).

The masthead carries a nav, which it did not need while there was one
page. Both its items — the current-page marker and the right-hand note —
are passed in by the handler rather than decided in the template, so
`/history` can put its scope line where the library puts its book count
without the partial branching on which page is rendering it.

### Layering for a future API

A programmatic API is expected later (**not** OPDS — specific features TBD).

Because HTML and JSON responses differ, the shared layer is **not** the HTTP
handlers — it is a **service layer beneath them**. Handlers stay thin: parse
request, call service method, render. A future `/api/v1` is then a second thin
transport over the same service calls, and no business logic ends up trapped
inside a template handler.

**Status: Built.** `internal/service` sits beneath `internal/web` as
described, and the handlers are thin over it. The surface has grown with
each feature rather than the handlers growing logic: `ListBooks`,
`SearchBooks`, `CountBooks` and `GetBook` for browsing;
`UpdateBookMetadata` for editing, which owns the validation and
normalisation rules; and `Recipients`, `QueueSend`, `SendState`,
`LatestSend`, `SendHistory` and `RemoveRecipient` for sending, where
`QueueSend` owns address parsing and the enqueue and `SendHistory` owns
the window and whether it truncated. The `/api/v1` transport itself
remains deferred — but the layer it would sit on has now been exercised
by every feature since it was built rather than asserted by one.

The split has held under a case that would have exposed it: the history
window is computed from the service's own clock, so the rule about what
"recent" means lives beside the query, while rendering a timestamp as
"yesterday, 22:41" stayed in the transport as a pure function of two
times. Presentation did not migrate into the service to reach a clock,
and no clock was injected into the transport to keep it testable.

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

**One qualification, added when the first state-changing routes shipped.**
"Trust the network" is a claim about who can *reach* the server, and a
browser breaks it: any page the user visits can issue a form POST to a
LAN or localhost address its author cannot reach, with no CORS preflight
in the way. With no login, network position is the only thing standing
between the collection and everyone else, and a send POST carries the
destination address in its body. Every state-changing route — the send,
the metadata edits, and removing a recipient — therefore rejects a request
the browser itself reports as cross-site, via `Sec-Fetch-Site`. A request
carrying no fetch metadata is allowed through: a client that sends none
is not the ambient-authority vector this guards, and failing closed there
would cost the UI for no security gain.

This is not authentication and does not weaken the case for having none.
It closes the one hole that "internal network only" does not actually
cover.

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
of scope. Only the six items above are decisions rather than backlog.

That distinction now costs nothing at all to maintain, because the "in
scope, not reached" set is **empty**. The send job model, the filesystem
watcher, the send history view, the multi-location badge and finally
provider enrichment have all left it. Everything designed in this
document is either built or on the list above, and the `field_sources`
rows that had no reader at all when they were introduced now have two.

Three things ruled out *within* enrichment are decisions of the same kind
as the six above, and belong here rather than reading as gaps:

- **Automatic enrichment on scan**, and on any schedule. The first thing
  a person should see is enrichment they asked for, on a book they chose.
  It is a small change to make it automatic once the manual path has been
  watched working, and much easier to add than to take back.
- **A library-wide enrich.** The queue already supports it; the missing
  piece is an honest progress display for something that takes hours
  behind a rate limiter, which is its own piece of work.
- **An enrichment history page.** `/history` exists for sends because a
  send is an irreversible outbound act you may need to prove happened.
  Enrichment is repeatable and its result is visible in the fields
  themselves.

If the first of those ever arrives, one thing has to arrive with it: a
ceiling on how many times a book is asked about, and something in the job
record that can tell "asked, and nothing exists to find" from "asked, and
every provider was down". Today a run in which every provider failed is
recorded exactly like an honest no-match, which is fine while a person is
the trigger and wrong the moment anything else is
(`docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`).
