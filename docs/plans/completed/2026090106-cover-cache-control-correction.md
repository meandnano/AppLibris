# Step: Correct the cover Cache-Control immutability claim

## Context

PR review of `2026090104-static-asset-serving` found the plan's own reasoning
for marking covers `Cache-Control: immutable` was wrong. The plan (and the
implementation following it) argued: "the bytes at a given URL can never
change, because different bytes get a different hash and therefore a
different URL" — but `cover.Store` is called as `cover.Store(coversDir,
book.ContentHash, coverBytes)`: the filename is the **book's** content hash
(the SHA-256 of the whole ebook file), not a hash of the resized,
JPEG-encoded thumbnail bytes actually written and served at that path.

So the invariant the plan relied on doesn't hold for the thing that's
actually served. A future change to `internal/cover`'s resize dimensions,
JPEG quality, or encoder — or a regeneration of an existing cover under a
changed pipeline (the scanner already regenerates a missing/zero-byte cover
on sweep, per `2026083111-cover-regeneration`) — can overwrite different
bytes at an unchanged URL. `immutable` promises a browser it never needs to
check again; that promise was false.

Also found: the test guarding this (`TestCoverCacheControlIsImmutable`)
only asserted the header *contained* `immutable`, and the ETag test for
static assets (`TestStaticAssetETagEnablesConditionalRequest`) only
asserted the ETag was non-empty and echoed back to a 304 — both would stay
green under a hardcoded, non-content-derived value, which is exactly the
failure mode that makes a validator worthless. A CLAUDE.md paragraph edit
in the same PR also overstated what the static-asset `max-age=300` policy
guarantees ("without risking a stale file surviving a deploy" — actually,
a client can serve stale content for up to five minutes post-deploy without
even consulting the ETag).

## Changes

- `coversHandler` (`internal/web/assets.go`) drops `immutable` and serves
  `Cache-Control: public, max-age=86400` instead — a day-long bound that
  still eliminates the redundant per-visit revalidation round trip this
  step exists to avoid, without promising a guarantee the naming scheme
  doesn't provide. `http.FileServer`'s own `Last-Modified` (from the file's
  mtime, untouched by this wrapper) is what lets a client revalidate once
  the window elapses.
- `TestCoverCacheControlIsImmutable` renamed to
  `TestCoverCacheControlIsBoundedNotImmutable`, asserting the header does
  **not** contain `immutable` and equals `public, max-age=86400` exactly.
- `TestStaticAssetETagEnablesConditionalRequest` renamed to
  `TestStaticAssetETagIsContentDerived`. Rather than accepting any non-empty
  ETag, it independently hashes the served response body (sha256, truncated
  to 8 bytes, quoted — the documented derivation) and asserts the `ETag`
  header equals that value, in addition to the existing conditional-request
  round trip. Also pins the exact `Cache-Control: public, max-age=300` on
  the same response.
- `TestStaticFileServed` and `TestCoverServedFromCoversDir` — the two tests
  the original plan pointed to as covering content integrity through the
  new wrapper handlers — now compare the response body against the actual
  source bytes (the embedded file, and the bytes written to the covers
  directory in setup) rather than only asserting status 200.
- CLAUDE.md's `internal/web` bullet rewritten: explains why neither mount
  gets `immutable`, describes the day-long cover policy and what backs its
  revalidation, and states the static-asset policy accurately — it *bounds*
  post-deploy staleness to five minutes rather than eliminating the risk.
- All four fixes verified with mutation checks (a constant ETag, and
  restoring `immutable` on the cover header) confirming the strengthened
  tests actually fail without the fix.

## Verification

- `go build ./...`
- `go vet ./...`
- `go test ./... -race`
- Manual: `curl -i` against a running server shows `Cache-Control:
  public, max-age=86400` (no `immutable`) on a cover request, and
  `Cache-Control: public, max-age=300` plus a content-derived `ETag` on a
  static asset request.
