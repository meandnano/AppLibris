# Step: Make write transactions composable

## Context

`DB.Write` runs `fn` inside a transaction on a pool pinned to one
connection (`write.SetMaxOpenConns(1)`) — DESIGN.md's "single writer
goroutine", implemented without a goroutine. That works, but it makes every
public write method on `*DB` non-composable: each one opens its own
transaction, so **two writes cannot be made atomic with each other**, and
calling any of them from inside a `Write` callback **deadlocks**.

Verified: a `db.Write(ctx, func(tx) error { return db.UpsertBookFile(...) })`
blocks on `BeginTx` waiting for the single connection its own caller is
holding, and only returns when the context expires. `cmd/server` passes
`context.Background()` into the scanner, so in the real binary this hangs
forever, silently, with no error and no log line.

Nothing hits this today because the scanner only ever calls one write
method at a time. But three queued steps all need multi-statement atomic
writes and would each independently walk into it:

- `2026083108-scanner-orphan-books` — reassign a path *and* delete the book
  it orphaned, atomically.
- `2026083109-scanner-sweep-resilience` — create a book *and* attach its
  first location, atomically.
- `2026083110-missing-file-reconciliation` — delete a set of file rows and
  the books they orphan, atomically.

Doing this first means those three are ordinary work instead of three
chances to reintroduce a hang.

## Scope

In scope: a `tx`-taking variant of every existing write method, the public
methods re-expressed in terms of them, and a doc comment on `Write` that
states the constraint. Out of scope: replacing the single-connection pool
with an actual writer goroutine and a channel — the pool does the job
DESIGN.md asks for, and swapping it out is a bigger change with no benefit
beyond what this step delivers. Also out of scope: any new caller; this
step adds no behaviour, only the seam.

## Shape

The existing split is `CreateBook`/`UpsertBookFile`/`UpdateBookFileStat`,
each of which opens `db.Write` and does its work. Invert it: put the work
in an unexported function that takes a `*sql.Tx`, and let the exported
method be a one-line wrapper.

```go
// createBookTx inserts a book and its authors. It must be called from
// inside a DB.Write callback, using the ctx that callback is handed — see
// DB.Write's contract.
func createBookTx(ctx context.Context, tx *sql.Tx, b Book, authorNames []string) (int64, error)

func (db *DB) CreateBook(ctx context.Context, b Book, authorNames []string) (id int64, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err = createBookTx(ctx, tx, b, authorNames)
		return err
	})
	return id, err
}
```

(`fn`'s `ctx context.Context` parameter is what carries the nesting guard's
marker — see the revision note under **Guarding the deadlock** below.)

Same for `upsertBookFileTx` / `UpsertBookFile` and
`updateBookFileStatTx` / `UpdateBookFileStat`. `findOrCreateAuthor` is
already `tx`-taking and needs no change — it is the pattern the rest should
match.

Callers that need two operations in one transaction then compose the
unexported forms inside a single `db.Write`, which is what the three queued
steps want.

**Unexported, not exported.** A caller outside `internal/storage` can't
usefully hold a `*sql.Tx` without also being handed the transaction
lifetime, and exporting them would let `internal/scanner` open transactions
it doesn't own. The composition points that need to exist get exported as
purpose-named methods on `*DB` (e.g. the later
`CreateBookWithFile(ctx, book, authors, path, size, mtime)`), each one
`Write` + two `…Tx` calls. That keeps transaction boundaries entirely
inside the storage package, which is where DESIGN.md's single-writer rule
is enforced.

## Guarding the deadlock

The seam above removes the *need* to nest, but nothing stops someone
nesting anyway a year from now. Two cheap guards, in order of preference:

1. **Document it.** Extend `Write`'s doc comment: the callback runs on the
   pool's only connection, so it must not call any exported `*DB` method —
   use the `…Tx` helpers instead — and doing so deadlocks until the context
   expires rather than returning an error.

2. **Make it fail loudly instead of hanging.** Optional, and worth doing
   only if it stays small: have `Write` return a plain error
   (`ErrNestedWrite`) instead of blocking when it detects nesting.

Do both. The comment is the real fix; the flag is what makes a mistake
survivable at 2am.

> **Revised during review.** The first cut of guard 2 tracked "a write is
> in flight" as a single `sync/atomic` bool on `*DB`, reasoning that this
> was safe *because* writes are serialised — if the flag is set, the caller
> must be on the same goroutine chain that holds the connection, since no
> second writer can be in flight.
>
> That reasoning only holds if nothing ever calls `Write` from a second,
> genuinely concurrent goroutine — true of every caller *today*, but not a
> property `Write` itself enforces or should assume. A DB-wide flag can't
> tell a directly nested call apart from an unrelated concurrent one; both
> just see "a write is in flight" and both would get `ErrNestedWrite`,
> including the concurrent one that should have simply blocked on `BeginTx`
> and succeeded once the first transaction committed. That silently breaks
> `Write`'s serialisation contract for any future concurrent caller (a web
> handler alongside the scanner, say) depending on scheduling — exactly the
> kind of bug this step exists to prevent, reintroduced by the guard meant
> to catch it.
>
> Fixed by scoping the marker to the call chain instead of the `DB`: `Write`
> hands its callback a `ctx` carrying an in-progress marker
> (`context.WithValue`), and checks incoming calls for that marker rather
> than a shared flag. A nested call — one made with the `ctx` `Write` handed
> its own callback — carries the marker and is caught. A concurrent call
> from an unrelated `ctx` carries no marker, so it is unaffected: it blocks
> on `BeginTx` and succeeds once the first transaction commits, exactly as
> it did before guard 2 existed. This does mean `fn`'s signature grows a
> `ctx context.Context` parameter (`func(ctx context.Context, tx *sql.Tx)
> error`), and every `…Tx` call inside a `Write` callback must use *that*
> `ctx`, not the one closed over from the callback's own caller — the
> `internal/storage` changes and tests below reflect this shape.

## Tests

- Each exported method still behaves exactly as before — the existing
  `books_test.go` cases (`TestCreateAndFindBook`, `TestSharedAuthorIsReused`,
  `TestUpsertBookFileInsertsNewLocation`,
  `TestUpsertBookFileUpdatesOnConflict`, `TestUpdateBookFileStat`) are the
  regression suite and must pass untouched.
- New: two `…Tx` calls inside one `db.Write` commit together — assert both
  rows exist afterwards.
- New: returning an error from the callback after a successful `…Tx` call
  rolls **both** back — assert neither row exists. This is the property the
  whole step is for, and it is not currently testable.
- New (if guard 2 lands): a nested `Write` returns the sentinel error
  rather than blocking. Give the test a short `context.WithTimeout` anyway,
  so a regression fails in two seconds instead of hanging the suite until
  `go test`'s ten-minute panic.
- New, added during review: two independent goroutines both calling `Write`
  — the second must block behind the first (proved by asserting it hasn't
  returned within a short window) and then succeed once the first commits,
  never returning `ErrNestedWrite`. This is what the review-round fix to
  guard 2 above is for, and it is exactly the case a DB-wide flag gets
  wrong.

## CLAUDE.md

Amend the `internal/storage` bullet: writes are serialised through a
single-connection pool, exported methods each own one transaction, and
multi-step atomic writes compose the package-internal `…Tx` helpers inside
one `DB.Write` — callbacks must never call an exported method.

## Verification

`go build ./...`, `go vet ./...`, `go test ./...` clean. No behaviour
change is expected anywhere: this step should be invisible in the running
app, and the existing storage and scanner tests passing unmodified is the
evidence for that.
