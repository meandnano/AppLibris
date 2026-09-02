# library

A self-hosted ebook library server: browse a book collection and send titles
to a Kindle by email. See [DESIGN.md](DESIGN.md) on `init` branch for the
full design.

## Current implementation

- `internal/storage` — SQLite storage layer (`modernc.org/sqlite`). WAL mode,
  foreign keys on. A read pool (`DB.Read()`), bounded to `readPoolSize` (8)
  open and idle connections so concurrency above `database/sql`'s default
  idle-2 ceiling reuses pooled connections instead of opening and
  discarding a fresh one per request, and a single-connection write pool
  (`DB.Write(ctx, fn)`) serialize writes without a hand-rolled
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
  with it (the author row itself survives). `book_authors` carries a
  `position` so a book's authors keep the order its source file listed
  them in — `author_id` order is first-sight-in-the-library order, which is
  not the same thing once an author is shared between books. A name
  credited twice in one file links once, at its first position, rather
  than failing the insert. `authors.name` has a unique
  index. Every timestamp column is written as fixed-width UTC RFC 3339 text
  (`sqliteTimeLayout`/`formatTime` in `internal/storage`), so SQLite's own
  date functions and a plain `ORDER BY` both work on it.
- `books_fts` is an FTS5 virtual table (`title`, `authors`, `description`,
  `isbn`, `tokenize='unicode61 remove_diacritics 2'`) — a plain table, not
  `content='books'`, since `authors` isn't a books column to begin with
  (it's assembled from a join) and a contentless table can't support the
  delete-by-rowid the sync below needs. Sync is asymmetric on purpose:
  deletion is a trigger (`books_fts_after_delete`, since books die on
  several independent Go paths and every future one dies for free too),
  while insert/update is the package-internal `syncBookFTSTx(ctx, tx,
  bookID)` — recomputes the row from scratch via a `group_concat` join
  over `book_authors`/`authors` rather than tracking deltas, and runs
  inside the same `DB.Write` transaction as `createBookTx`, right after it
  (`CreateBook`, `CreateBookWithFile`) so the FTS row can see the authors.
  No other write method touches an indexed column, so nothing else calls
  it. No backfill migration exists or ever will: every row is created
  through the synced path by construction. `SearchBooks(ctx, query)`
  joins against it and orders by `sort_title`, not relevance — see
  `internal/web` below for why. `query` must already be a valid FTS5
  `MATCH` expression; `SanitizeFTSQuery` (also `internal/storage`, no DB
  access) is the one place raw user input becomes one, by quoting and
  prefix-terming every whitespace-separated token so no input, however
  adversarial, can reach `MATCH` unescaped.
- `internal/epub` — reads embedded EPUB metadata (title, authors, language,
  ISBN, description, publisher, publication date) from the OPF package
  inside the zip, and extracts the declared cover image (EPUB3
  `properties="cover-image"`, falling back to EPUB2's `<meta name="cover">`)
  as raw bytes. Publication date prefers a `dc:date` tagged
  `opf:event="publication"` among repeated elements, falling back to the
  first event-less one (EPUB3's form); `creation`/`modification` dates are
  never used. ISBN is recognised via `opf:scheme="ISBN"`, a `urn:isbn:`
  identifier, or a bare ISBN-shaped one, in that order, and returned
  normalised (hyphens/spaces stripped, prefix stripped, trailing check
  digit upper-cased) rather than as found — it's the lookup key a future
  provider chain needs. Cover hrefs are percent-decoded (and any fragment
  stripped) before the zip lookup, since a manifest href is a URI
  reference.
- `internal/fb2` — reads embedded metadata from an FB2 document, mirroring
  `internal/epub`'s `Metadata` field set and surface exactly so the scanner
  can fill it from either source with the same code shape. `PublishedDate`
  prefers `publish-info/year` (when this *edition* was published, what
  `books.published_date` means) over `title-info/date` (when the work was
  *written*, per the FB2 spec), falling back to `date`'s `value` attribute
  then its text. Authors are given as structured `first-name`/`middle-
  name`/`last-name` elements, joined with a single space into the one
  display name the `authors` table stores — the one place FB2 offers more
  structure than the schema keeps. The cover is whichever `<binary>` the
  coverpage's namespaced `l:href="#id"` points at, base64-decoded. A
  document's declared XML encoding is never trusted: `ReadMetadata` sets an
  `xml.Decoder.CharsetReader` that passes every charset through unchanged,
  since the library's FB2 files are UTF-8 regardless of what they declare
  and a bare decoder fails outright (not gracefully) on any declared
  encoding it doesn't otherwise recognise. A `.fb2.zip` archive is parsed
  the same way after opening it and locating its one `.fb2` entry; zero or
  more than one is an error rather than a guess at which book it contains.
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
  FB2 files (plain `.fb2` and `.fb2.zip` archives alike) get the same
  treatment via `internal/fb2`, both recording `format` as `fb2` regardless
  of which — how a book is packaged on disk isn't something the format
  badge in the UI should surface. Supported files are matched on filename
  *suffix* rather than `filepath.Ext`, since a `.fb2.zip` archive is two
  extensions and `Ext` would only ever see the last one. For known content,
  a sweep re-extracts a recorded cover whose file is missing or zero bytes
  and refreshes its stored path, making `COVERS_DIR` disposable. An empty
  stored cover path records that no embedded cover was found and is not
  retried on every sweep; a separate `cover_retry` marker records a
  transient initial store failure and retries it later. Cover inspection
  regenerates only on a missing or zero-byte file; other stat failures warn
  without re-parsing the source. A new book is created together with its
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
  as a second thin transport alongside `internal/web`. `ListBooks` and
  `SearchBooks` both assemble `internal/storage`'s books and authors into a
  `BookSummary` per book via a shared unexported `summarize` helper.
  `SearchBooks` sanitizes via `storage.SanitizeFTSQuery` first; a query
  that sanitizes to nothing (blank, or built only of whitespace/quotes/
  operators) is treated as `ListBooks` — the empty search box and a
  freshly-loaded page are the same state, so callers don't special-case it.
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
  embedded). Both mounts wrap their filesystem in `noDirFS` so a directory
  with no `index.html` 404s instead of `http.FileServer` generating a
  browsable listing. Neither gets `Cache-Control: immutable`: `cover.Store`
  names each file by the *book's* content hash, not a hash of the
  resized/JPEG-encoded thumbnail bytes actually served at that path, so a
  future change to the resize/encode pipeline (or a regeneration under a
  changed pipeline) can overwrite different bytes at an unchanged URL —
  `immutable` would promise the opposite. Covers instead get a day-long
  `max-age`, bounding how stale a legitimately changed thumbnail can get
  while still eliminating the redundant per-visit revalidation round trip
  the header exists to avoid; `http.FileServer`'s own `Last-Modified`
  (from the file's mtime) is what lets a client revalidate once that
  window elapses. The embedded static assets get a content-derived `ETag`
  (computed once at startup, since `embed.FS` reports a zero `ModTime` and
  `http.FileServer` would otherwise emit no validator at all) paired with
  a short, five-minute `max-age` — that bounds how long a client can serve
  a stale file after a deploy without consulting the `ETag`, rather than
  eliminating the risk outright. Handlers map `service.BookSummary` onto a
  small per-page view model so templates stay logic-free. `render` executes
  into a buffer before writing anything to the response, so a template
  error is a clean 500 rather than a truncated page, and sets `Content-Type`
  explicitly rather than relying on sniffing. Only the pre-write `ExecuteTemplate`
  error is ever returned to the handler: once `Content-Type` is set and the
  buffer starts writing to the response, the response is committed, so a
  write failure past that point (almost always the client disconnecting)
  is logged inside `render` rather than returned — a handler reacting to
  it with `http.Error` would double-write onto an already-committed
  response. Book detail, search, inline metadata editing and
  send-to-Kindle are designed but not built — each needs backing features
  that don't exist yet.
- Search-as-you-type is `GET /{$}` extended, not a separate route: a `q`
  parameter narrows the grid, and htmx (vendored at
  `internal/web/static/js/htmx.min.js`, version pinned in a comment at the
  top of the file) turns each keystroke into a debounced (`delay:300ms`)
  partial request. Whether a request gets the full page or just the
  `book-grid` fragment (a named template in `templates/partials.html`,
  alongside the new `search-bar`) depends on the `HX-Request` header htmx
  sets, so the handler always sets `Vary: HX-Request` — the same URL now
  serves two different bodies, and without the header a cache or the
  browser's back-forward cache could serve one where the other was wanted.
  The search box itself lives only in the full-page render, never in the
  fragment: `hx-target="#book-grid"` with `hx-swap="outerHTML"` means only
  the grid is ever replaced, so a keystroke mid-request is never lost to a
  render. `hx-push-url="true"` keeps the URL shareable. With JavaScript
  off, the same `<form method="get">` degrades to a normal navigation
  hitting the identical handler, so there is no separate no-JS path to
  drift out of sync. A blank or whitespace-only `q` is "not searching" —
  the plain grid, no result count, same as before this query parameter
  existed; a non-blank `q` renders either the count-and-filtered-grid state
  or a distinct `search__empty` block (`Nothing matches "<query>"`, the
  query HTML-escaped by `html/template`) if nothing matched — kept visually
  and structurally separate from the "No books yet" empty-library block,
  since the two call for different next actions.
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
  (default `24h`). The scan loops run on their own `scanCtx`, a child of
  the signal-aware context but independently cancellable — a shared
  `waitForScan` helper cancels it and waits out a bounded 10s window
  before the database closes, on *both* the SIGINT/SIGTERM path (`run`
  shuts the HTTP server down first, then waits, then closes the database
  — order matters, so no request or in-flight scan write is torn down by
  the database closing under it) and a serving failure (e.g. `ADDR`
  already in use), which used to close the database immediately and race
  the still-running scan. `internal/scanner.Scan` itself checks
  `ctx.Err()` before the walk starts and on every entry it visits, so
  cancellation stops the walk outright (and skips reconciliation) rather
  than just failing each remaining file's own DB calls one at a time. The
  image is built on `distroless/static-debian12:nonroot` (CA
  certificates, tzdata, `/tmp`, and a non-root uid, none of which
  `scratch` provides) with an explicit `WORKDIR /` — the base image's own
  default, `/home/nonroot`, would otherwise silently resolve `LIBRARY_DIR`
  et al.'s relative defaults to the wrong place — so mounted volumes must
  be writable by that uid.
- Logging goes through `log/slog` (a text handler on stderr), leveled via
  the `LOG_LEVEL` env var (default `INFO`) set once in `cmd/server` and
  used everywhere else via `slog`'s package-level functions against that
  default logger.

Still missing from DESIGN.md: the book detail page, inline metadata
editing and send-to-Kindle (designed, not built), metadata provider
enrichment (Open Library / Google Books), the filesystem watcher (the
periodic rescan is the only live-update mechanism so far), near-duplicate
detection, and format conversion.

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
