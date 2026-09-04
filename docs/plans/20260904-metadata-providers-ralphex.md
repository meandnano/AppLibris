# Open Library and Google Books metadata providers

## Overview

`internal/enrich` already has the enrichment queue, the worker, the tiny
`Provider` interface and the `Resolve` merge function, but no real
provider — `cmd/server` constructs the worker with `enrich.New(db, nil)`,
so every job resolves to "nothing missing, no providers". This step writes
the two things that touch a network (`internal/openlibrary`,
`internal/googlebooks`), the three decorators DESIGN.md insists live
outside them (rate limit, cache, retry), the compile-time registry that
maps a configured name to a constructor, and the configuration that lists
providers in order.

It also settles the cover question the queue step deferred: covers **are**
fetched, which means `field_sources.field`'s CHECK constraint has to grow
a `cover` value via a table rebuild. A book with no embedded cover renders
as a dashed "no cover" box, the most visible gap in the library grid — six
filled text fields beside that one hole would read as broken.

The design questions are settled by DESIGN.md, so most of the risk is not
in the shape of the code but in how a provider behaves when the network is
unhelpful. This is the first code in the project that depends on a third
party staying up.

## Context

- Adopted from `docs/plans/2026090305-metadata-providers.md` (this repo's
  house-format plan, step 05 of four). The source file is left untouched.
- Depends on step 04
  (`docs/plans/completed/2026090304-enrichment-queue-and-resolver.md`),
  which shipped `enrichment_jobs`, `internal/enrich`'s worker/resolver and
  `storage.ApplyEnrichedFields`/`FieldSourcesForBook`.
- Blocks step 06 (`docs/plans/2026090306-enrichment-in-the-ui.md`), which
  has nothing to show until real answers arrive.
- `internal/resend` is the local precedent for an HTTP client: its own
  `*http.Client` with an explicit timeout rather than `http.DefaultClient`
  (which has none), and a named constant for each limit it enforces with
  the arithmetic in a comment.
- Migrations run in filename order, one statement per file; the newest is
  `2026090303_create_enrichment_jobs_status_index.sql`, so this step's
  four files take `2026090304`–`2026090307`. The `books.file_path` →
  `book_files` rebuild (`2026083010`–`2026083013`) is the precedent to
  copy for a CHECK-constraint change.
- Out of scope: any UI (step 06); a third provider (the registry makes
  adding one a file plus a map entry, and two already proves ordering and
  merging); persistent caching across restarts (the cache exists to stop a
  burst of jobs hammering the same ISBN, not to be a long-lived store);
  automatic re-enrichment on a schedule (step 06 makes it something a
  person asks for).

## Development Approach

- Testing approach: regular
- Complete each task fully before moving to the next
- Update this plan when scope changes during implementation
- No live network in the test suite, ever — providers are tested against
  `httptest.Server` with captured response bodies in `testdata`

## Testing Strategy

- Unit tests required for every code-changing Task
- Run project tests after each Task before proceeding
- Decorators are tested against a fake `Provider` with no HTTP at all,
  which is the point of them being decorators
- Provider fixtures are real captured response bodies stored as `testdata`
  files; a hand-written approximation tests the parser against
  imagination rather than against the API

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Update plan if implementation deviates from original scope

## Technical Details

### The four network cases, deliberately not collapsed

| Case | Behaviour |
|---|---|
| 200 with no match | Zero `Metadata`, nil error. **Not** an error — this is the common case. |
| 404 | Same as above. A missing record is an answer. |
| 429 / 5xx | Error. The retry decorator may retry; the resolver skips this provider and continues. |
| Timeout / transport error | Error, same handling. |

The first two collapsing into the third is the failure mode to avoid: it
would turn "this book is obscure" into a logged error on most books, and
an error log that fires constantly is one nobody reads.

Every provider gets its own `*http.Client` with an explicit and *short*
timeout (a few seconds), per the `internal/resend` precedent. Enrichment
is a background nicety; a provider that is slow should be skipped, not
waited on.

### Decorators

Three, each wrapping a `Provider` and satisfying `Provider`, so they
compose in any order and the resolver cannot tell they are there:

```go
func WithRateLimit(p Provider, every time.Duration) Provider
func WithCache(p Provider, size int) Provider
func WithRetry(p Provider, attempts int) Provider
```

Composed once at construction:
`WithRateLimit(WithRetry(WithCache(client)))` — cache innermost of the
three wrappers so a cached hit costs neither a rate-limit token nor a
retry budget, rate limit outermost so retries are also paced.

- **Rate limit**: a ticker-gated token honouring `ctx` cancellation while
  waiting. Open Library asks for courtesy limits rather than enforcing
  hard ones; a conservative default in a named constant, with that noted.
- **Cache**: keyed by the lookup arguments, bounded, in-memory. Caches the
  "no match" answer too — otherwise a shelf of obscure books re-asks on
  every run.
- **Retry**: only on the 429/5xx/transport cases, never on a "no match",
  with a backoff and a `ctx` check between attempts. A retry that ignores
  context is a shutdown that hangs.

### Covers

`field_sources.field` is CHECK-constrained to the seven text fields, and
SQLite cannot alter a CHECK constraint in place, so allowing `cover` is
the documented table rebuild — create the replacement, copy the rows, drop
the original, rename — four files under the repo's one-statement-per-file
convention:

```
2026090304_create_field_sources_with_cover.sql
2026090305_copy_field_sources_to_new.sql
2026090306_drop_old_field_sources.sql
2026090307_rename_field_sources.sql
```

Two rules the cover path must follow, both differing from the text fields:

- **A fetched cover goes through `internal/cover.Store`**, exactly like an
  embedded one — resized to ~400px, JPEG, named by the *book's* content
  hash. It must not be stored as a remote URL: the covers directory is
  designed to be disposable and regenerable, and a URL in `cover_path`
  would break both, while making the grid issue a third-party request per
  card.
- **A cover is only fetched when `cover_path` is empty.** The scanner's
  empty-string-means-"looked, found nothing" convention already exists and
  must not be confused with "not yet looked at".

A response body over a few hundred KB is rejected *before* decoding it.
`internal/cover` resizes whatever it is handed, and handing a decoder an
arbitrary remote image is the one place this feature could be made to
allocate without bound.

### Registration and configuration

One file, `internal/enrich/registry.go`:

```go
var providers = map[string]func() Provider{
	"openlibrary": func() Provider { return openlibrary.New() },
	"googlebooks": func() Provider { return googlebooks.New() },
}
```

`METADATA_PROVIDERS` (default `openlibrary,googlebooks`) lists names in
order; `cmd/server` resolves them through the map and passes the slice to
`enrich.New`, replacing today's `nil`. An unknown name **fails startup**
with a message naming it and listing the valid ones — unlike a missing
`RESEND_API_KEY`, which only warns. The difference is intent: an unset key
means "I have not set this up", while a misspelled provider name means "I
asked for something specific and did not get it", and silently running
with fewer providers than requested is the kind of thing nobody notices
for months.

`METADATA_PROVIDERS=` (empty) disables enrichment and is the documented
way to run with no outbound calls at all — worth stating in README beside
the other environment variables, since a LAN-only deployment is exactly
this project's assumed setting.

Google Books allows anonymous use at a low quota and takes an optional
`GOOGLE_BOOKS_API_KEY`: support it, warn (do not fail) when absent, and
keep the key out of logs.

## Implementation Steps

### Task 1: Allow `cover` provenance in `field_sources`

- [x] add the four migrations `2026090304`–`2026090307` rebuilding
      `field_sources` with `cover` accepted by the `field` CHECK, one
      statement per file, following the `2026083010`–`2026083013`
      precedent
- [x] add the `cover` member to `internal/storage`'s `MetadataField` enum
      and its `metadataFields` parse map, keeping `UpdateBookField`'s
      existing rejection of fields it cannot write as a plain column
      correct
- [x] make `ApplyEnrichedFields` able to write `cover_path` with `cover`
      provenance, through the same `fieldIsStillMissingTx` re-check every
      other field goes through
- [x] write tests for the new field: the migration applies to a fresh
      database, existing provenance rows survive the rebuild, and a
      `cover` source round-trips through `FieldSourcesForBook`
- [x] run project tests - must pass before next task

### Task 2: `internal/openlibrary` provider client

- [ ] add `internal/openlibrary` with `New()` returning a client
      satisfying `enrich.Provider` (`Name`, `ByISBN`, `Search`), owning
      its own `*http.Client` with a short named-constant timeout per the
      `internal/resend` precedent
- [ ] parse the response into `enrich.Metadata`, normalising ISBN exactly
      as `internal/epub` stores it so the lookup key round-trips
- [ ] implement the four network cases: 200-no-match and 404 are a zero
      `Metadata` with a nil error; 429/5xx and transport/timeout are
      errors
- [ ] capture real response bodies into `testdata` fixtures
- [ ] write tests against `httptest.Server`: fixture parses to the
      expected `Metadata`, each of the four cases asserts error-or-not,
      ISBN normalisation matches `internal/epub`, and a malformed or
      truncated body is an error rather than a panic
- [ ] run project tests - must pass before next task

### Task 3: `internal/googlebooks` provider client

- [ ] add `internal/googlebooks` mirroring Task 2's shape — `New()`, its
      own timeout-bearing `*http.Client`, `Name`/`ByISBN`/`Search`
- [ ] accept an optional API key, keeping it out of every log line
- [ ] parse the volume payload into `enrich.Metadata` with the same ISBN
      normalisation and the same four-case error contract
- [ ] capture real response bodies into `testdata` fixtures
- [ ] write tests against `httptest.Server` covering the same cases as
      Task 2, plus that a request carries the key when one is configured
      and is still made when it is not
- [ ] run project tests - must pass before next task

### Task 4: Rate-limit, cache and retry decorators

- [ ] add `WithRateLimit(p Provider, every time.Duration) Provider` —
      ticker-gated, honouring `ctx` cancellation while waiting, with a
      conservative default in a named constant noting Open Library's
      courtesy limits
- [ ] add `WithCache(p Provider, size int) Provider` — keyed by the lookup
      arguments, bounded, in-memory, caching the "no match" answer too
- [ ] add `WithRetry(p Provider, attempts int) Provider` — retrying only
      the 429/5xx/transport cases, never a "no match", with a backoff and
      a `ctx` check between attempts
- [ ] write tests against a fake `Provider`, no HTTP: rate limit paces
      calls and returns promptly on a cancelled context; cache serves a
      repeat without a second call, including for "no match"; retry
      retries a 5xx, does not retry a "no match", and stops on
      cancellation
- [ ] run project tests - must pass before next task

### Task 5: Registry, `METADATA_PROVIDERS` and `cmd/server` wiring

- [ ] add `internal/enrich/registry.go` with the compile-time name →
      constructor map for `openlibrary` and `googlebooks`
- [ ] compose each provider once at construction as
      `WithRateLimit(WithRetry(WithCache(client)))`
- [ ] read `METADATA_PROVIDERS` (default `openlibrary,googlebooks`) in
      `cmd/server`, resolve names in order through the map, and pass the
      slice to `enrich.New` in place of today's `nil`
- [ ] fail startup on an unknown provider name with a message naming it
      and listing the valid ones; treat `METADATA_PROVIDERS=` as
      "enrichment disabled" rather than an error
- [ ] read the optional `GOOGLE_BOOKS_API_KEY`, warning rather than
      failing when it is absent
- [ ] write tests for name resolution: order is preserved, an unknown name
      is an error naming it, empty resolves to no providers
- [ ] run project tests - must pass before next task

### Task 6: Cover fetch path

- [ ] extend the provider surface so a fetched cover's bytes reach the
      worker alongside the text fields, keeping "no cover" distinct from
      "no answer"
- [ ] add a size cap constant (a few hundred KB) and reject an oversized
      response body *before* decoding it
- [ ] store a fetched cover through `internal/cover.Store` under the
      book's content hash and record the resulting path — never the remote
      URL — with `cover` provenance
- [ ] only ask for a cover when the book's `cover_path` is empty, keeping
      the scanner's empty-string-means-"looked, found nothing" convention
      intact
- [ ] write tests: a fetched image is resized and stored under the content
      hash rather than saved as a URL, an oversized body is rejected
      before decoding, and a book that already has a cover is never asked
      for one
- [ ] run project tests - must pass before next task

### Task 7: Documentation

- [ ] add `internal/openlibrary` and `internal/googlebooks` bullets to
      CLAUDE.md in the house style — what each reads, the four-case error
      contract, and the fixture-based tests
- [ ] extend CLAUDE.md's `internal/enrich` bullet with the decorators and
      the registry, stating the composition order with its reason
- [ ] note the cover-provenance rebuild and the `cover` field in
      CLAUDE.md's `field_sources` coverage, including that a fetched cover
      is stored, never linked
- [ ] document `METADATA_PROVIDERS` and `GOOGLE_BOOKS_API_KEY` in README
      beside the other environment variables, including that
      `METADATA_PROVIDERS=` is the documented way to make no outbound
      calls at all
- [ ] run project tests - must pass before next task

### Task 8: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented
- [ ] run full project test suite with the race detector
- [ ] run project linter and formatter - all issues must be fixed
- [ ] build with cgo disabled to confirm the static-binary property
      survives the new dependencies
- [ ] mutation check: make a 404 an error → the "no match is not an error"
      test must fail
- [ ] mutation check: remove the cache's negative caching → the
      repeat-call test must fail
- [ ] mutation check: store the remote URL in `cover_path` instead of the
      stored file → the cover test must fail
- [ ] mutation check: accept an unknown provider name at startup → its
      test must fail
- [ ] revert all four mutations and re-run the suite

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- DESIGN.md's implementation-status rows (`Metadata providers, chain` from
  `Not built` to `Built`, `Metadata provenance` from `Partial` to `Built`)
  live only on the `init` branch, which a branch cut from `master` has no
  path to. There is already a backlog item covering exactly this drift:
  `docs/backlog/2026090308-design-md-status-update.md`. Once this step
  lands, DESIGN.md has no in-scope unbuilt items left, which is worth
  saying plainly in that document's implementation-status intro.
- Manual, once, by hand against the real APIs: a book with a known ISBN
  and a sparse FB2 book with none, confirming the ISBN path and the search
  fallback each return something plausible. Record what came back in the
  PR — this is the only part of the step whose correctness depends on a
  third party's behaviour matching its documentation.
- Manual: run with `METADATA_PROVIDERS=` and confirm zero outbound
  requests.
- Move the source plan (`docs/plans/2026090305-metadata-providers.md`)
  into `docs/plans/completed/` per the repo's planning convention.
