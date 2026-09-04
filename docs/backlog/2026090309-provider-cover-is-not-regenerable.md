# Backlog: a provider-fetched cover is not regenerable by the scanner

## Problem

`COVERS_DIR` is documented as disposable — delete it and the next sweep
rebuilds every thumbnail from the book files themselves. Step 05
(`docs/plans/completed/`, Open Library and Google Books providers)
introduced a second source for `books.cover_path`: a cover a provider
supplied, stored through the same `internal/cover.Store` path but with no
embedded original behind it.

`internal/scanner`'s `maybeRegenerateCover` only knows the embedded source.
For a book whose `cover_path` points at a file that is now missing or
zero bytes, it calls `readEmbeddedCover` on the book file and, finding
nothing, logs `regenerate cover failed … "embedded cover is missing"` — on
every sweep, forever — while `cover_path` keeps pointing at a file that
isn't there. The grid renders a broken image for that book, and the log
line repeats indefinitely.

The ordinary path is unaffected: a stored cover that is present and
non-empty short-circuits before any of this, which is why the tests and
normal operation don't show it. It takes wiping or losing `COVERS_DIR`,
which is precisely the operation the directory is advertised as
supporting.

## Why this is backlog, not a plan

It corrupts nothing, and it can't be reached at all on a deployment today:
nothing in production enqueues an enrichment job yet, so no book has a
provider-supplied cover to lose. Reaching it requires both a future
enrichment run and a manual `COVERS_DIR` wipe. It also isn't visibly wrong
in the shipped app for the same reason.

## Sketch

The fix and step 06 (`docs/plans/2026090306-enrichment-in-the-ui.md`) want
the same missing piece — a way to ask for a book to be enriched again — so
this is best decided alongside it rather than ahead of it.

Two candidate shapes, to be chosen when step 06 is planned:

- Have `maybeRegenerateCover` consult `field_sources` for `cover`. The test
  is "a row exists" and not "the row names a provider rather than
  `embedded`": `setEmbeddedFieldSourcesTx` never writes a `cover` row, so a
  scanner-extracted cover has no provenance at all and a comparison against
  `embedded` would match nothing. When a row exists — necessarily a
  provider's name, the only writer being `ApplyEnrichedFields` — clear
  `cover_path` and its `field_sources` row instead of warning, so the book
  reads as "no cover" again — which the grid already renders as a dashed
  box, and which `enrich.Resolve`'s `isMissing` would then treat as worth
  asking about on the next enrichment run. Costs one extra read per
  regeneration attempt, on a path that is already the uncommon one.
- Or leave `cover_path` alone and have step 06's re-enrichment action be
  the only repair, downgrading the log line to Debug so it stops being
  noise. Simpler, but leaves a broken image on the grid until someone
  notices and asks.

Either way the current behaviour — a repeated Warn and a dangling path —
should not survive step 06.
