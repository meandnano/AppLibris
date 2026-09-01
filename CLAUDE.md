# library

A self-hosted ebook library server: browse a book collection and send titles
to a Kindle by email. See [DESIGN.md](DESIGN.md) for the full design.

## Current implementation

- `internal/storage` — SQLite storage layer (`modernc.org/sqlite`). WAL mode,
  foreign keys on. A read pool (`DB.Read()`) and a single-connection write
  pool (`DB.Write(ctx, fn)`) serialize writes without a hand-rolled
  goroutine/channel. Migrations are embedded SQL files under
  `internal/storage/migrations/`, one statement per file, named
  `YYYYMMDDNN_description.sql`, applied in filename order inside individual
  transactions and tracked in a `schema_migrations` table. `storage.Open` is
  idempotent — safe to call on every process start.
- Schema so far: `books` (identity and metadata; no location fields),
  `authors`, `book_authors` (join table), and `book_files` — one row per
  physical file location, keyed by `book_id`, so byte-identical content at
  more than one path is one `books` row with multiple `book_files` rows
  rather than a single mutable `file_path`.
- `internal/epub` — reads embedded EPUB metadata (title, authors, language,
  ISBN, description) from the OPF package inside the zip, and extracts the
  declared cover image (EPUB3 `properties="cover-image"`, falling back to
  EPUB2's `<meta name="cover">`) as raw bytes.
- `internal/cover` — turns a raw cover image into the stored thumbnail:
  resized to ~400px on the long edge (never upscaling), JPEG, written to a
  derived directory keyed by content hash.
- `internal/scanner` — walks the library directory and syncs it into
  `internal/storage`. Cheap path+size+mtime check (against `book_files`)
  skips unchanged files; content hash (SHA-256) is a book's identity, so
  known content found at a not-yet-seen path gets an additional
  `book_files` row rather than a new book — this covers a moved/renamed
  file and a genuine duplicate location alike, since the two are
  indistinguishable from a single path's perspective. New EPUB files get
  embedded metadata and a stored cover via `internal/epub`/`internal/cover`;
  FB2 files are indexed (format, filename as title) but don't get embedded
  metadata or covers parsed yet. Missing-file handling (a `book_files` row
  whose path no longer exists on disk) is not implemented — such a row is
  simply left stale.
- `internal/service` — the layer beneath HTTP handlers DESIGN.md's
  "Layering for a future API" calls for, so a future `/api/v1` can reuse it
  as a second thin transport alongside `internal/web`. One method so far:
  `ListBooks`, assembling `internal/storage`'s books and authors into a
  `BookSummary` per book.
- `internal/web` — the browser UI's HTTP transport: thin handlers over
  `internal/service`, `html/template` templates and CSS/JS embedded via
  `go:embed` (`internal/web/templates/`, `internal/web/static/`), no build
  step. `GET /` renders the library grid — the first real page, translated
  from Claude Design's mockups (see `UI.md`, kept on the `init`
  branch/worktree, not on `master`); `GET /static/` serves the embedded
  stylesheet and theme script; `GET /covers/` serves the scanner's stored
  cover thumbnails out of `COVERS_DIR` (runtime data, so it's passed into
  `Routes` rather than embedded). Handlers map `service.BookSummary` onto a
  small per-page view model so templates stay logic-free. Book detail,
  search, inline metadata editing and send-to-Kindle are designed but not
  built — each needs backing features that don't exist yet.
- `cmd/server` — entrypoint. Opens the database (`DB_PATH` env var, default
  `./data/library.db`), runs a synchronous full scan of `LIBRARY_DIR`
  (default `./library`) against `COVERS_DIR` (default `./data/covers`)
  before serving, then reruns it on a `SCAN_INTERVAL` timer (default `15m`)
  in the background. Serves `/healthz` and mounts `internal/web`'s routes
  at `/`, on `ADDR` (default `:8080`).

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
