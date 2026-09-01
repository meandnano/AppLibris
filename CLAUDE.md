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
- Schema so far: `books`, `authors`, `book_authors` (join table), plus an
  index on `books.file_path` for the scanner's per-file lookup.
- `internal/epub` — reads embedded EPUB metadata (title, authors, language,
  ISBN, description) from the OPF package inside the zip. No cover
  extraction yet.
- `internal/scanner` — walks the library directory and syncs it into
  `internal/storage`. Cheap path+size+mtime check skips unchanged files;
  content hash (SHA-256) is identity, so a moved/renamed file updates its
  existing row instead of creating a duplicate. New EPUB files get embedded
  metadata via `internal/epub`; FB2 files are indexed (format, filename as
  title) but don't get embedded metadata parsed yet.
- `cmd/server` — entrypoint. Opens the database (`DB_PATH` env var, default
  `./data/library.db`), runs a synchronous full scan of `LIBRARY_DIR`
  (default `./library`) before serving, then reruns it on a
  `SCAN_INTERVAL` timer (default `15m`) in the background. Serves one route,
  `/healthz`, on `ADDR` (default `:8080`).

Still missing from DESIGN.md: EPUB/FB2 cover extraction, metadata provider
enrichment (Open Library / Google Books), the filesystem watcher (the
periodic rescan is the only live-update mechanism so far), near-duplicate
detection, format conversion, send-to-Kindle, and the web UI.

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
