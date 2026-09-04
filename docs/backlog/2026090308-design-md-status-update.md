# Backlog: update DESIGN.md's provider-enrichment status on `init`

## Problem

`docs/plans/completed/2026090304-enrichment-queue-and-resolver.md` shipped
the `enrichment_jobs` table, `internal/enrich`'s worker/`Provider`
interface/resolver, and `field_sources`' first reader
(`FieldSourcesForBook`). The plan's own "DESIGN.md (on `init`)" section
calls for moving that document's Metadata section status from "Embedded
and provenance are built; providers are not" to noting that the chain,
interface and resolver now exist and only the provider implementations are
outstanding.

That edit was not made. DESIGN.md lives only on the `init` branch — it was
never merged into `master`, and this plan's implementation PR (targeting
`master`, on a branch created from it) has no path to that file. Right now
`init`'s DESIGN.md still reads "Not built" for the whole provider chain,
provenance as write-only, and the resolver as design-only, which
understates what actually shipped.

## Why this is backlog, not a plan

It doesn't corrupt data, didn't block step 05 (Open Library / Google
Books providers, since landed) or step 06 (enrichment UI) from being
planned or built — both already exist as plan files and neither depends
on DESIGN.md's prose — and it isn't visibly wrong in the shipped app,
since DESIGN.md isn't part of what ships. It's a documentation drift between two branches that
were already diverging before this step (the whole reason the plan calls
the edit out as its own section rather than folding it into the code
change).

## Sketch

On the `init` branch (not `master`), update DESIGN.md's Metadata section
(the block starting "**Provenance is now built**; the provider chain, the
provider interface, compile-time registration and the resolver remain
design only.") to reflect that the whole chain is now built: the queue,
worker, `Provider` interface and resolver (`internal/enrich`), both
provider implementations (`internal/openlibrary`, `internal/googlebooks`),
the rate-limit/cache/retry decorators, and the compile-time registry
(`internal/providers`) that `METADATA_PROVIDERS` drives.

Step 05 has since landed, so the status-table rows go straight to `Built`
rather than the `Partial` this item originally proposed as an intermediate:
`| Metadata providers, chain | Built |`, and `Metadata provenance` from
`Partial` to `Built` (the `cover` value and `ApplyEnrichedFields` completed
it). With those two, DESIGN.md has no in-scope unbuilt items left, which is
worth saying plainly in that document's implementation-status intro rather
than leaving a reader to compare every row.
