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
- Schema so far: `books`, `authors`, `book_authors` (join table).
- `cmd/server` — entrypoint. Opens the database (`DB_PATH` env var, default
  `./data/library.db`) and serves one route, `/healthz`, on `ADDR` (default
  `:8080`).

Nothing else from DESIGN.md exists yet: no scanner, no metadata providers, no
send-to-Kindle, no web UI.
