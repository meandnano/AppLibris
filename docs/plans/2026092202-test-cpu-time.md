# Cut the CPU the `-race` suite spends on setup

## Context

CI's Test step takes about 2m15s. `2026092201` took about 12 s of sleeping
out of the suite and the step did not move. The suite is CPU-bound: CI runs
`go test -race ./...` on a four-core runner, packages run four at a time,
and a sleeping test costs no CPU.

Measured locally under `-race`, the packages add up to about 145 s of CPU.
Three costs in that come from test setup rather than from anything a test
asserts:

| Cost | Where | Measured |
|---|---|---|
| A fresh database with all 34 migrations for every test | about 450 tests across `web`, `storage`, `scanner`, `service`, `importer`, `enrich`, `sender` | 127 ms each under `-race` (18 ms without), about 60 s in all |
| One FB2 test parsing a 32 MiB `<binary>` twice | `TestReadMetadataOverCapCoverBinaryCostsOnlyTheCappedCopy` | 18.7 s |
| Seeding rows through one exported write each | `TestPruneMissingFilesHandlesMoreIDsThanOneSQLChunk`, `TestSendHistoryReportsTruncatedOnlyWhenCapBites`, `TestHistoryScopeLineNamesTheCapOnlyWhenTruncated`, `seedSearchableBooks` | 7 s, 3.8 s, 1.9 s, about 2 s |

Out of scope:

- The build cache is `2026092203`. It is a separate plan so this plan's
  effect on the Test step can be measured alone, against the same cold
  cache `2026092201`'s run had.
- `TestCapBlankLinesMatchesTheObviousSpelling` runs 400,000 random inputs
  (1.9 s). How many it runs is a coverage decision, not a setup cost.
- The other slow FB2 tests. `…KeepsCoverBinaryExactlyAtTheCap` and
  `…DropsCoverBinaryOverTheCap` are sized by the 8 MiB `cover.MaxCoverBytes`
  and cannot shrink. `…SkipsNonCoverBinariesWithoutHoldingThem` sizes its
  illustrations to rise above the tokeniser's buffer doubling, which the
  race build doubles again, so shrinking them weakens what it separates.

## Step 1: tests copy a migrated template database

Measured under `-race`: copying an already-migrated database file and
opening the copy takes 6.7 ms, against 127 ms for a fresh file.
`storage.Open` still runs `migrate`, which finds every migration applied.

Add a package `internal/storage/storagetest` with
`Open(t testing.TB) *storage.DB`:

- **The template is built once.** A package-level `sync.Once` opens a
  fresh database in an `os.MkdirTemp` directory, closes it, reads the main
  file into memory and removes the directory. Keeping the bytes rather
  than a path leaves nothing on disk and needs no `TestMain`. It is built
  from the current migrations on every run, so it can never go stale the
  way a committed fixture would.
- **Each call writes the bytes to `t.TempDir()/library.db`,** opens the
  copy, and registers `t.Cleanup` to close it.
- **It works inside a `synctest` bubble.** The template build opens and
  closes within the one call, so no connection opener outlives it. The
  bubble rules in `docs/notes/testing.md` already require the per-test open
  to happen inside the bubble, and it does.
- **A test pins the template.** `storagetest`'s own test compares a
  template copy with a fresh `storage.Open`: the same `schema_migrations`
  names and the same `sqlite_master` SQL. `Close` is what checkpoints the
  WAL into the main file, so a template read before its checkpoint would
  lack tables, and this test is what catches that.

Replace `storage.Open(filepath.Join(t.TempDir(), "library.db"))`, and the
cleanup registered after it, with `storagetest.Open(t)`:

- `web`: `newHistoryTestDB`, `newPagingTestDB`, `newEditTestDB`,
  `newSendTestDB`, `newLocationsTestBook`, `newImportHandlerWritable`, and
  the inline opens in `web_test.go`, `book_test.go` and `locations_test.go`.
- `service`, `scanner`, `importer`, `enrich` and `sender`: each package's
  `openTestDB`, `newMetadataTestService`, `newImportTestService`, and the
  inline opens in `service/import_test.go`.

`storage`'s own in-package tests cannot import `storagetest`, because it
imports `storage` and that would be a cycle. `books_test.go`'s `openTestDB`
therefore carries the same few lines. Say so in a comment there, so the
two copies change together.

Keep a fresh `storage.Open` where the test is about opening:

- `storage/db_test.go`'s migration, WAL and pragma tests.
- `cmd/server`, whose tests open a second handle on the file `run()`
  created.

## Step 2: the over-cap FB2 node at twice the cap

In `TestReadMetadataOverCapCoverBinaryCostsOnlyTheCappedCopy`, change
`make([]byte, 32<<20)` to `16<<20`. Its base64 is 22,369,624 bytes, exactly
twice `maxCoverBase64Bytes`.

The test guards against a regression its comment describes: copying the
token, stripping it and appending it before the cap is consulted costs
three extra copies of the node. The limit allows three caps. A node of
twice the cap puts that regression at twice the limit.

This was measured with the regression put back into `readCoverBinary`:

| Node | Excess with the regression | Limit | Correct code |
|---|---|---|---|
| 32 MiB (now) | 134 MB | 33.5 MB | — |
| 16 MiB | 67.1 MB | 33.5 MB | 22.4 MB, passes |

The figures are the same under `-race` and without it. Under `-race` the
test drops from 18.7 s to 9.1 s.

Add a comment saying why the size is twice the cap, so nobody shrinks it
past the point where the regression still fails.

## Step 3: seed rows in one transaction

Measured under `-race`: 500 `CreateBook` calls take 1.46 s, and the same
500 books through `createBookTx` in one `db.Write` take 0.48 s.

- **Inside `storage`:**
  - `TestPruneMissingFilesHandlesMoreIDsThanOneSQLChunk` seeds its
    `pruneMissingFilesChunkSize + 200` books with `createBookTx` and
    `upsertBookFileTx` in one `db.Write`, collecting the file ids there
    instead of calling `FindFileByPath` for each row.
  - `seedSearchableBooks` does the same with `createBookTx`.
  - Both follow the rule that a `Write` callback composes `…Tx` helpers and
    never calls an exported method.
- **Outside `storage`,** the `…Tx` helpers are out of reach.
  - Add `storagetest.SeedSends(t, db, bookID, title, at []time.Time)`. It
    inserts `send_log` rows in one `db.Write`, with the columns and
    `queued` status `EnqueueSend` writes, one distinct address per row.
  - Seeding the table alone is faithful here because history reads only
    `send_log`'s denormalised columns and never joins `books` or
    `recipients`. That is a CLAUDE.md storage invariant, and it is the
    reason this helper is safe.
  - `storagetest`'s test asserts that a seeded row and an `EnqueueSend` row
    read back identically through `ListSendsSince`.
  - `service`'s `enqueueSendsAt` and `web`'s
    `TestHistoryScopeLineNamesTheCapOnlyWhenTruncated` use `SeedSends`.

Measure each conversion. Where one does not at least halve its test,
leave that test as it is. These are only the four seeders above one
second, not a rule for every seeding loop.

## Documentation

- `docs/notes/testing.md` gains a short section:
  - A test's database is a copy of a template migrated once per test
    binary. Only tests about opening a database migrate from scratch.
  - `SeedSends` exists because history's reads touch `send_log` alone.
- CLAUDE.md:
  - Add `internal/storage/storagetest` to the code map.
  - Add one sentence to the testing convention: use `storagetest.Open`
    rather than `storage.Open` on a fresh path.

## Verification

1. `go vet ./...` and `go test -race ./...` pass.
2. `storagetest`'s template test passes. Also delete the `Close` before the
   template is read and confirm the test fails: that is the checkpoint it
   exists to catch.
3. Re-run the FB2 regression from Step 2 at the new size. The test must
   fail, with an excess of about 67 MB, under `-race` and without.
4. Break what each changed seeding test guards, and confirm it still fails:
   - Have `PruneMissingFiles` handle only its first chunk.
   - Flip the `truncated` computation behind `SendHistory`.
5. Compare before and after with `go test -race -count=1 -json ./...`
   summed per package, on the same machine with the same build cache.
   Expected: roughly 145 s down to about 75 s of CPU. That is about 55 s
   from the template, 9.6 s from the FB2 node, and about 8 s from seeding.
6. Compare CI's Test step against `2026092201`'s cold-cache 2m13s, still on
   a cold cache. Record both figures when this plan moves to `completed/`.
