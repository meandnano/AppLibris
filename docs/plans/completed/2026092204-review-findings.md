# Fix what the review found on `2026092202`

## Context

`2026092202` moved every test above `internal/storage` onto a template
database, halved the over-cap FB2 node and put three seeding loops in one
transaction: 177 s down to 107 s of `-race` CPU.

A review over that change found no critical or major defect and eight minor
ones. Three of them matter, and they are the reason this plan exists rather
than a backlog note: two are comments and notes claiming guarantees the code
does not have, which is exactly what CLAUDE.md's documentation rule is
against, and one is the largest remaining saving left behind by a comment
that misstates why.

`2026092202` is in `completed/` and immutable, so its corrections live here.

## Step 1: `internal/storage` gets the same template

`internal/storage/books_test.go`'s `openTestDB` migrates from scratch, and
124 in-package tests call it. The comment says `storagetest` is out of reach
here, which is true of the import and not of the technique — and reads as
though no template were possible.

- Add `templateDB`, a `sync.OnceValues` builder beside `openTestDB`: an
  `os.MkdirTemp` directory, `Open`, **`Close`** (the WAL checkpoint),
  `os.ReadFile`, `defer os.RemoveAll`. The same shape as `storagetest`'s.
- `openTestDB` writes those bytes into `t.TempDir()` and opens the copy.
- Say in the comment what the cycle actually blocks, and keep the
  instruction that the two copies change together.

`db_test.go`'s three `openTestDB` callers come along, correctly: none is
about migrating. Every `Open` in `db_test.go` that *is* about opening stays,
and `migrations_test.go` never goes through `storage.Open` at all.

A missing `Close` here is silent — every test still passes, only slower — so
`db_test.go` gains the guard `storagetest` already has:
`TestTemplateCarriesTheMigratedSchema` reads the template bytes with a bare
`sql.Open`, which runs no migrations, and compares `schema_migrations` and
`sqlite_master` with a database migrated the long way.

## Step 2: make the `sqliteTimeLayout` comment true

The comment on `storagetest`'s copy of `sqliteTimeLayout` says the package's
test pins the timestamp shape. It does not:
`TestSeedSendsReadsBackLikeAnEnqueuedRow` compares `describeSend` of two
rows, i.e. values the driver has already parsed, so a layout that parses to
the same instant leaves it green while text ordering on `queued_at` breaks.
Strengthen the test rather than weaken the comment.

- `TestSeedSendsReadsBackLikeAnEnqueuedRow` seeds two rows a second apart
  with an `EnqueueSend` row timestamped between them, and asserts the order
  `ListSendsSince` returns the three in. That is what a fixed-width layout
  is for, and it pins the distinct-address promise on the way past.
- `TestSeedSendsWritesTheRowEnqueueSendWrites` writes one row by each route
  for the same instant and asserts byte-identical `queued_at` **text**, read
  as `TEXT` off `send_log`, plus the column-by-column comparison.

## Step 3: the small fixes

- `storagetest.go` — collapse the `sync.Once` triple into `sync.OnceValues`
  (`internal/web/assets.go` already uses `sync.OnceValue`), and write
  `string(storage.SendQueued)` where `SeedSends` had `"queued"`, since the
  comment above promises the status `EnqueueSend` writes.
- `books_test.go`'s prune seeder — one clause noting the seeded books carry
  no `books_fts` row, that being the one thing `CreateBookWithFile` does
  which `createBookTx` + `upsertBookFileTx` skips.
- `fb2_test.go` — drop "the smallest node": 12 MiB would also clear the
  bound. The argument is the 2× margin, which the rest of the comment makes.
- `web/import_test.go` — `var err error` and a plain assignment, rather than
  `s, err :=` and a copy into `stager`.
- `storagetest_test.go` — an explicit `_ "modernc.org/sqlite"`, since the
  bare `sql.Open` needs the driver in its own right.

## Step 4: the documentation

`docs/notes/testing.md`'s database section:

- Detach the test count from the template claim and qualify the twenty-fold
  figure as the `-race` measurement it is.
- Replace the exception sentence, which is wrong about `cmd/server`: its
  stated reason covers two of that package's six opens. Three exceptions,
  each with its own reason — a test about opening a database; `cmd/server`,
  which stays on `storage.Open` throughout; and `storage`'s own tests, which
  cannot import `storagetest` and carry the template themselves.
- State the property that makes `storagetest.Open` legal inside a bubble —
  the build opens *and* closes within the one call — since that is what a
  move to a `TestMain` would break, and it lives only in the source today.
- Say what Step 2's tests pin.

The first bubble rule names `openTestDB`, which after Step 1 means only
`storage`'s in-package helper: name `storagetest.Open`.

CLAUDE.md: the code map's `storagetest` line says "every package above this
one", which `cmd/server` is and does not; and the testing convention states
the rule with one wrong exception where there are three.

## Verification

1. `go vet ./...`, `go test -race ./...` and `go test -race -shuffle=on
   ./...` pass.
2. Delete the `Close` in Step 1's builder: `TestTemplateCarriesTheMigratedSchema`
   must fail with `no such table: schema_migrations`.
3. Change `storagetest`'s `sqliteTimeLayout` to `"2006-01-02T15:04:05Z07:00"` —
   precision only. Step 2's test must fail; it passes today, which is the
   finding.
4. Re-break the three guards `2026092202` verified, to confirm Step 1 did not
   weaken them: first-chunk-only `PruneMissingFiles`, a broken `truncated`
   behind `SendHistory`, and the FB2 copy-strip-append regression.
5. Measure `internal/storage` and the whole suite before and after.

   **Measured**, same laptop as `2026092202`'s table: `internal/storage`
   27.67 s → 14.93 s (0.54×), and the whole suite 107.42 s → 94.26 s. Against
   that plan's 176.99 s baseline the two together are a 47% cut, which is
   where its 48% estimate expected to land.

   Everything else in 1–4 holds as written. The one thing worth recording:
   Step 1's guard had to read the template bytes with a bare `sql.Open` for
   the same reason `storagetest`'s does, and deleting the `Close` proves it —
   without the bare handle the test passes either way, since `Open` migrates
   whatever it is given.
