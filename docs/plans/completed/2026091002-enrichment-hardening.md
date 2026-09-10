# Plan: enrichment hardening

## Position in the sequence

Independent of the other 2026-09-10 plans. Touches `internal/enrich`,
`internal/sender`, `internal/googlebooks`, `internal/openlibrary` and one
rule in `internal/web/static/css/app.css`. No schema change.

This plan replaces seven backlog items, deleted in the same change that
adds it:

- `2026090715-enrichment-job-panic-crash-loop`
- `2026090716-cover-fetch-reaches-private-addresses`
- `2026090717-openlibrary-placeholder-cover`
- `2026090718-googlebooks-detail-failure-cached`
- `2026090611-refused-redirect-is-retried`
- `2026090610-description-paragraphs-do-not-render`
- `2026090609-provider-language-can-be-wrong`

Six are fixes. The seventh is a decision to do nothing, recorded here so it
stops being an open question.

## Context

Enrichment is a background job against one book: a `Worker` claims an
`enrichment_jobs` row, `Resolve` asks the configured providers for the
fields the book is missing, and the worker fetches a cover and writes the
answers. Each of the items above is a place where that pipeline is less
robust than its own rules say, re-validated against the code on 2026-09-10:

- `internal/enrich/worker.go:174` `process` and `internal/sender/sender.go:199`
  `process` have no `recover`. A panic in a job kills the process with the
  row left `running`. `storage.RequeueInterruptedEnrichment` then requeues
  it at the next start and `Run` drains immediately, so the same input
  panics again. Under a container restart policy that is a loop with no
  exit but editing the row by hand. The sender has the smaller version:
  `FailInterruptedSends` fails the row, so a panicking send costs one
  restart. No panic path is known today; the failure mode is structural.
- `internal/enrich/cover.go:79` and `CheckCoverRedirect` at `:48` check a
  cover URL's scheme and hop count and nothing else. The worker's
  `coverClient` (`worker.go:104`) uses the default transport, so a provider
  response or a redirect from whichever host answered can point the fetch
  at `127.0.0.1`, an RFC 1918 address, a link-local metadata endpoint or
  another container. The request is blind and only image bytes are kept, so
  exposure is low, but the redirect check reads as if it guards hops and
  guards only their scheme.
- `internal/openlibrary/openlibrary.go:208` and `:335` build a cover URL as
  `{coverBaseURL}/b/id/{id}-L.jpg`. Open Library documents that a missing
  cover is answered `200` with a placeholder image unless the URL carries
  `?default=false`, in which case it is `404`. A stale `cover_i` is
  ordinary in search results, so the placeholder is stored under the
  provider's name as a real cover and never reconsidered.
- `internal/googlebooks/googlebooks.go:425` `enrichVolume` swallows every
  failure of the detail request at Debug and leaves the list answer in
  place, returning nothing to its caller. That is right for the book in
  hand, but `search` returns the result with `nil` error and
  `enrich.WithCache` (`decorator.go:135`, `:148`) stores every nil-error
  answer for the process lifetime. A transient failure of the second
  request becomes a permanently degraded answer for that key until restart.
- Both `checkRedirect` policies (`googlebooks.go:87`, `openlibrary.go:81`)
  return plain errors, and both lookup paths wrap every `httpClient.Do`
  error in `enrich.ErrRetryable` (`googlebooks.go:352`,
  `openlibrary.go:359`). A refused redirect is therefore retried
  `DefaultRetryAttempts` times, though the policy is a pure function of a
  URL that does not change between attempts. Neither host redirects today.
- `books.description` can hold paragraph breaks — Google's `plainText`
  emits them and the edit textarea preserves them — but
  `.detail__description` (`app.css:931`) sets no `white-space`, so the read
  view collapses them into one paragraph.
- A `Search`-sourced `language` can be wrong on a correctly identified
  volume (a captured `O Alquimista` answered `en`). The plausibility gate
  compares title and author, which say nothing about language.

## Scope

In scope: the six fixes above and the recorded decision on `language`.

Out of scope, with reasons:

- **A TTL on `WithCache`.** Broader than the one problem it would solve
  here, and it changes the argument the cache was built on (a shelf of
  obscure books re-asked on every run). A partial answer is the specific
  thing that should not be cached, so that is what is marked.
- **A host allowlist for cover fetches.** `covers.openlibrary.org`
  legitimately redirects to archive.org backends and Google's image hosts
  vary. An allowlist would refuse real covers to close a hole an address
  check closes more precisely.
- **Rendering description paragraphs as `<p>` elements.** It touches the
  inline editor's swap target and the grid card's excerpt for a result one
  CSS property already gives.
- **Withholding `language` from a search answer.** See Decision 7.
- **An attempt ceiling on enrichment.** Still governed by
  `docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`, and
  load-bearing only once enrichment is triggered by something other than a
  person.

## Decision 1: both workers recover a panic inside `process` and fail the job

A `defer` at the top of `enrich.Worker.process` and `sender.Worker.process`
recovers, logs at Error with the job or send id, the book id, the panic
value and `debug.Stack()`, and writes the row terminal through the existing
`fail` helper with a fixed reason: `"enrichment crashed — see the server
log"` and `"sending crashed — see the server log"`. `fail` already writes
under `context.WithoutCancel` plus `markTimeout`, so the verdict lands even
if the panic happened during shutdown.

The reason is a sentence for the status box, not the panic text: the panic
value may carry a URL, a key fragment or a Go type name, none of which a
person can act on, and the log line has all of it. The row going `failed`
is what puts the Retry button on the page and, for enrichment, what stops
`RequeueInterruptedEnrichment` from putting the same input back on the
queue. Retrying is the person's choice, made after the log has said why.

Rejected: recovering in `Run`'s loop. That saves one function per worker
but loses the job id at the point of recovery and, worse, leaves the row
`running`, which is exactly the state the requeue turns into a loop.

Rejected: recovering in enrichment only. The sender has the same shape with
a smaller blast radius (one restart, then `failed`), but one restart of the
whole server for a send job is still the server down, and the change is
the same five lines.

## Decision 2: the cover fetch refuses private and local addresses at dial time

The worker's `coverClient` gets an `http.Transport` whose `DialContext`
resolves the host, checks every returned IP with one predicate, and refuses
to connect if any of them is loopback, private (RFC 1918 and the IPv6 ULA
range `net.IP.IsPrivate` covers), link-local unicast or multicast (v4 and
v6), multicast, or unspecified. A refused dial surfaces as an ordinary
fetch failure, which `storeCover` already tolerates, logged at Debug with
the resolved address so a misconfigured local mirror can be diagnosed.

**Correction, found while implementing.** Resolving the host and then
dialing it is the hole this decision exists to close, one layer down: the
address checked and the address connected to are two separate lookups, so
a short TTL or a rebinding answer slips between them. The implementation
uses `net.Dialer.Control` instead, which Go calls once per candidate
address *after* resolution and *before* the connect, with the address the
dialer actually settled on — no window, no manual resolve, and the
per-hop and per-DNS-answer coverage the decision asks for. The exported
predicate is still `RefusePrivateAddress(net.IP) error`, as specified;
only the hook it hangs on differs. The transport is a clone of
`http.DefaultTransport` so proxy and TLS defaults survive.

**Second correction, from review.** Two more things this decision's list of
ranges got wrong, both found by probing the implemented predicate rather
than reading it. `net.IP`'s own methods do not cover 100.64.0.0/10, which
is carrier-grade NAT *and* the range Tailscale assigns — so on the
deployment the README recommends, every tailnet peer was reachable by a
cover URL, which is precisely the address class this decision exists to
refuse. Nor do they cover 0.0.0.0/8, 192.0.0.0/24, 198.18.0.0/15,
240.0.0.0/4, 255.255.255.255 or the deprecated IPv6 site-local fec0::/10,
or the IPv4 embedded in a NAT64 (64:ff9b::/96) or 6to4 (2002::/16)
address. All are refused explicitly now, the translated ones by re-checking
the address they carry — at byte 12 for NAT64 and byte 2 for 6to4, which
are not interchangeable. IPv4-mapped IPv6 needed nothing: `net.IP`'s
predicates go through `To4`.

And the transport's proxy is cleared rather than kept. `Control` sees the
address the dialer connects to, which through a proxy is the proxy's: a
proxy on a LAN address would be refused as private and no cover would ever
be fetched, while a public one would let a cover URL reach anything the
proxy can with the guard checking the wrong host and reporting success. A
cover is a direct GET of a public image, so honouring a proxy here buys
nothing and cannot be done without giving up the check.

Checking at dial rather than on the URL string is what makes the check
cover every redirect hop and DNS rebinding alike: a hostname that resolves
to a public address when the URL is inspected and a private one when the
connection is made is caught, and so is a redirect to a bare IP literal.

The predicate is `func(net.IP) error`, exported from `internal/enrich` as
`RefusePrivateAddress` and held on the `Worker` as `dialGuard`, defaulted
by `New`. Tests need to replace it: every cover test in the package runs an
`httptest.Server` on `127.0.0.1`, which the real guard refuses by design.
The replacement is a package-private test helper that sets the field to a
permissive guard, so production code has no "allow loopback" switch and
the tests say in one place that they are opting out of it.

Rejected: a check inside `FetchCover` on the parsed URL's host. It would
catch an IP literal in the first URL and nothing a redirect or DNS answer
chose later, which is the case the item was filed for.

## Decision 3: the Open Library cover URL asks for a 404 instead of a placeholder

Both places that build the URL — `toMetadata` for a search document and the
Read API path for an edition's `covers[0]` — append `?default=false`. That
is the whole change on the provider side: `FetchCover` already treats any
non-200 as a failed fetch, and the worker already leaves a failed cover out
of `Values`, so a book whose only cover id is stale keeps an honest "no
cover", stays in the missing set for the next provider, and can be asked
again later.

The two format strings become one helper so the parameter cannot be added
to one path and forgotten on the other.

A live capture of the `404` body is committed beside the existing
`edition_*.json` fixtures with its provenance noted at the top of
`openlibrary_test.go`, if network access is available when this lands. If
it is not, the test asserts the URL shape only and its comment says the
`404` behaviour is documented rather than captured, per the fixture
convention in `docs/notes/enrichment.md`.

**Correction, found while implementing.** Network access was available and
the behaviour was verified live on 2026-09-10, but no fixture was
committed: the `404` body is the thirteen bytes `404 Not Found`, and
nothing in this package parses a covers-host response at all — only the
URL shape is its business, which is what the test asserts. A file no test
reads is not a fixture. The measurement is recorded in
`openlibrary_test.go`'s provenance comment instead, including the finding
that the placeholder the parameter suppresses is a 43-byte 1x1 GIF for an
unknown id rather than the grey cover image the Decision assumed.

Rejected: detecting a placeholder from the bytes. Nothing in `FetchCover` or
`cover.Store` can tell a small JPEG that says "no cover" from a small JPEG
that is one, and the same reasoning already keeps `internal/googlebooks`
away from rewriting the `zoom` parameter.

## Decision 4: a partial Google Books answer is not cached

`enrichVolume` returns an error to its caller instead of swallowing it.
`search` still returns the list answer with `nil` error — the six text
fields in hand are the answer, and losing them over the second request
would be the wrong trade — but sets a new `Metadata.Partial` field to
`true` when the detail request failed for any reason (transport, non-200,
malformed body, a body naming another volume). `WithCache` stores a result
only when `!m.Partial`, so the next lookup for that key asks again and the
detail request gets another chance.

`Resolve` ignores `Partial`. A partial answer is still an answer: the
provider was asked and it spoke, so it counts toward `Asked` and not toward
`Failed`, and the worker's `Asked > 0 && Failed == Asked` rule is
unaffected. `IsEmpty` ignores it too; a partial answer with text in it is
not empty.

`Partial` is the one field on `Metadata` that describes the answer rather
than the book, and it is documented as such: providers other than Google
never set it, and nothing downstream of the cache reads it.

Rejected: a TTL on cache entries (see Scope). Rejected: caching the
partial answer and re-running only the detail request on a hit, which
would mean the cache knowing which provider it wraps.

## Decision 5: a refused redirect is not retryable

Each package gets an unexported sentinel, `errRedirectRefused`, and every
`return` in its `checkRedirect` wraps it. `http.Client.Do` returns a
`CheckRedirect` error inside a `*url.Error`, which unwraps, so the lookup
paths can test `errors.Is(err, errRedirectRefused)` and classify it as
non-retryable — the same class as a 400 or 403, and for the same reason
already written beside those: another attempt answers identically. The
error is still wrapped through `redactKey` on the Google side. Both
packages change in one commit, since `docs/notes/enrichment.md` describes
them as shaped identically and fixing one would make that sentence false.

The adjacent hole recorded in the backlog item is decided here too.
`internal/openlibrary`'s `checkRedirect` has no host check, so a redirect
off `openlibrary.org` would make that client adopt the answering host's
whole response, gated only by title and author on the search path and by
nothing on the ISBN path. `TestByISBNFollowsARedirect` shows the client
depends on following redirects (an ISBN aliasing the canonical edition
key), and every such hop anyone has seen is same-host. Implementation
verifies that live against a few aliased ISBNs; if every observed hop stays
on `openlibrary.org`, the same `sameHost` check `internal/googlebooks`
carries is added here, with a same-host downgrade refused as well. If a
legitimate cross-host hop is observed, the check is not added and the
observation is recorded in `docs/notes/enrichment.md` as the reason.
`sameHost` moves to `internal/enrich` so both clients use one comparison.

**Verified while implementing (2026-09-10).** The Read API answers an ISBN
directly — `/api/volumes/brief/isbn/{isbn}.json` was `200` with no redirect
for every ISBN probed — and the `/isbn/{isbn}` and `/isbn/{isbn}.json`
aliases redirect one and two hops respectively, every hop on
`openlibrary.org`. No cross-host hop was observed, so the check is added.

**Correction, found while implementing.** Adding it made the two
`checkRedirect` functions identical — five clauses, the same messages, the
same hop bound, and two sentinels differing only in which package they sat
in. A per-package sentinel was right while the policies differed and stopped
being right the moment this decision made them the same. So the whole policy
is shared, not only the comparison: `enrich.CheckLookupRedirect`,
`enrich.ErrRedirectRefused` and `enrich.MaxLookupRedirects` live beside
`SameHost` in `internal/enrich/redirect.go`, which is also why that file
earns its place. Each client keeps its own one-line `errors.Is` test, which
has to stay per-client because it sits where each wraps `ErrRetryable`.

## Decision 6: description paragraphs render, and only paragraphs

`.detail__description` gets `white-space: pre-line`. That keeps line breaks
and collapses runs of spaces and tabs, so a provider's leading indentation
and stray double spaces are not reproduced on the page. `pre-wrap` would
preserve those too, which is the reason the backlog item held back; with
`pre-line` the trade goes away.

The rule applies to the class, so it reaches both the read view and the
edit `<textarea>`, which already preserves whitespace and is unaffected.

`sanitizeValue` also caps consecutive newlines at two for
`storage.FieldDescription`, making "at most one blank line between
paragraphs" a property of every provider's value rather than of
`internal/googlebooks`' `collapseBlankLines` alone. Open Library's edition
descriptions are plain and mostly single-block today; the cap costs nothing
there and holds for the next provider.

Rejected: normalising only on the way in and leaving the CSS. Values
already stored would never render their paragraphs, and the textarea would
go on showing structure the page hides.

## Decision 7: `language` from a search answer stays accepted

No code changes. The plausibility gate already refuses a cross-language
mismatch when it shows up as a transliterated title (`title_mismatch`).
Over the Russian no-ISBN population the search path exists for, the gate
accepted four correct `ru` values and one wrong `en`; withholding
`language` the way `isbn` is withheld would drop the four to avoid the one.
The wrong value is one visible field, carries the provider marker the
detail page renders for any provider-supplied value, and is hand-fixable,
after which it is `manual` and never touched again. That is the feature
working as designed, and `docs/notes/enrichment.md` says so in one sentence
so the question is not reopened from the backlog.

## Changes

- `internal/enrich/worker.go`: `defer` with `recover` at the top of
  `process`, a `crashedReason` constant beside the other reasons, a
  `dialGuard func(net.IP) error` field on `Worker` defaulted in `New`, and
  `coverClient` built with a `Transport` whose `DialContext` applies it.
- `internal/enrich/cover.go`: `RefusePrivateAddress(net.IP) error` and the
  dialer that resolves and checks; `sameHost(a, b *url.URL) bool` moved
  here from `internal/googlebooks`.
- `internal/enrich/provider.go`: `Metadata.Partial bool`, documented as a
  property of the answer that only `WithCache` reads.
- `internal/enrich/decorator.go`: `cachedProvider.ByISBN` and `Search` skip
  `put` when `m.Partial`.
- `internal/enrich/resolver.go`: `sanitizeValue` caps consecutive newlines
  at two for `FieldDescription`.
- `internal/sender/sender.go`: the same `defer`/`recover` at the top of
  `process`, with `crashedReason`.
- `internal/googlebooks/googlebooks.go`: `errRedirectRefused` wrapped by
  every `checkRedirect` return; `search` classifies it non-retryable;
  `enrichVolume` returns an error and `search` sets `Partial` on it;
  `sameHost` imported from `internal/enrich`; the `enrichVolume` comment
  paragraph naming `docs/backlog/2026090610-…` is rewritten to describe the
  rendering as it now is.
- `internal/googlebooks/googlebooks_test.go`: the comment at `:944` naming
  `docs/backlog/2026090609-…` points at Decision 7's sentence in
  `docs/notes/enrichment.md` instead.
- `internal/openlibrary/openlibrary.go`: one `coverURL(id int) string`
  helper appending `?default=false`, used by both paths;
  `errRedirectRefused` and the non-retryable classification; the host and
  downgrade checks in `checkRedirect` if Decision 5's verification holds.
- `internal/openlibrary/testdata/`: a live `404` capture for a missing
  cover id, if network access allows.
- `internal/web/static/css/app.css`: `white-space: pre-line` on
  `.detail__description`.
- `docs/notes/enrichment.md`, `docs/notes/web.md`: see below. Every
  citation of the seven deleted backlog files in those notes is replaced by
  the current-state sentence.

## Tests

`internal/enrich`:

- `TestWorkerRecoversAPanickingProviderAndFailsTheJob`: a provider whose
  `Search` panics; the job ends `failed` with `crashedReason`, the worker
  goroutine is still alive (a second job is processed), and the row is not
  `running`.
- `TestRefusePrivateAddress`: table over `127.0.0.1`, `::1`, `10.0.0.1`,
  `172.16.0.1`, `192.168.1.1`, `fc00::1`, `169.254.169.254`, `fe80::1`,
  `224.0.0.1`, `0.0.0.0`, `::` (all refused) and `93.184.216.34`,
  `2606:2800:220:1:248:1893:25c8:1946` (allowed).
- `TestWorkerRefusesACoverOnALoopbackAddress`: the worker built by `New`
  with its real guard against an `httptest.Server`; the cover is not
  stored, the job is `done` when text fields resolved, the fetch failure is
  logged. Every other cover test replaces `dialGuard` through the test
  helper and asserts nothing about addresses.
- `TestCacheDoesNotStoreAPartialAnswer`: a fake provider answering
  `Partial: true` is called twice for the same key; a second fake answering
  `Partial: false` is called once.
- `TestResolveCountsAPartialAnswerAsAnswered`: `Asked` 1, `Failed` 0,
  fields merged.
- `TestSanitizeDescriptionCapsBlankLines`: three or more consecutive
  newlines become two; single newlines survive.

`internal/sender`:

- `TestWorkerRecoversAPanickingTransportAndFailsTheSend`: a `Transport`
  whose `Send` panics; the row ends `failed` with the sender's
  `crashedReason` and the worker processes the next send.

`internal/googlebooks`:

- `TestRefusedRedirectIsNotRetryable`: a redirect off-host on the list
  request; `errors.Is(err, enrich.ErrRetryable)` is false. Added to the
  existing classification table where it fits.
- `TestDetailRequestFailureMarksTheAnswerPartial` and
  `TestDetailRequestSuccessLeavesTheAnswerWhole`: extend the existing
  detail-failure tests to assert `Partial`.
- `TestDetailBodyNamingAnotherVolumeMarksTheAnswerPartial`.

`internal/openlibrary`:

- `TestCoverURLAsksForA404NotAPlaceholder`: both `ByISBN` and `Search`
  answers carry `?default=false`.
- `TestRefusedRedirectIsNotRetryable`: an endless chain and a `file://`
  hop both classify non-retryable.
- `TestByISBNRefusesARedirectOffHost`, if Decision 5's check is added.
- A test over the `404` capture, if one is committed.

`internal/web`:

- `TestDescriptionRendersParagraphBreaks`: a book whose description holds
  `\n\n` renders a `.detail__description` rule with `white-space: pre-line`
  present in the embedded stylesheet; the existing class-has-a-rule test
  covers the selector.

## docs/notes/enrichment.md

Under **Covers**: the fetch refuses private, loopback, link-local,
multicast and unspecified addresses at dial time, on every hop, and why
dial rather than URL. Open Library's cover URL carries `?default=false` so
a missing cover is a `404` the fetch already treats as failure, not a
placeholder stored as a cover.

Under **Providers: Google Books**: a detail-request failure leaves the list
answer in place and marks it `Partial`, which `WithCache` does not store,
so the next lookup asks again. Replace the sentence citing the paragraph
backlog item with: paragraph breaks reach the page through
`.detail__description`'s `pre-line`.

Under **Shared provider contract**: a refused redirect is not retryable,
and why (a policy over an unchanging URL answers identically). Record
whether Open Library's `checkRedirect` gained the host check and the
observation that decided it.

Under **Job outcomes**: a panic inside `process` is recovered, logged with
its stack and recorded as a `failed` job with `crashedReason`, and why the
row must not stay `running`.

Under **The plausibility gate**: one sentence that `language` from a search
answer is accepted, with the four-to-one measurement and the provider
marker as the reason.

Under **Sanitising values**: descriptions are capped at two consecutive
newlines.

## docs/notes/web.md

Under **Book detail and editing** (or **Styling**): `.detail__description`
is `white-space: pre-line`, so stored paragraph breaks render and runs of
spaces do not. Replace the sentence citing the paragraph backlog item.

## docs/notes/sending.md

Under the queue worker: a panic inside `process` is recovered and recorded
as `failed` with `crashedReason`, the same shape as enrichment.

## Verification

- `go vet ./...` and `go test -race ./...` green.
- Run the server with a provider fake whose `Search` panics once
  (`METADATA_PROVIDERS` pointed at a test registry entry, or a temporary
  local edit): press Fetch metadata; the control shows the crash reason
  with Retry, the log carries the stack, the process is still serving, and
  a restart does not re-run the job.
- With a scratch HTTP server on `127.0.0.1` answering a JPEG, point a fake
  provider's `CoverURL` at it: the job finishes with no cover stored and a
  Debug line naming the refused address. Point it at a redirect to that
  address from a public host: same result.
- Look up an Open Library book whose `cover_i` is known stale (or use the
  fixture's id): the detail page shows the dashed no-cover box, not a
  placeholder image, and the job reads "Nothing to add" or names the text
  fields it wrote.
- Open a book whose description has two paragraphs: the read view shows
  them as two paragraphs, the textarea unchanged.
- Fetch metadata for a book while Google's detail endpoint is blocked at
  the network, then unblock and fetch again: the second run upgrades the
  cover without a restart.
