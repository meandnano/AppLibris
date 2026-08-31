# library

A self-hosted ebook library server: browse a book collection and send titles
to a Kindle by email. See [DESIGN.md](DESIGN.md) for the full design.

## Current implementation

- `internal/storage` — SQLite storage layer (`modernc.org/sqlite`). WAL mode,
  foreign keys on. A read pool (`DB.Read()`) and a single-connection write
  pool (`DB.Write(ctx, fn)`) serialize writes without a hand-rolled
  goroutine/channel. Each exported write method owns one transaction; a
  multi-step atomic write composes the package-internal `…Tx` helpers (e.g.
  `createBookTx`) inside a single `DB.Write` call instead of calling two
  exported methods — a `Write` callback must never call an exported `*DB`
  method, since the pool's one connection is already held by the outer
  call, and nesting deadlocks until the context expires. A directly nested
  `Write` call is instead caught and returns `ErrNestedWrite`. Migrations
  are embedded SQL files under
  `internal/storage/migrations/`, one statement per file, named
  `YYYYMMDDNN_description.sql`, applied in filename order inside individual
  transactions and tracked in a `schema_migrations` table. `storage.Open` is
  idempotent — safe to call on every process start.
- Schema so far: `books` (identity and metadata; no location fields),
  `authors`, `book_authors` (join table), and `book_files` — one row per
  physical file location, keyed by `book_id`, so byte-identical content at
  more than one path is one `books` row with multiple `book_files` rows
  rather than a single mutable `file_path`. `books.sort_title` is derived
  from the title rather than a copy of it — one leading English article
  stripped, case folded — and the column is declared `COLLATE NOCASE`, so
  any `ORDER BY sort_title` is case-insensitive without a per-query clause.
  `book_files.book_id` and both `book_authors` foreign keys are `ON DELETE
  CASCADE`, so deleting a book takes its file locations and author links
  with it (the author row itself survives). `authors.name` has a unique
  index. Every timestamp column is written as fixed-width UTC RFC 3339 text
  (`sqliteTimeLayout`/`formatTime` in `internal/storage`), so SQLite's own
  date functions and a plain `ORDER BY` both work on it.
- `internal/epub` — reads embedded EPUB metadata (title, authors, language,
  ISBN, description) from the OPF package inside the zip, and extracts the
  declared cover image (EPUB3 `properties="cover-image"`, falling back to
  EPUB2's `<meta name="cover">`) as raw bytes.
- `internal/cover` — turns a raw cover image into the stored thumbnail:
  resized to ~400px on the long edge (never upscaling), JPEG, written to a
  derived directory keyed by content hash. `Store` creates that directory
  on demand and writes through a same-directory temporary file plus atomic
  rename, so readers never observe a partial canonical cover.
- `internal/resend` — `Client.Send` POSTs one attachment to Resend's API
  (DESIGN.md's send-to-Kindle transport), enforcing the ~28MB size limit
  DESIGN.md derives before attempting a send. Nothing calls it yet — no
  `recipients`/`send_log` schema, job queue, or `RESEND_API_KEY`/
  `RESEND_FROM` wiring into `cmd/server` exists, both separate
  prerequisites for send-to-Kindle per DESIGN.md.
- `internal/scanner` — walks the library directory and syncs it into
  `internal/storage`. Cheap path+size+mtime check (against `book_files`)
  skips unchanged files; content hash (SHA-256) is a book's identity, so
  known content found at a not-yet-seen path gets an additional
  `book_files` row rather than a new book — this covers a moved/renamed
  file and a genuine duplicate location alike, since the two are
  indistinguishable from a single path's perspective. New EPUB files get
  embedded metadata and a stored cover via `internal/epub`/`internal/cover`;
  for known content, a sweep re-extracts a recorded cover whose file is
  missing or zero bytes and refreshes its stored path, making `COVERS_DIR`
  disposable. An empty stored cover path records that no embedded cover
  was found and is not retried on every sweep; a separate `cover_retry`
  marker records a transient initial store failure and retries it later.
  Cover inspection regenerates only on a missing or zero-byte file; other
  stat failures warn without re-parsing the source. FB2 files are indexed
  (format, filename as title) but don't get embedded metadata or covers
  parsed yet. A new book is created together with its
  first file location in one transaction (`storage.CreateBookWithFile`).
  Content replacing a known path's previous content reassigns that
  `book_files` row and, in the same transaction, deletes whatever book it
  left with zero locations (`storage.ReassignFileAndPruneOrphan` /
  `CreateBookWithFile` — either can orphan a path's previous owner, since
  both reassign it unconditionally), logging the deletion at Info.
  `book_files.file_path` is stored relative to `LIBRARY_DIR`
  (slash-separated), so the index survives the library being mounted at a
  different absolute path (dev's `./library` versus a container's
  `/library`); a directory `WalkDir` can't read skips its subtree and
  counts an error rather than aborting the rest of the sweep. A
  `book_files` row whose path goes missing is reconciled in two phases:
  first marked (`missing_since`, via `storage.SetFilesMissing`) rather than
  deleted outright, then actually deleted (`storage.PruneMissingFiles`,
  taking its book with it via the same orphan-pruning path as a reassigned
  file if it was the book's last location) once it's stayed missing past
  the `MISSING_GRACE` duration — a row that reappears before then has its
  mark cleared (`storage.ClearFilesMissing`) instead. `storage.
  PruneMissingFiles` does no filtering of its own (no age check, no path
  exclusion) — it deletes exactly the file IDs it's given; every guard
  below lives in the scanner, the only place with access to live
  filesystem state, and decides that ID list. Guards that keep this from
  misreading a transient failure as deletion: a row is only eligible if
  `os.Lstat` on it fails with `fs.ErrNotExist` specifically (any other
  error, e.g. a path component that's no longer a directory, only logs a
  warning), and this check runs fresh *every* sweep — including for a row
  already marked from an earlier sweep — so a row whose failure mode
  later changes (`ErrNotExist` to `EACCES`, say, or to a directory now
  sitting at that path) can never be deleted on the strength of a
  confirmation that's since gone stale; a row under a directory the sweep
  couldn't read *this* sweep is left untouched, at both mark and prune
  time (`Scan` tracks these as `skippedDirs`, a negative list — `WalkDir`'s
  callback only ever reports directory-read failure as a second,
  error-bearing invocation, so a positive "cleanly read" list isn't
  obtainable from the API); and reconciliation is skipped entirely if the
  sweep visited zero files (an unmounted volume can present as an empty
  directory, so seeing nothing is not evidence that everything is gone),
  logged at Warn.
- `internal/service` — the layer beneath HTTP handlers DESIGN.md's
  "Layering for a future API" calls for, so a future `/api/v1` can reuse it
  as a second thin transport alongside `internal/web`. One method so far:
  `ListBooks`, assembling `internal/storage`'s books and authors into a
  `BookSummary` per book.
- `internal/web` — the browser UI's HTTP transport: thin handlers over
  `internal/service`, `html/template` templates and CSS/JS embedded via
  `go:embed` (`internal/web/templates/`, `internal/web/static/`), no build
  step. `GET /{$}` renders the library grid — the first real page,
  translated from Claude Design's mockups (see `UI.md`, kept on the `init`
  branch/worktree, not on `master`) — `{$}` matches only the exact path, so
  the mux's own 404 handles everything else, including a stale or mistyped
  `/books/{id}`; `GET /static/` serves the embedded stylesheet and theme
  script; `GET /covers/` serves the scanner's stored cover thumbnails out of
  `COVERS_DIR` (runtime data, so it's passed into `Routes` rather than
  embedded). Handlers map `service.BookSummary` onto a small per-page view
  model so templates stay logic-free. `render` executes into a buffer
  before writing anything to the response, so a template error is a clean
  500 rather than a truncated page, and sets `Content-Type` explicitly
  rather than relying on sniffing. Only the pre-write `ExecuteTemplate`
  error is ever returned to the handler: once `Content-Type` is set and the
  buffer starts writing to the response, the response is committed, so a
  write failure past that point (almost always the client disconnecting)
  is logged inside `render` rather than returned — a handler reacting to
  it with `http.Error` would double-write onto an already-committed
  response. Book detail, search, inline metadata editing and
  send-to-Kindle are designed but not built — each needs backing features
  that don't exist yet.
- `cmd/server` — entrypoint. `main` sets up logging and a
  `signal.NotifyContext` (SIGINT/SIGTERM) and calls `run(ctx) error`, so
  every failure path has one exit point (`slog.Error` + `os.Exit(1)`).
  `run` opens the database (`DB_PATH` env var, default `./data/library.db`)
  and starts serving immediately — `/healthz` and `internal/web`'s routes
  at `/`, on `ADDR` (default `:8080`), with `ReadHeaderTimeout`,
  `ReadTimeout`, `WriteTimeout` and `IdleTimeout` all set — rather than
  blocking startup on a scan; the initial full sweep of `LIBRARY_DIR`
  (default `./library`) against `COVERS_DIR` (default `./data/covers`)
  runs in the background alongside the `SCAN_INTERVAL`-timed (default
  `15m`) periodic rescan, with missing-file grace period `MISSING_GRACE`
  (default `24h`). Both scan loops take the signal-aware context, so a
  sweep in progress unwinds on shutdown instead of being killed mid-write.
  On SIGINT/SIGTERM, `run` shuts the HTTP server down first (bounded to
  10s) and only then closes the database — order matters, so no request is
  ever mid-query when the database closes. The image is built on
  `distroless/static-debian12:nonroot` (CA certificates, tzdata, `/tmp`,
  and a non-root uid, none of which `scratch` provides), so mounted
  volumes must be writable by that uid.
- Logging goes through `log/slog` (a text handler on stderr), leveled via
  the `LOG_LEVEL` env var (default `INFO`) set once in `cmd/server` and
  used everywhere else via `slog`'s package-level functions against that
  default logger.

Still missing from DESIGN.md: the book detail page, search, inline metadata
editing and send-to-Kindle (designed, not built), FB2 cover/metadata
extraction, metadata provider enrichment (Open Library / Google Books), the
filesystem watcher (the periodic rescan is the only live-update mechanism so
far), near-duplicate detection, and format conversion.

## Planning

Each implementation step is planned in its own file under
`docs/plans/<YYYYMMDDNN-description>.md` (e.g.
`docs/plans/2026083001-covers.md`) — `NN` a same-day sequence number, same
scheme as the migration filenames. Once a plan's step has been implemented,
move its file into `docs/plans/completed/`.

Plans in `docs/plans/completed/` are immutable: never edit one after it's
moved there, even to fix a mistake found later. If a problem is discovered
in a completed plan, write a new plan for the fix instead of rewriting the
old one.

## Backlog

`docs/backlog/` holds known work that is worth doing but isn't top
priority — things that don't corrupt data, don't block another step, and
aren't visibly wrong in the shipped app today. Anything that *does* meet
one of those bars belongs in `docs/plans/` instead, not here.

Backlog files use the same `<YYYYMMDDNN-description>.md` naming as plans,
sharing one same-day `NN` sequence with them so a number identifies exactly
one file across both directories.

A backlog item is a **problem statement, not an approved plan**: it records
what's wrong and why it was judged non-urgent, with only a sketch of a fix.
It is never implemented directly. To act on one, first re-validate it
against the current code — the finding may have been fixed in passing,
changed shape, or become urgent since it was written — then write a real
plan under `docs/plans/` and delete the backlog file in the same change.
