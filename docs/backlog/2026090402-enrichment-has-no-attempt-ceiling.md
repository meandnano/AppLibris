# Backlog: enrichment has no attempt ceiling

## What is left of this item

It was filed with three gaps. Two have become plans and are no longer
described here:

- **Gap 1, a total provider failure recorded as success** →
  `docs/plans/2026090601-enrichment-failure-honesty.md`. It stopped being
  merely an observability hole when step 06 shipped a button: the control
  renders such a run as "Nothing to add" in the success treatment, which
  is a false statement to a person's face rather than a distinction a
  table cannot express.
- **Gap 3, an ISBN lookup never falling back to a title search** →
  `docs/plans/2026090602-search-match-confidence.md`, where it belongs
  beside the match-confidence guard that makes the search path safe to
  widen. The two were always one decision, as both items said.

What remains is gap 2, below. It is unchanged in substance and still
correctly backlog.

## Problem

Nothing records how many times a book has been through enrichment, or
whether those runs achieved anything, so there is no way to stop asking
about a book that will never resolve.

`enrichment_jobs` has `id`, `book_id`, `status`, `failure_reason`,
`updated_fields`, `queued_at`, `started_at`, `finished_at` — no attempt
count, and nothing on `books` either. Terminal history isn't pruned, so
*counting* a book's rows is possible today without a schema change.

Until `2026090601` lands, that count also cannot distinguish "asked twice,
both times the API was down" from "asked twice, this book is not in either
catalogue" — which is precisely why that plan is the prerequisite for this
one and not merely adjacent to it. A ceiling built on today's rows would
count the network's bad days as evidence about the book.

`WithRetry` bounds attempts *within* one lookup (`DefaultRetryAttempts`,
3). Nothing bounds how many times a book is put through the whole chain.

## Why this is backlog, not a plan

Nothing triggers enrichment except a person pressing a button. Step 06
built exactly that and explicitly ruled out both a library-wide "enrich
everything" and automatic enrichment on scan; DESIGN.md's deferred list
records all three as decisions rather than gaps.

With a human as the trigger there is no loop to bound. Pressing the button
again is a deliberate choice, and a person who presses it twice on a book
that found nothing has learned something the log did not tell them.

It becomes load-bearing the moment enrichment is triggered by anything
other than a person — a bulk action, a scan hook, a periodic re-run —
because at that point every permanently-unenrichable book is re-asked on
every trigger, spending a ~1 req/s rate-limit budget and Google Books'
daily quota (which this project has already been observed to exhaust
anonymously) on books that cannot resolve, crowding out books that could.

DESIGN.md's deferred list already names this as the thing that has to
arrive alongside automatic enrichment, so the trigger for acting on it is
that decision being revisited, not this file.

## Sketch

- **Count only informative attempts.** A run where every provider errored
  says nothing about the book, only about the network that day, and must
  not consume ceiling budget. Otherwise one Google Books quota-exhaustion
  day could permanently retire a shelf of perfectly enrichable books — a
  worse failure than the unbounded retrying this item exists to stop.
  `2026090601` is what makes such a run identifiable, by recording it as
  `failed` with a reason rather than as a bare `done`.
- **Then the ceiling itself.** Count a book's terminal `enrichment_jobs`
  rows that were informative, and refuse to enqueue past a maximum. The
  count is already available, so this may need no schema change at all.
  The number should be a named constant carrying its reasoning, the way
  `MaxCoverBytes` and `DefaultRetryAttempts` are.
- **A person's explicit request is never blocked — decided, not open.**
  The ceiling governs automatic enqueues only. Someone looking at a book
  and asking for it to be enriched has supplied exactly the judgement the
  ceiling substitutes for in their absence, and a control that refuses is
  a control that has to explain "we have given up on this book" and offer
  a way to overrule itself — a state and a reversal path bought for
  nothing.

  That has one sharp consequence for where the cap goes: **not inside
  `storage.EnqueueEnrichment`.** `Service.EnrichBook` and any future
  automatic trigger both call that one method, so a ceiling added there
  would block the button by construction. It belongs in the automatic
  caller — or, if it must be enforced in storage, behind a distinct entry
  point the button does not use.

  Note the contrast with the dedup guard already in that method
  (`WHERE NOT EXISTS … status = queued`), which is correctly shared: a
  double-press should not double-queue, and that is true whoever pressed.
  A ceiling is the opposite kind of rule — it is *about* who is asking, so
  it cannot live where the caller is invisible.

## Validate before planning

- Confirm `2026090601` has landed and that a run where every provider
  failed is distinguishable in `enrichment_jobs`. Without it, any count
  built here counts the wrong thing.
- Confirm the automatic trigger this ceiling is for actually exists or is
  being built. A ceiling with no automatic caller has nothing to govern.
