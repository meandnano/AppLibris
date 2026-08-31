# Backlog: bound the read connection pool

## Problem

`storage.Open` constrains the write pool but leaves the read pool at
`database/sql`'s defaults:

```go
read, err := sql.Open("sqlite", dsn)   // no SetMaxOpenConns
write, err := sql.Open("sqlite", dsn)
write.SetMaxOpenConns(1)
```

`database/sql`'s default `MaxOpenConns` is unlimited and `MaxIdleConns` is
2. So the read pool will open as many SQLite connections as there are
concurrent readers, and close all but two of them again afterwards — a
churn of open/close on the database file under any burst of concurrent
requests, and an unbounded ceiling under a pathological one.

Each `modernc.org/sqlite` connection also carries a page cache, so the
memory cost is per-connection, not shared.

## Why this is backlog, not a plan

DESIGN.md's answer to concurrency is "single user (plus wife as a send
target), internal network only". The realistic peak is one person loading a
page whose grid then fetches covers — and covers are served by
`http.FileServer` off the filesystem, not through the database. Actual
concurrent database readers number in the low single digits.

There is no observed problem here. This is a default that happens not to be
the one a deliberate design would pick, which is a different and much
weaker claim than a bug.

## Sketch

```go
read.SetMaxOpenConns(runtime.NumCPU())  // or a small constant, e.g. 4
read.SetMaxIdleConns(runtime.NumCPU())
read.SetConnMaxIdleTime(...)
```

Matching `MaxIdleConns` to `MaxOpenConns` is the part that actually matters:
it stops the churn, so a burst reuses connections instead of opening and
closing them. The ceiling itself is close to irrelevant at this scale.

Worth pairing with a `busy_timeout` pragma in the DSN if it is ever
observed to matter. In WAL mode readers do not block the writer and vice
versa, so `SQLITE_BUSY` should not arise from normal operation — but a WAL
checkpoint contending with long-lived readers is the one case that can
surface it, and the current DSN sets no timeout at all, so such an error
would return immediately rather than retrying. **This has not been observed;
do not add it speculatively** — add it with a reproduction, or not at all.

## Validate before planning

- Look for evidence first. If nothing in the logs has ever shown a
  `database is locked` / `SQLITE_BUSY` error, and no latency problem has
  been noticed, the honest conclusion may be to close this item rather than
  plan it.
- Reassess if a background job queue lands (send-to-Kindle, provider
  enrichment): those add genuine concurrent readers that are not tied to a
  browser request, which is the first thing that would make this real.
