# Step: a send that timed out has an unknown outcome, not a failed one

## Position in the sequence

Independent of the other send plans. `internal/sender` and the strings
the UI shows.

## Context

Found in the 2026-09-07 review. `process` in `internal/sender/sender.go`
runs the transport under a per-job deadline and then decides what the
error means by looking at the wrong context:

```go
sendCtx, cancel := context.WithTimeout(ctx, resend.SendTimeout)
messageID, err := w.transport.Send(sendCtx, ...)
cancel()
if err != nil {
	if ctx.Err() != nil {
		return
	}
	w.fail(ctx, send.ID, truncate(err.Error(), maxFailureReason))
	return
}
```

When `sendCtx` alone expires, `ctx.Err()` is nil, so the row is marked
`failed` with the reason `send request: Post "https://api.resend.com/
emails": context deadline exceeded`. But the whole design of
`FailInterruptedSends` rests on the observation that an abandoned request
is **unknowable**: the body may have been fully uploaded and accepted
with only the response outstanding. `resend.SendTimeout`'s own comment
computes that a 28 MB attachment needs about four of the five minutes on
a 1.5 Mbit/s uplink. A NAS on a slower line lands past five.

The person sees "Failed", presses Retry (a new row, by design), and the
Kindle receives the book twice. The rule CLAUDE.md states for the sender
is that terminal writes follow "whether the outcome is known"; this path
records a definite verdict for an unknown one.

## Scope

In scope: classifying a per-job deadline as an unknown outcome and
recording it honestly; making the deadline scale with the attachment.

Out of scope, with reasons:

- **Leaving the row `sending` for startup recovery**, as the
  parent-cancel case does. That case is a shutdown, where recovery runs
  soon. A timeout is not a shutdown; the process keeps running and the
  row would sit `sending` until the next restart, with the UI polling it
  forever. It needs a terminal state now.
- **Querying Resend for the message's fate.** Resend's API can retrieve
  an email by id, but the id is in the response that never arrived. The
  idempotency-key feature Resend offers would let a retry be safe, and
  is the right long-term answer; it changes the transport contract and
  deserves its own plan once this stopgap is in.

## Decision 1: a deadline is recorded `failed` with a reason that says the outcome is unknown

`failed` is the only terminal state that lets Retry appear, and the person
does need a way to retry. What changes is the sentence: `timedOutReason =
"timed out before Resend answered — check the Kindle before sending
again"`. It replaces the raw error text, which today carries the request
URL and the words `context deadline exceeded`, neither of which a person
can act on.

Detection is `errors.Is(err, context.DeadlineExceeded)` on the transport
error, checked before the generic `fail`. The parent-cancel branch stays
first and unchanged.

## Decision 2: the per-job deadline scales with the attachment

`resend.SendTimeout` stays as the floor. The worker computes its per-job
deadline as `max(SendTimeout, size / minUplinkBytesPerSecond +
SendTimeout/2)` where `minUplinkBytesPerSecond` is a named constant
sized for a slow domestic uplink (1 Mbit/s, so 125 KB/s, giving a 28 MB
attachment about five and a half minutes plus the floor's slack). The
`http.Client.Timeout` inside `resend.NewClient` must be raised to match
or removed in favour of the context, since the smaller of the two wins.

The constant carries the arithmetic in its comment, the way `SendTimeout`
already does, so it is retuned on measurement rather than instinct.

## Changes

- `internal/sender/sender.go`: `timedOutReason`; deadline computed from
  `info.Size()`; `errors.Is(err, context.DeadlineExceeded)` branch.
- `internal/resend/resend.go`: `NewClient`'s `http.Client.Timeout`
  replaced by relying on the caller's context, with `SendTimeout` kept as
  the documented floor and used by the worker. A caller passing
  `context.Background()` loses its backstop; the doc comment says so and
  names the worker as the only caller.

## Tests

`internal/sender`:

- A transport that blocks until its context expires, with a short
  `SendTimeout` injected: the row is `failed` with `timedOutReason`, not
  the raw error.
- The same transport with the parent context cancelled: the row stays
  `sending` (existing behaviour, re-pinned).
- A transport returning an ordinary error: the raw text is recorded as
  before.
- The deadline computation as a table over sizes.

## CLAUDE.md

The `internal/sender` paragraph's "the line those two rules are drawn
along is whether the outcome is known" sentence gains the timeout case
and its reason string, and notes the size-scaled deadline. The
`internal/resend` paragraph's `SendTimeout` sentence is updated to say
the worker owns the deadline.

## Verification

- Point `RESEND_API_KEY` at a local `httptest`-style stub that sleeps past
  the deadline. Send a book: the control shows Failed with the new
  sentence, Retry is offered, the log carries the send id.
