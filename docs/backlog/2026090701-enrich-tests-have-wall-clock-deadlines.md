# Backlog: `internal/enrich`'s tests carry wall-clock deadlines

## Problem

One `go test ./...` against a freshly extracted tree failed in
`internal/enrich`, during PR #56's review. It did not reproduce: not
across three further full-suite runs, five isolated runs, a
`-count=6 -p 8` stress from the reviewer, nor three sequential runs, a
`-race -count=3 -p 8` run and six concurrent runs from a cold test cache
here. Which test failed was not captured.

**This item is one unreproduced sighting.** It is written down so a second
one has somewhere to attach, not because anything is known to be broken.

What makes it plausible rather than noise is that the package's tests are
timing-shaped in two ways a first-run compile can disturb:

- Two wall-clock deadlines set *before* the call they bound —
  `cover_test.go:104` (5 ms) and `decorator_test.go:41` (20 ms) — paired
  with handlers that sleep. This is the exact shape that made
  `internal/googlebooks`' own cancellation case flake at ~9% under
  `-race`, and it was fixed there in this same PR by cancelling from
  inside the handler instead. The two here differ in that they *want* an
  error, so an early deadline is harmless; only a late one is not.
- Several `time.After` budgets in `worker_test.go` (3 s once, 2 s
  repeatedly) waiting for a background worker to reach a state. Generous
  on an idle machine; a first `go test` compiles the whole module while
  they run.

## Why this is backlog, not a plan

Nothing is known to be wrong, and the suite is green everywhere it has
been run since. A test that failed once and cannot be made to fail again
is not evidence of a defect in the code under test — and `internal/enrich`
is untouched by the branch that surfaced it.

It also costs nothing today: CI has not failed on it.

## Re-validate before acting

- Has it happened again? If not, and the tests below have since been
  rewritten for another reason, delete this item rather than chasing it.
- If it has, capture **which test** and the surrounding output. That is
  the missing piece; without it this is a rumour with a plausible
  mechanism attached.

## Sketch

Do not raise the timeouts. A bigger number makes a slow machine pass and
tells you nothing about why it was slow.

Where a test wants to observe a cancellation, cancel deterministically
from inside the handler rather than racing a deadline against it — the
pattern `internal/googlebooks`'
`TestDetailRequestCancellationKeepsTheListAnswer` uses:

```go
ctx, cancel := context.WithCancel(context.Background())
client, _ := …(t, func(w http.ResponseWriter, r *http.Request) {
    cancel()
    <-r.Context().Done()
})
```

Where a test waits for a worker to reach a state, the budget is harder to
remove, since the thing being waited on is genuinely asynchronous. A
channel the worker closes on transition would replace polling with a
signal; that is a change to the worker's test surface, not just its
tests, so it wants weighing rather than doing.
