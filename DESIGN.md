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

## Duplicate detection

**v1: byte-identical only.** Same content hash at two paths is one entry with
multiple file locations. The UI flags the entry as having multiple files.

Near-duplicates — the same book re-downloaded with different compression, or
different editions — are deferred. Matching those requires normalised title +
author + ISBN comparison and should surface as a suggestion rather than an
automatic merge, since false positives (omnibus editions, different translations)
are annoying to undo.

## Metadata

### Sources, in order

1. **Embedded** — the OPF package inside the EPUB gives title, author, language,
   sometimes ISBN, and often a cover. Many books will need nothing further.
2. **Providers**, chained.

Enrichment is **optional and never blocks a book from appearing in the library.**
The scanner and index are the source of truth; enrichment is a background job
queue running against existing records.

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
| `file_path` | mutable attribute |
| `format` | |
| `file_size` | |
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

## Conversion

Not required for the primary flow (Amazon accepts EPUB directly), so it is a
"someday" feature. But the model accommodates it now:

A converted file is a **separate book entity** with `derived_from` pointing at the
source. It **skips enrichment** and copies metadata from its parent. The UI shows
both rows with a format label, so the same book in two formats appears twice.

This avoids the failure mode where the two entities get independently enriched
into disagreeing metadata, or where fixing an author on one leaves the other
wrong. If the rows should ever collapse into one, the link is already there.

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

## Web UI

**Server-side rendered Go templates + htmx.** No JSON API for the UI, no
JS framework. Templates, htmx, and CSS embedded via `go:embed` — no build step,
nothing node-shaped in the container.

htmx is used only where dynamism is actually needed:

- search-as-you-type against the FTS5 index
- the send-to-Kindle button swapping into a status indicator that polls the job
- inline metadata editing on a book row

### Layering for a future API

A programmatic API is expected later (**not** OPDS — specific features TBD).

Because HTML and JSON responses differ, the shared layer is **not** the HTTP
handlers — it is a **service layer beneath them**. Handlers stay thin: parse
request, call service method, render. A future `/api/v1` is then a second thin
transport over the same service calls, and no business logic ends up trapped
inside a template handler.

## Authentication

None. Internal network only. Bind it and trust the network.

## Deferred list

- Series
- Tags
- Format conversion
- Near-duplicate detection
- Programmatic API
- Authentication / user management
