# Backlog: enrichment has no attempt ceiling, and no way to build one

## Problem

Nothing records how many times a book has been through enrichment, or
whether those runs achieved anything. So there is no way to stop asking
about a book that will never resolve — and, worse, no way to *tell* one
from a book nobody has asked about yet.

Three specific gaps, each verified against the code as it stands after
step 05:

**1. A total provider failure is recorded as success.** `internal/enrich`'s
worker ends with:

```go
if len(values) > 0 {
	// ... ApplyEnrichedFields, fail on error
}
w.done(ctx, job.ID)
```

`Resolve` logs and skips a failing provider (`slog.Warn`, then `continue`)
and always returns a nil error, so a job in which *every* provider errored
— a 429, a 5xx, a timeout — arrives here with an empty `values` map and
goes straight to `w.done`. In `enrichment_jobs` that row is
`status='done'`, `failure_reason=''`: byte-for-byte identical to a job
where both providers were reached, answered cleanly, and genuinely had
nothing to say about an obscure book. The distinction that matters most
for deciding whether to try again is the one the table cannot express.

**2. There is no attempt counter.** `enrichment_jobs` has `id`, `book_id`,
`status`, `failure_reason`, `queued_at`, `started_at`, `finished_at` — no
attempt count, and nothing on `books` either. Terminal history isn't
pruned, so *counting* a book's rows is possible today without a schema
change, but by gap 1 that count cannot distinguish "asked twice, both
times the API was down" from "asked twice, this book is not in either
catalogue".

**3. A failed or missed ISBN lookup never falls back to a title search.**
`Resolve` picks one path per provider and does not reconsider:

```go
if book.ISBN != "" {
	answer, perr = p.ByISBN(ctx, book.ISBN)
} else {
	answer, perr = p.Search(ctx, book.Title, authors)
}
if perr != nil {
	slog.Warn(...)
	continue
}
```

`Search` is reachable only for a book with no ISBN at all. A book whose
ISBN is absent from both catalogues, or whose ISBN lookup errors, is never
searched by title even though the data to do so is right there — so it is
permanently unenrichable while looking, in the job log, like an ordinary
completed run. Whether an ISBN *error* should fall back is a genuine
question (a transient 5xx is not evidence the ISBN is wrong), but an ISBN
that returns a clean "no match" almost certainly should.

Together these mean the retry story is unbounded at one end and
uninformed at the other: `WithRetry` bounds attempts *within* one lookup
(`DefaultRetryAttempts`, 3), and nothing at all bounds how many times a
book is put through the whole chain.

## Why this is backlog, not a plan

Nothing in production enqueues an enrichment job. Step 06
(`docs/plans/2026090306-enrichment-in-the-ui.md`) makes enrichment a
per-book button a person presses, and it explicitly rules out both a
library-wide "enrich everything" and automatic enrichment on scan. With a
human as the trigger there is no loop to bound: pressing the button again
is a deliberate choice, and a person who presses it twice on a book that
found nothing has learned something the log did not tell them.

So this does not corrupt data (unlike
`docs/plans/2026090401-openlibrary-field-fidelity.md`, which does, and is
therefore a plan), and it does not block step 06 from being built. It
does *shape* whatever comes after: the ceiling becomes load-bearing the
moment enrichment is triggered by anything other than a person — a bulk
action, a scan hook, a periodic re-run — because at that point every
permanently-unenrichable book gets re-asked on every trigger, spending a
~1 req/s rate-limit budget and Google Books' daily quota (which this
project has already been observed to exhaust anonymously, see
`docs/plans/2026090401-openlibrary-field-fidelity.md`) on books that
cannot resolve, crowding out books that could.

Re-validate gap 1 before acting: it is the cheapest to fix and the other
two depend on it.

## Sketch

Fix the observability first, then the ceiling — a cap built on top of gap
1 would count the wrong thing.

- **Distinguish "nothing found" from "nobody answered".** `Resolve`
  already knows: it logs each provider failure. Have it report how many
  providers it asked and how many failed, and let the worker record a job
  where every provider failed as something other than a bare `done` — a
  `failed` row with a reason, or a fourth terminal state if `failed`'s
  "the job itself went wrong" meaning should stay narrow. This is the
  prerequisite for everything below and is worth doing even if no ceiling
  is ever built.
- **Then a per-book ceiling.** Count a book's terminal `enrichment_jobs`
  rows and refuse to enqueue past a maximum — the count is already
  available, so this may need no schema change at all. The number should
  be a named constant with its reasoning, the way `MaxCoverBytes` and
  `DefaultRetryAttempts` are.
- **Count only informative attempts.** A run where every provider errored
  should not consume ceiling budget: it says nothing about the book, only
  about the network that day. Otherwise one Google Books quota-exhaustion
  day could permanently retire a shelf of perfectly enrichable books,
  which is a worse failure than the unbounded retrying this item exists to
  stop.
- **A person's explicit request is never blocked — decided, not open.**
  The ceiling governs automatic enqueues only. If there is a button, it
  works: someone looking at a book and asking for it to be enriched has
  supplied exactly the judgement the ceiling exists to substitute for in
  their absence, and a control that refuses is a control that has to
  explain "we have given up on this book" and offer a way to overrule
  itself — a state and a reversal path bought for nothing.

  That has one sharp consequence for where the cap goes: **not inside
  `storage.EnqueueEnrichment`.** Step 06's `Service.EnrichBook` and any
  future automatic trigger both call that one method, so a ceiling added
  there would block the button by construction. It belongs in the
  automatic caller — or, if it must be enforced in storage, behind a
  distinct entry point the button does not use. Note the contrast with the
  dedup guard already in that method (`WHERE NOT EXISTS … status =
  queued`), which is correctly shared: a double-press should not double-
  queue, and that is true whoever pressed. A ceiling is the opposite kind
  of rule — it is *about* who is asking, so it cannot live where the
  caller is invisible.
- **Decide the ISBN fallback separately.** Falling back to a title search
  after a clean "no match" is a small change to `Resolve` and probably
  right; falling back after an *error* is not, and conflating them would
  accept a weaker title-search answer because of a transient failure.
  Related: `docs/backlog/2026090310-search-fallback-accepts-the-top-hit.md`
  records that the title search takes a provider's top hit with no
  similarity test, so widening how often that path is taken makes that
  item more urgent, not less. The two should be considered together.
