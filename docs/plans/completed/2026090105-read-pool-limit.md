# Step: Bound the read connection pool

## Context

Promoted from `docs/backlog/2026083120-read-pool-limit.md`, deleted in this
change.

**Read this section before implementing.** That backlog file said, of
itself: *"Look for evidence first. If nothing in the logs has ever shown a
`database is locked` error, and no latency problem has been noticed, the
honest conclusion may be to close this item rather than plan it."*

The evidence was gathered, and it lands in between. There **is** a real,
reproducible inefficiency — but it costs no measurable time at this
project's scale. **This is a hygiene step, not a performance fix.** Anyone
implementing it should not expect the app to get faster, and should not
tune the numbers below chasing a speedup that isn't there.

`storage.Open` still constrains only the write pool:

```go
read, err := sql.Open("sqlite", dsn)   // no limits set
write, err := sql.Open("sqlite", dsn)
write.SetMaxOpenConns(1)
```

`database/sql` defaults to unlimited `MaxOpenConns` and `MaxIdleConns` of
**2**. So under any concurrency above two, connections are opened, used
once, and immediately closed — each one a fresh SQLite open: file open, WAL
header read, and the DSN's pragmas re-applied.

## What was measured

200 library-page renders against a 500-book database, through the real
`web.Routes` handler, counting `sql.DBStats.MaxIdleClosed` — connections
opened and then discarded rather than returned to the pool.

**At concurrency 4** (realistic for one user on a LAN, plus the scanner):

| configuration | wall clock | connections discarded |
|---|---|---|
| default (unbounded / idle 2) | 873ms | **49** |
| `SetMaxIdleConns(8)` | 852ms | 0 |
| `MaxOpen(4)` + `MaxIdle(4)` | 823ms | 0 |
| `MaxOpen(8)` + `MaxIdle(8)` | 811ms | 0 |

**At concurrency 64** (a stress case this app will never see, included to
show which knob does the work):

| configuration | wall clock | connections discarded |
|---|---|---|
| default | 741ms | **106** |
| `SetMaxIdleConns(8)` | 749ms | 63 |
| `MaxOpen(4)` + `MaxIdle(4)` | 744ms | 0 |
| `MaxOpen(8)` + `MaxIdle(8)` | 733ms | 0 |

Three conclusions, and the second and third both correct the backlog file:

1. **The churn is real and fully eliminable.** 49 discarded connections
   across 200 renders at realistic concurrency, 106 at high concurrency,
   both reduced to exactly zero.

2. **It costs nothing measurable.** 873ms → 811ms is inside the noise, and
   the concurrency-64 runs are *faster* than the concurrency-4 runs — which
   proves the measurement is dominated by the 500-book query and template
   render, not by connection handling. **Do not justify this step on speed.**

3. **The backlog file named the wrong knob as decisive.** It said matching
   `MaxIdleConns` to `MaxOpenConns` "is the part that actually matters" and
   that "the ceiling itself is close to irrelevant at this scale." At
   concurrency 4 raising idle alone is indeed sufficient — but at
   concurrency 64 it still discards 63, because with an unbounded ceiling
   64 goroutines each open a connection and only 8 can be kept. Bounding
   `MaxOpenConns` is what makes the result deterministic instead of
   dependent on how many requests happen to arrive at once.

`WaitCount` was **0** in every configuration, including `MaxOpen(4)` — so
bounding the pool did not make anything queue at these levels.

## Why do it at all

On the evidence above, closing this item would have been defensible. It is
worth the four lines anyway, for reasons that are about predictability
rather than throughput:

- The current behaviour is not a considered choice, it is two library
  defaults meeting. The write pool got a deliberate limit and a comment
  explaining it; the read pool got neither, which reads as an oversight
  rather than a decision.
- A bounded pool has a bounded memory footprint. Each `modernc.org/sqlite`
  connection carries its own page cache, so "unlimited" is an unbounded
  resource on a box that may be a Raspberry Pi or a small NAS.
- It puts a ceiling in place **before** the first background job queue
  (send-to-Kindle, provider enrichment) adds readers that are not tied to a
  browser request — the exact condition the backlog file said to reassess
  on. Adding the ceiling now costs four lines; adding it later, in response
  to a symptom, costs a diagnosis first.

## Scope

In scope: the pool limits and a comment recording why. Out of scope:
`busy_timeout`, and anything else speculative — see below.

No schema change, no migration, no new dependency, no behaviour change any
user can observe.

## The change

In `storage.Open`, alongside the existing write-pool limit:

```go
// The write pool is pinned to one connection to serialise writes. The read
// pool is bounded for a different reason: database/sql defaults to an
// unlimited ceiling with only 2 idle connections, so any concurrency above
// two opens connections, uses them once and discards them — measured at 49
// discarded across 200 page renders at concurrency 4, and each discard is a
// fresh SQLite open plus pragma application. Matching max and idle keeps
// connections in the pool instead. This is about predictability and a
// bounded page-cache footprint, not speed: the wall-clock difference is
// inside the noise at this scale.
const readPoolSize = 8
read.SetMaxOpenConns(readPoolSize)
read.SetMaxIdleConns(readPoolSize)
```

**Pick 8, a constant, not `runtime.NumCPU()`.** The backlog file suggested
`NumCPU` as an option. Don't: it makes the pool size vary between the
developer's laptop and the target box for no reason connected to what the
pool is doing, and SQLite reads are I/O-bound against the page cache rather
than CPU-bound. A named constant with the comment above is easier to reason
about, and 8 is comfortably above anything a single-user LAN app plus a
background sweep will ask for.

**Do not set it to 1 or 2.** The scanner reads through this same pool —
`reconcileMissing` calls `ListFilesUnder`, which loads every `book_files`
row — so a too-small ceiling would let a sweep's read block page renders.
WAL mode means readers never block each other, but the *pool* would. 8
leaves ample headroom; `WaitCount` was 0 even at 4.

## Explicitly not doing

**`busy_timeout`.** The backlog file raised it and then said "this has not
been observed; do not add it speculatively — add it with a reproduction, or
not at all." That still holds, and this step does not change it. In WAL mode
readers and the single writer do not block each other, so `SQLITE_BUSY`
should not arise in normal operation. If it ever does, it gets its own plan
with the reproduction attached.

## Tests

There is genuinely little to assert here, and a test that merely restates
the constant is worse than none.

- One test that `storage.Open` returns a DB whose read pool reports
  `Stats().MaxOpenConnections == readPoolSize`. It pins that the limit is
  applied at all — the failure this guards against is someone adding a
  second `sql.Open` path that forgets it, which is exactly the mistake the
  read pool already made once.
- Do **not** add a test asserting connections are reused, or a timing test.
  Both would be measuring `database/sql`'s behaviour rather than this
  package's, and the timing one would be flaky for a difference that is
  inside the noise anyway.
- The existing `TestOpen` and `TestOpenIsIdempotent` must keep passing.

## CLAUDE.md

Extend the `internal/storage` bullet: the read pool is bounded and its idle
count matched to it, so connections are reused rather than opened and
discarded per request; the write pool stays pinned to one connection to
serialise writes.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- No manual verification step, deliberately. There is nothing a user can
  observe, and the measurement that justifies the change is recorded above
  rather than being something to re-run — reproducing it means writing a
  throwaway concurrency harness, which is not worth carrying in the repo.
