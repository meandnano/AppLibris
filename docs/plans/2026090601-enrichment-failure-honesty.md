# Step: tell "nothing found" apart from "nobody answered"

## Position in the sequence

**First of the enrichment trio (this, 02, 03), and a prerequisite for
both.** Step 02 changes which provider path is taken and adds a reason an
answer can be rejected; step 03 changes what a failed cover leaves behind.
Both are easier to reason about once a job's terminal row can express
*why* a run came back empty, and step 02 in particular makes the empty
case more common.

It is also the only one of the three that is wrong on screen today, which
is why it goes first regardless.

## Context

This is gap 1 of `docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`,
re-validated against the code as it stands after step 06 (the enrichment
UI). It was written while enrichment had no trigger; step 06 gave it one,
which moved this from "a distinction the table cannot express" to "a
sentence the UI says that is not true".

`internal/enrich.Resolve` logs and skips a provider that errors
(`slog.Warn`, then `continue`) and always returns a nil error — deliberate,
per DESIGN.md's provider-chain section: a provider erroring "is logged and
skipped, and the chain continues — it is not the resolver's own failure to
report." The worker then ends:

```go
var written []storage.MetadataField
if len(values) > 0 {
	// ... ApplyEnrichedFields
}
w.done(ctx, job.ID, written)
```

So a job in which *every* provider errored — a 429, a 5xx, a timeout, a
Google Books daily quota this project has already been observed to
exhaust — reaches `w.done` with an empty `values` map. The row is
`status='done'`, `updated_fields=''`, `failure_reason=''`: byte-for-byte
identical to a run where both providers were reached, answered cleanly,
and genuinely had nothing to say.

`internal/web`'s `enrichmentResultLine` renders that as **"Nothing to
add"**, with `EnrichResultOK = true` — the success treatment. DESIGN.md
is explicit that "Nothing to add" is a success and that rendering it as a
failure "would train people to distrust a feature working exactly as
intended". That reasoning is right, and it is exactly what makes this
defect worth fixing rather than tolerating: the sentence is load-bearing.
A person who presses Fetch on a day Open Library is throttling is told,
in the success treatment, that their book has nothing left to find — and
the control offers no reason to press it again.

Nothing is corrupted. The cost is a wrong answer to the one question the
control exists to answer.

## Scope

In scope: making `Resolve` report how many providers it asked and how many
of those failed, and making the worker record a run where every asked
provider failed as something other than a bare `done`.

Out of scope, with reasons:

- **The per-book attempt ceiling.** Gaps 2 and 3 of the backlog item.
  The ceiling is only load-bearing once something other than a person
  triggers enrichment, which DESIGN.md's deferred list rules out; and a
  ceiling built on today's job rows would count "the API was down" as
  evidence about the book, which is the mistake this step exists to make
  impossible. The backlog item stays, rewritten to cover only what is
  left of it.
- **The ISBN-to-search fallback.** Gap 3, moved into step 02, where it
  belongs beside the match-confidence guard that makes it safe.
- **Distinguishing retryable from permanent provider failures.** Both
  provider clients already classify their errors against
  `enrich.ErrRetryable` for `WithRetry`'s benefit, so the information is
  there. But the person's next action is the same either way — press the
  button again — so surfacing the difference buys a sentence nobody acts
  on differently. Revisit if a ceiling ever needs it.
- **Naming *which* provider failed in the UI.** The log line already
  names it (`slog.Warn("enrichment provider failed", "provider", …)`).
  Putting a provider's name in front of a person implies they can do
  something about it.

## Decision 1: `Resolve` returns a struct, not two more values

`Resolve`'s signature is already five returns
(`values, sourceName, coverURL, coverSource, err`). Adding an asked count
and a failed count makes seven, at which point the call site stops being
readable and every future addition is worse.

Replace them with one result type in `internal/enrich`:

```go
// Resolution is everything one Resolve call learned. Values/SourceName/
// CoverURL/CoverSource are what it found; Asked/Failed are what it cost,
// which is what lets the caller tell an empty result that means "this
// book is not in either catalogue" from one that means "neither
// catalogue answered".
type Resolution struct {
	Values      map[storage.MetadataField]string
	SourceName  map[storage.MetadataField]string
	CoverURL    string
	CoverSource string

	// Asked counts providers actually called — not providers configured.
	// A chain that stops early because nothing is left missing did not
	// ask the rest, and counting them would report a run as broader than
	// it was.
	Asked int
	// Failed counts, of those Asked, how many returned an error.
	Failed int
}

func Resolve(...) (Resolution, error)
```

`Asked` counting only providers actually called is the detail to get
right: DESIGN.md's early-stop ("the chain stops early and saves the API
calls") means a two-provider chain routinely asks one, and a rule keyed on
`Asked == Failed` would misread `0 == 0` if `Asked` counted configuration
instead. The existing test that asserts an un-called provider is never
called gains a companion assertion on `Asked`.

## Decision 2: a total provider failure is a `failed` job, not a fourth state

The backlog offered two shapes: a `failed` row with a reason, or a fourth
terminal state if `failed`'s "the job itself went wrong" meaning should
stay narrow.

**`failed`, for three reasons.**

First, it *is* the job going wrong. The job's entire purpose is to ask
providers. A run that reached none of them did not do the thing it
promised, in exactly the way a run whose write failed did not.

Second, the retry affordance already exists and is already correct for
this. `internal/web`'s enrichment control renders a failed job with its
`failure_reason` and a **Retry** button, and a re-run is a new row — the
same rule sending follows. A fourth state would need its own CHECK
constraint migration, its own `EnrichmentStatus` constant, its own service
shaping, its own template branch, and its own copy — all to reach a screen
that offers the person the same single button.

Third, the contrast the schema is built on stays intact. `internal/sender`
never calls a send "delivered" because there was nothing left to send;
this is the same rule pointed the other way — a run that learned nothing
must not be recorded as a run that found nothing.

The reason text, beside the three the worker already has:

```go
// allProvidersFailedReason is recorded when every provider the run asked
// returned an error — a throttle, a 5xx, a timeout. It is deliberately
// not a done job with an empty result: that row is indistinguishable
// from an honest "this book is in neither catalogue", and the control
// renders it in the success treatment, telling a person there is nothing
// left to find on a day nobody was reachable.
const allProvidersFailedReason = "no metadata provider could be reached — try again"
```

## Decision 3: the rule is *every* asked provider failed, not *any*

```go
if res.Asked > 0 && res.Failed == res.Asked && len(res.Values) == 0 {
	w.fail(ctx, job.ID, allProvidersFailedReason)
	return
}
```

Three conditions, each earning its place:

- `Asked > 0` keeps the two legitimate zero-provider cases as successes:
  a book with nothing missing (`Resolve` returns before the loop) and a
  deployment running `METADATA_PROVIDERS=`, which DESIGN.md documents as
  the way to disable enrichment outright. Both are honest "nothing to
  add" runs.
- `Failed == Asked` rather than `Failed > 0`: if one provider was
  throttled and the other answered cleanly and had nothing, the run *did*
  learn something about the book, and failing it would hide a real answer
  behind a flaky neighbour and invite a retry that cannot improve on it.
  The narrower rule is the one that never lies; the wider one trades one
  false success for a stream of false failures.
- `len(res.Values) == 0` is belt-and-braces and should be unreachable
  given the clause above — a provider that failed contributed no values.
  It is written anyway because the day it stops being unreachable is the
  day a partial result would otherwise be thrown away.

**Ordering matters and is easy to get backwards.** The worker's existing
`ctx.Err() != nil` check after `Resolve` must stay *ahead* of this
classification. `Resolve` cannot tell a provider call abandoned by
shutdown from one that failed outright — its doc comment says so — so
during shutdown every provider "fails", and classifying first would write
a permanent `failed` row for a job that `RequeueInterruptedEnrichment`
should have retried. The cancellation check returns and leaves the row
`running`; only a live-context run reaches the new branch.

## Changes

- `internal/enrich/resolver.go`: introduce `Resolution`; change `Resolve`
  to return it; increment `Asked` before each provider call and `Failed`
  in the existing `perr != nil` branch.
- `internal/enrich/worker.go`: adapt the call site to the struct; add
  `allProvidersFailedReason`; add the Decision 3 branch immediately after
  the existing post-`Resolve` `ctx.Err()` check and before the cover fetch.
- No schema change, no migration, no service change, no template change.
  The failed state, its reason line and its Retry button all already
  render.

## Tests

`internal/enrich/resolver_test.go`, against the existing fakes:

- All providers answer: `Asked == len(providers or fewer)`, `Failed == 0`.
- All providers error: `Asked == 2`, `Failed == 2`, empty values, nil error
  (the last is the existing contract and must not change).
- Mixed: first errors, second answers — `Asked == 2`, `Failed == 1`, values
  from the second.
- Early stop: first answers everything — the second is never called *and*
  `Asked == 1`. Extend the existing never-called test rather than adding a
  second one.
- Nothing missing: `Asked == 0`, `Failed == 0`, no provider called.

`internal/enrich/worker_test.go`:

- Every provider errors → job is `failed` with `allProvidersFailedReason`,
  and `updated_fields` is empty.
- One provider errors, one answers cleanly with nothing → job is `done`
  with an empty `updated_fields`. This is the test that pins Decision 3;
  without it the `any`-versus-`every` rule silently drifts.
- Zero providers configured → `done`, not `failed`.
- Cancellation still leaves the row `running` — the existing test, which
  must keep passing with the new branch in place. If it passes only
  because the fake provider returns before noticing cancellation, tighten
  it so a cancelled context reaches the classification branch and is still
  turned away by the `ctx.Err()` check above it.

`internal/web/enrich_test.go`: the existing test asserting "Nothing to add"
is a *success* stays exactly as it is. It is now guarding a narrower claim
than it was, which is the point.

## CLAUDE.md

`internal/enrich`'s paragraph currently says "a job that reached at least
one provider still finishes `done`" — true, and now the precise statement
of the rule rather than an aside. Extend it to say what happens when it
reached none, and note that `Resolve` reports `Asked`/`Failed` for exactly
that decision. The `internal/storage` paragraph on `enrichment_jobs` needs
one clause: `failed` now also covers "every provider the run asked was
unreachable", alongside the book vanishing and a write failing.

## DESIGN.md (on `init`)

Two places. The provider-chain section's four-case table is unchanged — the
classification it describes is what makes this possible — but the sentence
"A provider erroring is logged and skipped, and the chain continues — it is
not the resolver's own failure to report" now needs its other half: the
resolver still does not fail, and the *worker* is what decides that a run
in which nobody answered is not a success.

The deferred list's closing paragraph says "Today a run in which every
provider failed is recorded exactly like an honest no-match, which is fine
while a person is the trigger and wrong the moment anything else is." That
is no longer true and should say so — with the ceiling itself still listed
as the thing that must arrive with automatic enrichment.

## Verification

- Point a dev instance at `METADATA_PROVIDERS=openlibrary` with the host
  unroutable (`/etc/hosts` entry, or an unreachable proxy). Press Fetch on
  a book with an empty publisher. The control must show the failure
  treatment with the new reason and a Retry button — not "Nothing to add".
- Restore the route, press Retry on the same book, confirm it resolves
  normally and the row is `done`.
- Press Fetch on a book whose metadata is already complete. Still "Nothing
  to add", still the success treatment. This is the case the fix must not
  touch.
