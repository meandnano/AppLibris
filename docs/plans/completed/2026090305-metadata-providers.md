# Step: Open Library and Google Books providers

## Position in the sequence

**Second of four.** Depends on `2026090304` (the queue, the `Provider`
interface and the resolver). Blocks `2026090306` (the UI), which has
nothing to show until real answers arrive.

Step 04 built everything except the two things that touch a network. This
step writes them, plus the decorators DESIGN.md insists live outside them.

## Context

DESIGN.md specifies this step almost completely:

> Providers are walked in configured order — Open Library first, then
> Google Books.
>
> **Provider interface.** Deliberately tiny: fetch by ISBN; search by
> title + author; a name, for logging.
>
> Rate limiting, caching, and retries are **decorators wrapping
> providers**, not baked into each implementation.
>
> **Registration.** Compile-time. A map from provider name to
> constructor, in one file. Config lists names in order; the resolver
> looks them up. No dynamic loading, no plugin API contract, no
> versioning.

So the design questions are settled and this is mostly careful client
work. What is *not* settled, and what this plan decides, is how a
provider behaves when the network is unhelpful — which is most of the
risk, because this is the first code in the project that depends on a
third party staying up.

`internal/resend` is the local precedent for an HTTP client: its own
`*http.Client` with an explicit timeout rather than `http.DefaultClient`
(which has none), and a named constant for the limit it enforces with the
arithmetic in a comment.

## Scope

In scope: `internal/openlibrary`, `internal/googlebooks`, the decorators,
the compile-time registry, configuration, and the decision about covers
that step 04 deferred to here.

Out of scope, with reasons:

- **Any UI** — step 06.
- **A third provider.** The registry makes adding one a file plus a map
  entry; two is enough to prove the chain orders and merges correctly,
  which is the only thing a third would also prove.
- **Persistent caching across restarts.** The cache decorator is
  in-memory and bounded. A book is enriched once and then has no missing
  fields, so the cache exists to stop a burst of jobs hammering the same
  ISBN, not to be a long-lived store. Adding a table for it would be
  storage nobody has shown a need for.
- **Automatic re-enrichment on a schedule.** Nothing re-runs enrichment
  on its own; step 06 makes it something a person asks for.

## Decision 1: covers — deferred from step 04, decided here

Step 04 excluded covers because `field_sources.field` is CHECK-constrained
to the seven text fields and a cover cannot carry provenance without a
migration, and because whether covers are worth fetching depends on what
these two providers actually return.

**Decision: fetch covers, and migrate `field_sources` to allow `cover`.**

The reason is that the alternative is worse in a specific way. A book with
no embedded cover renders as a dashed "no cover" box, and that is the most
visible gap in the whole library grid — far more noticeable than a missing
publisher. Open Library's covers API is a stable, documented URL keyed by
ISBN, and Google Books returns thumbnail links in its volume payload. If
enrichment fills six text fields and leaves the one visible hole, the
feature looks broken to the person using it.

SQLite cannot alter a CHECK constraint in place, so this is the documented
table rebuild — create the replacement, copy the rows, drop the original,
rename — and since the repo's convention is one statement per file, it is
four files rather than one:

```
2026090304_create_field_sources_with_cover.sql
2026090305_copy_field_sources_to_new.sql
2026090306_drop_old_field_sources.sql
2026090307_rename_field_sources.sql
```

Applied in filename order, each in its own transaction, exactly as
`migrate` already does — the same shape the `books.file_path` → `book_files`
move used (`2026083010`–`2026083013`), which is the precedent to copy.

(That series is why the two later plans in this sequence take
`2026090308` and `2026090309` for their own migrations.)

**Two rules the cover path must follow**, both of which differ from the
text fields:

- **A fetched cover goes through `internal/cover.Store`**, exactly like an
  embedded one — resized to ~400px, JPEG, named by the *book's* content
  hash. It must not be stored as a remote URL. The covers directory is
  designed to be disposable and regenerable, and a URL in `cover_path`
  would break both: the file would not be there to regenerate, and the
  grid would make a third-party request per card.
- **A cover is only fetched when `cover_path` is empty.** The scanner's
  empty-string-means-"looked, found nothing" convention already exists
  and must not be confused with "not yet looked at".

Size limit: reject a response body over a few hundred KB before decoding
it. `internal/cover` resizes whatever it is handed, and handing a decoder
an arbitrary remote image is the one place this feature could be made to
allocate without bound.

## Decision 2: what a provider does when the network is unhelpful

Four distinct cases, deliberately not collapsed:

| Case | Behaviour |
|---|---|
| 200 with no match | Zero `Metadata`, nil error. **Not** an error — this is the common case. |
| 404 | Same as above. A missing record is an answer. |
| 429 / 5xx | Error. The retry decorator may retry; the resolver skips this provider and continues. |
| Timeout / transport error | Error, same handling. |

The first two collapsing into the third is the failure mode to avoid: it
would turn "this book is obscure" into a logged error on most books, and
an error log that fires constantly is one nobody reads.

Every provider gets its own `*http.Client` with an explicit timeout, per
the `internal/resend` precedent — and a *short* one (a few seconds).
Enrichment is a background nicety; a provider that is slow should be
skipped, not waited on.

## The decorators

Three, each wrapping a `Provider` and satisfying `Provider`, so they
compose in any order and the resolver cannot tell they are there:

```go
func WithRateLimit(p Provider, every time.Duration) Provider
func WithCache(p Provider, size int) Provider
func WithRetry(p Provider, attempts int) Provider
```

Composed once at construction: `WithRateLimit(WithRetry(WithCache(client)))`
— cache outermost of the three inner concerns so a cached hit costs
neither a token nor a retry budget; rate limit outermost overall so
retries are also paced.

Each is independently testable against a fake `Provider` with no HTTP at
all, which is the point of them being decorators. Specifically:

- **Rate limit**: a simple ticker-gated token, honouring `ctx`
  cancellation while waiting. Open Library asks for courtesy limits
  rather than enforcing hard ones; pick a conservative default and make
  it a named constant with that noted.
- **Cache**: keyed by the lookup arguments, bounded, in-memory. Caches
  the "no match" answer too — otherwise a shelf of obscure books re-asks
  on every run.
- **Retry**: only on the 429/5xx/transport cases above, never on a "no
  match", with a backoff and a `ctx` check between attempts. A retry that
  ignores context is a shutdown that hangs.

## Registration and configuration

One file, `internal/enrich/registry.go`:

```go
var providers = map[string]func() Provider{
	"openlibrary": func() Provider { return openlibrary.New() },
	"googlebooks": func() Provider { return googlebooks.New() },
}
```

`METADATA_PROVIDERS` (default `openlibrary,googlebooks`) lists names in
order; `cmd/server` resolves them through the map and passes the slice to
the worker. An unknown name **fails startup** with a message naming it
and listing the valid ones — unlike a missing `RESEND_API_KEY`, which
only warns. The difference is intent: an unset key means "I have not set
this up", while a misspelled provider name means "I asked for something
specific and did not get it", and silently running with fewer providers
than requested is the kind of thing nobody notices for months.

`METADATA_PROVIDERS=` (empty) disables enrichment, and is the documented
way to run without any outbound calls at all — worth stating in README
beside the other environment variables, since a LAN-only deployment is
exactly this project's assumed setting.

Google Books allows anonymous use at a low quota and takes an optional
`GOOGLE_BOOKS_API_KEY`; support it, warn (do not fail) when absent, and
keep the key out of logs.

## Tests

**Provider clients**, against an `httptest.Server` — no live network in
the suite, ever:

- A real captured response body for each provider, parsed into the
  expected `Metadata`. Store the fixtures as testdata files; a hand-written
  approximation of an API response tests the parser against your
  imagination rather than the API.
- The four network cases in Decision 2, each asserting error-or-not.
- ISBN normalisation matches what `internal/epub` stores, so a lookup key
  round-trips. This is the seam where a hyphen would silently produce
  zero matches forever.
- A malformed or truncated body is an error, not a panic.

**Decorators**, against a fake provider:

- Rate limit paces calls and returns promptly on a cancelled context.
- Cache serves a repeat without a second call, including for "no match".
- Retry retries a 5xx, does not retry a "no match", and stops on
  cancellation.

**Cover path:**

- A fetched image is resized and stored under the content hash, not saved
  as a URL.
- An oversized body is rejected before decoding.
- A book that already has a cover is not asked for one.

**Mutation checks:**

1. Make a 404 an error → the "no match is not an error" test fails.
2. Remove the cache's negative caching → the repeat-call test fails.
3. Store the remote URL in `cover_path` instead of the stored file → the
   cover test fails.
4. Accept an unknown provider name at startup → its test fails.

## CLAUDE.md

New `internal/openlibrary` and `internal/googlebooks` bullets in the
house style — what each reads, the four-case error contract, and the
fixture-based tests. The `internal/enrich` bullet gains the decorators
and the registry, and states the composition order with its reason.

## DESIGN.md (on `init`)

The Metadata status becomes Built: the chain, interface, registration and
both providers exist as designed. The status table row `Metadata
providers, chain` moves from `Not built` to `Built`, and `Metadata
provenance` from `Partial` to `Built` — the resolver reading it is what
the "nothing reads it" caveat was waiting for.

At that point DESIGN.md has no in-scope unbuilt items left, which is
worth saying plainly in the implementation-status intro.

## Verification

- `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test -race ./...`.
- `CGO_ENABLED=0 go build ./...` — two new dependencies' worth of risk to
  the static-binary property, if either pulls anything in.
- The four mutations above.
- Manual, against the real APIs, once, by hand: a book with a known ISBN
  and a sparse FB2 book with none, confirming the ISBN path and the
  search fallback each return something plausible. Record what came back
  in the PR — this is the only step whose correctness depends on a third
  party's behaviour matching the docs.
- Manual: `METADATA_PROVIDERS=` and confirm zero outbound requests.
