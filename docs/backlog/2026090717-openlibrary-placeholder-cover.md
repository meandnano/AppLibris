# Backlog: an Open Library cover URL can return a placeholder image

## Problem

`internal/openlibrary` builds a matched result's cover URL as
`https://covers.openlibrary.org/b/id/{cover_i}-L.jpg`. Open Library's
covers API documents that a request for a cover it does not have is
answered with `200` and a small placeholder image unless the URL carries
`?default=false`, in which case it answers `404`.

Search results carry `cover_i` values for covers that have since been
removed, so a stale id is an ordinary occurrence. Nothing in
`enrich.FetchCover` or `cover.Store` can tell a placeholder JPEG from a
cover. It is stored under the provider's name, `field_sources` records
that a provider supplied it, and `Resolve` never reconsiders a field that
is filled.

This is the same class of problem `internal/googlebooks` explicitly
avoids for Google's `zoom=` parameter, where a size a volume lacks comes
back as a `200 image/jpeg` "image not available" placeholder. The reason
recorded there ("only a URL Google itself named is safe to fetch")
applies here with a different mechanism.

## Why this is backlog, not a plan

Not verified against the live API in this pass; the behaviour is
documented by Open Library but the fixture set here has no capture of a
placeholder response. The cost is one wrong cover, visible on the detail
page with a `via openlibrary` marker beside it, and a person can clear
it by wiping the file (the scanner's `ClearProviderCover` path forgets a
provider cover whose file is gone) and pressing Fetch again once the
provider answers `404`.

## Re-validate before acting

- Fetch a known-missing cover id both ways and confirm the `200` versus
  `404` behaviour is still what the documentation says.
- Whether both `ByISBN` and `Search` still build the URL the same way.

## Sketch

Append `?default=false` in the one place the URL is built. `FetchCover`
already treats a non-200 as a failed fetch, and the worker already
tolerates a failed fetch by leaving the cover out of `Values`, so the
book keeps an honest "no cover" and stays in the missing set for the
next provider or a later run. A live capture of the `404` should be
committed beside the existing `edition_*.json` fixtures, with the
provenance noted at the top of the test file per the convention there.
