# Testing

## Time

A test that waits on time runs inside `synctest.Test`, on a fake clock
that moves only when every goroutine in the bubble is durably blocked. A
wait for state is then `synctest.Wait()` and one check rather than a poll
against a deadline, and a duration is asserted exactly: a retry schedule,
a rate-limit interval, a debounce window. A slow machine cannot fail such
a test and a fast one cannot pass it by luck.

Four rules come with the bubble.

- **Everything the test runs is built inside it, with the bubble's `t`.**
  `database/sql` starts a connection opener per pool, which `db.Close`
  stops, and `t.Cleanup` runs inside the bubble, so `openTestDB` there is
  correct. A handle opened outside and used inside is not: the pool
  answers a request channel the bubble made from a goroutine the bubble
  does not own, which is a fatal error.
- **Nothing a bubble runs may own a goroutine that outlives its caller.**
  The bubble does not end while one remains. This is why `WithRateLimit`
  keeps a next-slot time rather than a ticker: nothing that builds a
  limiter ever stops one.
- **Cancel straight after the signal a test waits for.** While the test
  goroutine is blocked, the clock jumps to the next deadline in the code
  under test. A sender test that blocked between `entered` and `cancel()`
  would find the send failed at its own five-minute deadline rather than
  left `sending`.
- **A hang shows up in one of two ways.** A deadlock among the bubble's
  channels fails the test at once, so a `time.After` limit around a plain
  receive adds nothing. A goroutine blocked on a mutex or a syscall is not
  durably blocked, so it stops the clock, and the test runs until
  `go test`'s own timeout.

SQL-side `strftime('now')` reads the real clock while Go's `time.Now()`
in a bubble starts at 2000-01-01, so a bubbled test does not compare the
two.

## HTTP

A test server is `httptest.NewTestServer`, which serves over an in-memory
network, fails the test on a handler panic and closes itself. Its
`Client()` sends every request to that one server whatever host or scheme
the URL names, https included. That makes it easy to wire wrongly, so:

- **`Client()` comes before anything reads `URL`.** `URL` is empty until
  `Client()` starts the fake network, and the order of evaluation inside
  one struct literal is unspecified. Tests use fixed URLs instead.
- **Fixed URLs name hosts under `.test`**, which never resolves, so a
  client that missed the in-memory transport fails at DNS instead of
  reaching a real API or `example.com`.
- **One server per client, dispatching on `r.Host`.** A second server is
  unreachable from the first one's client, so a test about which host was
  asked — a redirect off the API host, a cover host that must not be
  fetched — keeps a handler per host and counts what each one saw.
- **A production client takes the server's transport, not its client.**
  `openlibrary.New`, `googlebooks.New` and `resend.NewClient` are built
  as production builds them and handed `server.Client().Transport`, so
  the real `Timeout` and redirect policy are what the tests exercise. It
  is that `Transport` object, not a clone: the server closes only its own
  transport's idle connections, and a clone's would outlive a bubble.

A test that is about the socket itself calls `Start` or `StartTLS` on a
`NewTestServer` and listens on loopback. The two `Worker` tests about the
cover address guard do, because the guard hangs on the dial the worker's
own transport makes; so do the import refusal tests, which pin that an
answer survives a real socket while the body is still arriving. The
`Worker`'s other cover tests reach the same loopback server through
`coverServer` and opt the guard out — they wait on no clock, so one
server for all of them is cheaper than a second one on the in-memory
network.

## The database

A test's database is `storagetest.Open(t)`: a copy of a template the test
binary migrated once, opened from `t.TempDir()`. Running every migration
again per test costs twenty times what copying the file does, and a suite
of around 450 such tests spends most of its CPU there rather than on
anything it asserts. `storage.Open` still runs `migrate` over the copy,
which finds every migration applied.

The template is built from the migrations the binary was compiled with, so
it cannot go stale the way a committed fixture would, and it is held as
bytes rather than a path, so nothing needs a `TestMain` to clean it up.
`storagetest`'s own test is what pins it: it reads the template with a bare
driver handle, which runs no migrations, and compares its
`schema_migrations` and `sqlite_master` with a database migrated the long
way. `Close` is what checkpoints the WAL into the main file, and only a
test that looks at the template directly can see a template read before
that checkpoint — through `storage.Open` an empty one is silently migrated
and looks identical.

Only tests about opening a database migrate from scratch: `storage`'s own
`db_test.go`, and `cmd/server`, whose tests open a second handle on the
file `run()` created. `storage`'s in-package tests cannot use
`storagetest` at all, since it imports `storage`, so `books_test.go`
carries its own copy of the same few lines.

`storagetest.SeedSends` writes `send_log` rows directly, in one
transaction, for the two tests that need the history cap's worth of them.
Seeding that table alone is faithful because history reads send_log's
denormalised `book_title` and `recipient_address` and never joins `books`
or `recipients`; the helper's test pins that a seeded row and an
`EnqueueSend` row read back identically.

## What runs on real time

- **The watcher tests about the kernel** — removal delivery, every
  `Refresh` case, recovery after the library directory is replaced, the
  kernel watch count, and `NewWatcher`'s own refusal of a missing
  directory. A real fsnotify reader sits in a syscall, which is never
  durably blocked, so a bubble holding one never goes idle. The debounce,
  the delivery probe and the event filter are the watcher's own logic, and
  run on a fake `watchSet` and event channels instead.
- **`TestWriteWaitsForAnExternalLockInsteadOfFailing`.** SQLite's busy
  handler sleeps through `nanosleep` on Linux and through Go's
  `time.Sleep` on darwin, so in a bubble it would pass on a Mac and time
  out in CI.
- **`TestScanStopsOnCancellationMidSweep`.** Its poller races `Scan`'s
  file and database I/O, none of which blocks durably, so in a bubble the
  poller could only wake once the sweep had finished.
- **`cmd/server`.** `run()` listens on real TCP, the tests reach it
  through `http.DefaultTransport`, and it starts a real watcher. Its
  `time.After` limits fire only on a failure.
