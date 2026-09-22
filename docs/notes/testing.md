# Testing

The test rules every package shares, and `internal/storage/storagetest`,
the database a test opens.

## Time

- **A test that waits on time runs inside `synctest.Test`, and a wait for
  state is `synctest.Wait()` and one check, never a poll against a
  deadline.** The fake clock moves only when every goroutine in the bubble
  is durably blocked, so a duration is asserted exactly: a slow machine
  cannot fail the test and a fast one cannot pass it by luck.
- **Everything the test runs is built inside the bubble, with the bubble's
  `t`.** `database/sql` runs a connection opener per pool, so a handle opened
  outside answers the bubble's queries from a goroutine the bubble does
  not own, which is a fatal error. `t.Cleanup`
  runs inside the bubble, so `storagetest.Open` there is correct.
- **Nothing a bubble runs may own a goroutine that outlives its caller.**
  The bubble does not end while one remains. This is why `WithRateLimit`
  keeps a next-slot time rather than a ticker: nothing that builds a
  limiter ever stops one.
- **Cancel straight after the signal a test waits for.** While the test
  goroutine is blocked the clock jumps to the next deadline in the code
  under test, so a sender test that blocked between `entered` and
  `cancel()` would find the send failed at its own deadline rather than
  left `sending`.
- **No `time.After` limit around a plain receive in a bubble.** A deadlock
  among the bubble's channels fails the test at once. A goroutine blocked on
  a mutex or a syscall is not durably blocked, so it stops the clock and the
  test runs to `go test`'s own timeout, which no limit in the test shortens.
- **A bubbled test never compares SQL `strftime('now')` with Go's
  `time.Now()`.** SQLite reads the real clock; the bubble's starts at
  2000-01-01.

## HTTP

- **A test server is `httptest.NewTestServer` on its in-memory network.**
  Its `Client()` sends every request to that one server whatever host or
  scheme the URL names, https included, which the rules below depend on.
- **`Client()` is called before anything reads `URL`, and tests use fixed
  URLs.** `URL` is empty until `Client()` starts the fake network, and the
  order of evaluation inside one struct literal is unspecified.
- **Fixed URLs name hosts under `.test`.** That suffix never resolves, so a
  client that missed the in-memory transport fails at DNS instead of
  reaching a real API.
- **One server per client, dispatching on `r.Host`.** A second server is
  unreachable from the first one's client, so a test about which host was
  asked keeps a handler per host and counts what each one saw.
- **A production client is built as production builds it and handed
  `server.Client().Transport` itself, never a clone.** `openlibrary.New`,
  `googlebooks.New` and `resend.NewClient` then exercise the real `Timeout`
  and redirect policy. The server closes only its own transport's idle
  connections, so a clone's would outlive a bubble.
- **`Start` and `StartTLS` are only for a test about the socket itself.** The two
  `Worker` tests about the cover address guard need one because the guard
  hangs on the dial the worker's own transport makes; the import refusal
  tests need one because they pin that an answer survives a real socket
  while the body is still arriving.
- **The `Worker`'s other cover tests share that one loopback server through
  `coverServer`, with the guard opted out.** They wait on no clock, so one
  server for all of them is cheaper than a second on the in-memory network.

## The database

- **A test's database is `storagetest.Open(t)`, never `storage.Open` on a
  fresh path.** It copies a template the test binary migrated once into
  `t.TempDir()`; running every migration for each of some four hundred
  tests costs twenty times as much under `-race`. `storage.Open` still runs
  `migrate` over the copy and finds every migration applied.
- **The template is built from the compiled-in migrations and held as bytes,
  not a path.** Built from the migrations, it cannot go stale the way a
  committed fixture would. Held as bytes, it needs no `TestMain` to clean
  up, and it is legal inside a bubble because the build opens and closes
  within the one call that first needs it, leaving no connection opener
  behind.
- **A test pins the template by reading its bytes with a bare driver handle
  and comparing `schema_migrations` and `sqlite_master` against a database
  migrated the long way.** `Close` is what checkpoints the WAL into the main
  file. A template read before that is an empty database, and `storage.Open`
  would silently migrate it into one that looks identical.
- **Three kinds of test migrate from scratch instead; every other package
  above `storage` uses `storagetest`.** Tests about opening a database, in
  `storage`'s `db_test.go`. `cmd/server`, which stays on `storage.Open`
  throughout because two of its opens are second handles on the file
  `run()` created, which no template can supply. And `storage`'s own tests,
  which cannot import `storagetest` because it imports them: the cycle
  rules out the import, not the technique, so `books_test.go` carries the
  same template and the two change together.
- **`storagetest.SeedSends` writes `send_log` rows directly, in one
  transaction.** Seeding that table alone is faithful because history reads
  its denormalised `book_title` and `recipient_address` and never joins
  `books` or `recipients`.
- **Its tests pin a seeded row against an `EnqueueSend` one.** Identical
  columns, byte-identical `queued_at` text for the same instant, and the
  order `ListSendsSince` gives an enqueued row between two seeded ones,
  which is what the fixed-width timestamp exists for and what a comparison
  of parsed times cannot see.

## What runs on real time

- **The watcher tests about the kernel.** Removal delivery, every `Refresh`
  case, recovery after the library directory is replaced, the kernel watch
  count, and `NewWatcher`'s refusal of a missing directory. A real fsnotify
  reader sits in a syscall, which is never durably blocked, so a bubble
  holding one never goes idle. The debounce, the delivery probe and the
  event filter are the watcher's own logic and run on a fake `watchSet`.
- **`TestWriteWaitsForAnExternalLockInsteadOfFailing`.** SQLite's busy
  handler sleeps through `nanosleep` on Linux and through Go's `time.Sleep`
  on darwin, so in a bubble it would pass on a Mac and time out in CI.
- **`TestScanStopsOnCancellationMidSweep`.** Its poller races `Scan`'s file
  and database I/O, none of which blocks durably, so in a bubble the poller
  could only wake once the sweep had finished.
- **`cmd/server`.** `run()` listens on real TCP, the tests reach it through
  `http.DefaultTransport`, and it starts a real watcher. Its `time.After`
  limits fire only on a failure.
