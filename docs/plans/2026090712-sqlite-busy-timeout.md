# Step: wait for a lock instead of failing on it

## Position in the sequence

Independent of every other plan. One line in `internal/storage/db.go`
and a test.

## Context

Found in the 2026-09-07 review. The DSN sets two pragmas:

```go
dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
```

`modernc.org/sqlite` v1.57.0 applies a busy timeout only when the DSN
names one (`_busy_timeout`, `_timeout` or `_pragma=busy_timeout`); the
default is zero. So any lock contention returns `database is locked`
immediately.

Within the process that rarely matters: the write pool has one
connection and WAL lets readers proceed beside it. The contention that
does occur comes from outside the process, and on a NAS it is routine:

- A backup tool (`sqlite3 .backup`, litestream, a nightly `cp` under a
  filesystem snapshot) holding a shared lock for a few seconds.
- Someone opening `library.db` in the `sqlite3` CLI or a GUI to look at
  it, which the documentation implicitly invites.
- WAL recovery after a crash, during which the first connection to open
  the file holds an exclusive lock briefly while others fail.

Each of these makes every `DB.Write` for that window fail instantly. The
scanner logs one error per file it was processing and moves on, so a
backup that overlaps a sweep leaves a handful of files unindexed until
the next sweep. The web layer answers 500 to an edit or a send. Neither
is data loss, but both are failures that a five-second wait would have
turned into nothing.

## Scope

In scope: a busy timeout on both pools.

Out of scope: retrying at the application level. SQLite's own busy
handler is the right layer, and it already exists.

## Decision 1: `busy_timeout(5000)` in the DSN, on both pools

Five seconds is SQLite's own conventional value and covers every case
above except a long-running external reader, which should fail rather
than hang a request. Both pools take the same DSN so both get it; a
reader also needs it, since a checkpoint or recovery can lock readers
out briefly.

Set via `_pragma=busy_timeout(5000)` alongside the existing pragmas so
the DSN reads as one list, rather than the driver's `_busy_timeout`
alias.

## Decision 2: the pragma is asserted, not assumed

A test opens the database and reads `PRAGMA busy_timeout` back on a
connection from each pool. The driver's DSN syntax has changed before
and a misspelt pragma name is applied silently as nothing.

## Changes

- `internal/storage/db.go`: the DSN.
- `internal/storage/db_test.go`: the pragma round-trip on both pools, and
  a contention test: hold a write transaction open on the write pool from
  one goroutine for 500 ms, start a second `DB.Write` from another, and
  assert the second succeeds rather than returning `database is locked`.
  The single-connection write pool already queues that case inside
  `database/sql`, so this test needs a second `*DB` opened on the same
  file to reach SQLite's lock; do that.

## CLAUDE.md

`internal/storage`'s first paragraph names the two pragmas; it gains the
third and one sentence on why (external lockers on a NAS).

## Verification

- Start the server with a large library scanning. In another shell, run
  `sqlite3 data/library.db '.backup /tmp/b.db'` a few times. The scan log
  shows no `database is locked` errors.
