# Design

Project-level decisions and the startup rules of `cmd/server`.

## Purpose

- **One user. A family member is a saved recipient address, never a second account.**
- **Simplicity beats generality: no multi-tenancy, no permissions, no configuration beyond environment variables.** A sole maintainer on an internal network needs none of them.

## Constraints

- **Written in Go.**
- **One container, no external processes: no database server, sidecar or broker.** A NAS box runs one image and nothing else.
- **Embedded, file-backed storage.** Backup is copying a directory.
- **No JavaScript build step and no `node_modules`. Templates, CSS and the vendored htmx are embedded in the binary.**
- **`CGO_ENABLED=0`.** A static binary cross-compiles trivially.
- **The image base is `distroless/static` (`nonroot`), not `scratch`.** Scratch lacks the CA certificates, tzdata, writable `/tmp` and non-root uid the app needs; distroless still has no shell or package manager.

## Library directory

- **Originals are never modified. Writes only ever create new paths.**
- **A derived file lands beside the originals as a new path, registered in the index in the transaction that created it.** The next sweep then sees a known content hash.
- **The one path written today is an imported book in the library root, named from the offered file: `<name>.part`, then `os.Link` onto the first free name, then `IndexFile` in the confirming request.** No sweep indexes `.part`, and a link fails where `os.Rename` silently replaces. See `docs/notes/import.md`.
- **A read-only library is a legitimate deployment.** The scanner only reads it, so importing is probed at startup and offered or explained, never assumed.
- **"Importing is offered" means `probeWritable` on `LIBRARY_DIR` succeeded and the staging directory under `os.TempDir()` could be created; `newStager` builds an `importer.Stager` only then.** Either failing is a Warn, never a startup failure: a library that can be read is still worth serving.
- **A flat pile: no folder conventions, no directory-as-metadata heuristics.** A folder name is a guess about the file; the file's metadata is not.
- **Covers live in a separate directory keyed by content hash, only the path in the database, and the directory is disposable.** An extracted cover is rebuilt by the next sweep; a provider's is fetched again from the book's page.

## Startup and shutdown

- **`LIBRARY_DIR` goes through `requireExistingDir` (stat, then `EvalSymlinks`) and is never created. `COVERS_DIR` and `DB_PATH`'s directory go through `resolveDir` (create, then `EvalSymlinks`).** Creating the library is the one call that fails a read-only mount, and an absent library is a misconfiguration to name rather than an empty grid to serve.
- **A dangling symlink in any component fails startup naming link and target. A refused `MkdirAll` names both uids.** `MkdirAll` on a dangling link names the link and not the missing target, which is the whole question when a volume did not mount.
- **The server serves immediately. The scan loop, sender worker, enrichment worker and import janitor run on one cancellable `scanCtx`; the janitor exists only when the importer does.** A first sweep is minutes of hashing; a readiness probe would otherwise restart the container.
- **Shutdown order: HTTP server, then `waitForBackground` over each goroutine with a 10s deadline, then the database.** A goroutine past its deadline would otherwise write onto a closed connection.
- **`MAX_IMPORT_SIZE` is parsed through `parseByteSize`.**
- **Logging is `log/slog` on stderr through the package-level functions, levelled once in `cmd/server` from `LOG_LEVEL`.**

## Storage engine

- **SQLite through `modernc.org/sqlite`, a pure-Go port, not a key-value store, and its slower concurrent writes are accepted.** FTS5 gives full-text search out of the box; writes arrive in scan bursts and reads dominate; the pure-Go driver is what keeps `CGO_ENABLED=0` true.

## Conversion model

- **Conversion is not built. `books.derived_from` exists and nothing writes it.** Adding conversion is then not a migration of every book.
- **A converted file is a separate book row pointing at its source through `derived_from`, never a second file on one book. It skips enrichment, copies the parent's metadata, and the UI shows both rows with a format label.** One shared row is what lets independent enrichment write disagreeing metadata, or an author fix on one leave the other wrong.

## Authentication

- **None. The server trusts its network. No rate limiting, no request logging.** All three hold only under that assumption; exposure beyond a trusted network makes this the first decision to change.
- **Every state-changing route rejects a request the browser reports as cross-site, and the server must sit behind HTTPS for that report to exist.** Any page a person visits can POST to a LAN address its author cannot reach. This is not authentication. Mechanism in `docs/notes/web.md`.

## Deferred by decision

None of these is backlog and none is started.

- **Series.** A real relation rather than a flag, so the one that hurts most to retrofit; acceptable for a mostly standalone library.
- **Tags.**
- **Format conversion.** Amazon accepts EPUB directly. The conversion model above is its shape if it arrives.
- **Near-duplicate detection.** Byte-identical duplicates merge by content hash. Other editions need normalised title, author and ISBN comparison, surfaced as a suggestion, since a false positive such as an omnibus is annoying to undo.
- **Programmatic API.** Expected later, not OPDS. The service layer is its second thin transport; `StageImport` then `ConfirmImport` is already a one-shot import. It needs a bearer-token credential of its own and routes that bypass `sameSiteOnly`, since a non-browser client never sends `Sec-Fetch-Site`.
- **Authentication and user management.** See above.
- **Automatic enrichment on scan or on a schedule.** The first enrichment a person sees is one they asked for; automatic is a small change and a hard one to take back. If it arrives, a per-book attempt ceiling arrives with it: `docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`.
- **A library-wide enrich.** The queue supports it; what is missing is an honest progress display for hours of work behind a rate limiter.
- **An enrichment history page.** A send is an irreversible outbound act one may need to prove; enrichment is repeatable and visible in the fields themselves.
