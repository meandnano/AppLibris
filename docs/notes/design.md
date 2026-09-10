# Design

Project-level decisions. Per-area rationale is in the sibling files under
`docs/notes/`; the package map and invariants are in CLAUDE.md.

## Purpose

A personal ebook library server. It solves one problem: see what books I
have, and send one to a Kindle by email. Everything else is secondary.

One user, plus a family member as an occasional send recipient. That
second person is a saved address, never a second account. The server runs
on an internal network with a sole maintainer, so every decision favours
simplicity over generality: no multi-tenancy, no permissions, no
configuration surface beyond environment variables.

## Constraints

- Written in Go.
- Ships as a single container with no external process dependencies: no
  database server, no sidecar, no message broker. A NAS box should run it
  as one image and nothing else.
- Embedded, file-backed storage, so backup is copying a directory.
- No JavaScript build step and no `node_modules`. Templates, CSS and the
  vendored htmx are embedded in the binary.
- `CGO_ENABLED=0`, so the binary is static and cross-compiles trivially.
  The image base is `distroless/static-debian12:nonroot` rather than
  `scratch` because the app needs four things scratch lacks: CA
  certificates for the outbound HTTPS calls, tzdata so local time is not
  always UTC, a writable `/tmp`, and a non-root uid. It still ships no
  shell and no package manager, so the static-binary property survives.

## Library directory

One library directory, and the originals in it are never modified. The
rule the code enforces is that writes only ever create new paths. Any
derived file the app might produce lands beside the originals as a new
path and is registered in the index in the same transaction that created
it, so the next sweep sees a known content hash rather than a mystery
arrival.

The library is a flat, unorganised pile of files. There are no folder
conventions and no directory-as-metadata heuristics, because a folder name
is a guess about the file inside it and the file's own metadata is not.

Covers live in a separate derived directory keyed by content hash, with
only the path stored in the database. The directory is disposable: a
cover extracted from a book is rebuilt by the next sweep, and one a
provider supplied is forgotten and can be fetched again from the book's
page.

## Storage engine

SQLite through `modernc.org/sqlite`, a pure-Go port. It was chosen over a
key-value store because FTS5 gives full-text search out of the box, which
is most of what a library server needs; a KV store would mean hand-rolling
every index. The pure-Go driver is slower than the C one under heavy
concurrent writes, which does not matter here: writes arrive in scan
bursts and reads dominate, and it is what keeps `CGO_ENABLED=0` true.

## Conversion model

Format conversion is not built, but the schema accommodates it so that
adding it is not a migration of every book. A converted file would be a
separate book entity with `books.derived_from` pointing at its source. It
would skip enrichment and copy its metadata from the parent, and the UI
would show both rows with a format label.

Two entities linked by a foreign key rather than one entity with two files
avoids the failure where both get independently enriched into disagreeing
metadata, or fixing an author on one leaves the other wrong. If the rows
should ever collapse into one, the link is already there. Today the column
exists and nothing writes it.

## Authentication

None. The server is bound to an internal network and trusts it. There is
no rate limiting and no request logging, and both are acceptable only
under that assumption. If the server is ever exposed beyond a trusted
network, this is the first decision that has to change, and several others
follow from it.

The one qualification is that "trust the network" describes who can reach
the server, and a browser breaks it: any page a user visits can POST to a
LAN or localhost address its author cannot reach. So every state-changing
route rejects a request the browser reports as cross-site, and the service
must sit behind an HTTPS front for that report to exist at all. The
mechanism and the deployment requirement are in `docs/notes/web.md`. This
is not authentication and does not weaken the case for having none; it
closes the one hole "internal network only" leaves open.

## Deferred by decision

These were consciously ruled out of scope. None is backlog, and none has
been started.

- **Series.** A real relation rather than a flag, so the one that hurts
  most to retrofit. Acceptable given a mostly standalone library.
- **Tags.**
- **Format conversion.** Amazon accepts EPUB directly, so the primary flow
  does not need it. See the conversion model above for the shape it takes
  if it arrives.
- **Near-duplicate detection.** Byte-identical duplicates are merged by
  content hash. Matching different compressions or editions needs
  normalised title, author and ISBN comparison and should surface as a
  suggestion, since false positives (omnibus editions, translations) are
  annoying to undo.
- **Programmatic API.** Expected later, not OPDS. The service layer
  beneath the HTTP handlers exists so it can be a second thin transport.
- **Authentication and user management.** See above.

Three things are ruled out within enrichment on the same footing:

- **Automatic enrichment on scan or on a schedule.** The first thing a
  person should see is enrichment they asked for, on a book they chose. It
  is a small change to make automatic and a hard one to take back. If it
  ever arrives, a ceiling on how many times one book is asked about must
  arrive with it; the failed-job record that such a ceiling would count is
  already in place, and the ceiling itself is tracked in
  `docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`.
- **A library-wide enrich.** The queue supports it. What is missing is an
  honest progress display for work that takes hours behind a rate limiter,
  which is its own piece of work.
- **An enrichment history page.** Send history exists because a send is an
  irreversible outbound act one may need to prove happened. Enrichment is
  repeatable and its result is visible in the fields themselves.
