# Backlog: the title/author fallback accepts a provider's top hit unchecked

## Problem

`enrich.Resolve` asks a provider by ISBN when the book has one and falls
back to `Search(ctx, book.Title, authors)` when it doesn't
(`internal/enrich/resolver.go`). Both providers answer that search with
their first result and no similarity test at all —
`internal/openlibrary`'s `search` returns `c.toMetadata(parsed.Docs[0])`,
`internal/googlebooks`' returns `toMetadata(parsed.Items[0])` — so
whatever the remote ranking puts first becomes the answer.

The books that reach this path are exactly the ones with the least to
match on. A book with no ISBN is the sparse-metadata case, and
`internal/scanner`'s `filenameTitle` means such a book's stored title is
often the filename rather than a real title. So the query can be a
filename, and a provider that returns *something* for it supplies
`publisher`, `published_date`, `language`, `isbn`, `description` and a
cover for a different book entirely.

A wrong match is then permanent. `ApplyEnrichedFields` records the
provider's name in `field_sources`, so the fields are no longer empty and
`isMissing` never reconsiders them on a later run; only a hand edit
undoes it. A wrong `isbn` is the worst of these, since it is also the
lookup key every subsequent enrichment would use.

## Why this is backlog, not a plan

Nothing in production enqueues an enrichment job yet — step 06
(`docs/plans/2026090306-enrichment-in-the-ui.md`) is what starts that — so
no book can currently acquire a wrong match, and nothing is visibly wrong
in the shipped app. The ISBN path, which is the one an EPUB with real
embedded metadata takes, is unaffected: it matches on an identifier, not
a ranking.

## Sketch

To be decided when step 06 is planned, since that step is what makes the
path reachable:

- Gate a `Search`-sourced answer on a minimum confidence before merging —
  normalised title equality or containment, or an author-name overlap
  when the book has authors — and treat a failing answer as "no match",
  which the four-case contract already has a shape for.
- Or narrow what a `Search` answer may fill. A cover and a description
  from a near-miss are cheap to be wrong about; an `isbn` written from a
  fuzzy title match is not, and refusing that one field alone removes
  most of the permanence.
- Either way, log the matched title alongside the provider name, so a bad
  match is diagnosable from the record rather than only from the result.
