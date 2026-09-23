# Metadata enrichment

Rules for `internal/enrich`, `internal/openlibrary`, `internal/googlebooks`,
`internal/providers` and the enrichment surface of `internal/service`.

## Sources, in order

- **A book's file is the first source; providers are the second, chained in
  the order `METADATA_PROVIDERS` lists them.** The scanner and the index are
  the source of truth.
- **Enrichment never blocks a book from appearing.** It is a background queue
  over records the index already holds.
- **Enrichment is per book and on request from the detail page.** Nothing
  enriches on scan or on a schedule; see the last section.

## The resolver and the missing rule

- **`Resolve` takes no database and no clock; the book's state is passed
  in.** The merge logic is then testable against fakes.
- **`isMissing` is empty and not `manual`, both halves in one function.**
  Dropping the emptiness half overwrites embedded metadata with a guess;
  dropping the `manual` half refills a field someone deliberately cleared.
- **Providers are asked in order, each only for what is still missing, and
  the loop stops once the set is empty without calling the rest.** The test
  asserts the un-called provider is never called, not that its answer goes
  unused.
- **`storage.ApplyEnrichedFields` re-checks `fieldIsStillMissingTx` per
  field inside its own transaction and records only what it wrote.**
  `Resolve`'s snapshot can be minutes stale, and a manual edit has committed
  by then.
- **Values travel as `map[storage.MetadataField]string`, authors
  newline-joined.** That is the author textarea's representation, so the
  join-table write is storage's job.
- **`Resolution.SourceName` carries each field's own answerer.** One job can
  fill fields from two providers.

## ISBN versus search

- **A provider is asked by ISBN when the book has one, by title and author
  otherwise.**
- **An ISBN no-match falls back to `Search` in the same iteration; an ISBN
  error does not.** A no-match means that catalogue lacks the edition. A 5xx
  says nothing about the ISBN, so searching on it accepts a fuzzy answer
  because a host was briefly unreachable.
- **A cover-only reply counts as an answer.** `Metadata.IsEmpty` includes
  `CoverURL`, so nothing searches past one.
- **A book with no title never searches.** The gate would reject whatever
  came back.
- **A `Search` answer never fills `isbn`, so enrichment cannot write `isbn`
  by any route.** An identifier has no partial credit and is the key every
  later run uses. It is withheld by dropping `isbn` from the missing set on
  the search path, which is also what lets the early stop fire for a book
  with no ISBN.

## The plausibility gate

- **A `Search` answer must pass `plausibleMatch` before any of it is merged;
  a `ByISBN` answer never does.** A ranking is not an identification, where
  an ISBN names one edition and gating it would reject correct data over a
  differing title string.
- **The gate lives in the resolver, not in the provider clients.** The
  resolver is the one place that knows which path was taken.
- **Title match is required; author overlap is a veto and never a pass.**
  Both providers bind the author into the query, so an author match only
  confirms a constraint this package supplied. Overlap is consulted only
  when both sides have authors.
- **Titles are compared as delimited segments, never as substrings or
  undelimited runs.** A title is split at `:;,()[]{}—–/|`, and at `-.?!`
  only beside whitespace, since those have word-internal meanings. Titles
  match when their segments agree pairwise, or when a one-segment title
  equals a segment of a title with at most two parts, `maxSegments`, on
  either side. Substrings let "It" match "Italy", undelimited containment
  lets "Dune" match "Dune Messiah", and an unbounded segment count lets
  "Hamlet" match a collected edition; the author veto rescues none of these.
- **A two-segment answer whose second part describes the book still
  matches.** "The Hobbit" accepts "The Hobbit: A Study Guide"; telling an
  edition note from a companion volume needs the words' meaning. A known
  limit.
- **Text is case-folded with `cases.Fold`, not `strings.ToLower`.**
  `ToLower` never matches Greek ending in `ς`.
- **Text is NFD with combining marks dropped; spacing marks continue a
  word.** Decomposed text, as macOS filenames produce, then equals composed
  text, and splitting on Indic vowel signs shreds a title into fragments.
- **One leading English article is optional per segment, not per title.** A
  segment is not always at its title's edge.
- **`match.go` is the one file in the package importing outside the standard
  library.** Neither the folding nor the normalisation can be hand-rolled.
- **`maxTitleTokens`, 64, refuses an absurd title outright.** The matcher is
  quadratic and runs on a raw title before `sanitizeValue` caps anything.
- **A rejected answer is a no-match, not a failure.** The chain continues
  with the missing set intact, no provenance is recorded, no cover is taken,
  and `Failed` is unchanged.
- **A rejection is logged at Debug with a `reason`; the accepting Info line
  is emitted after the merge and only when the answer contributed.** An
  author veto otherwise shows two titles that look like a fine match.
- **Most filename-titled books resolve to nothing, and that is intended.** An
  empty field is recoverable; a plausible wrong answer is never reconsidered.
- **A `language` from a search answer is accepted, not withheld like
  `isbn`.** Withholding it would drop the correct values measured over the
  no-ISBN population to avoid one wrong field that is visible, marked and
  correctable.

## Sanitising values

- **Every provider value passes through `sanitizeValue`, which is
  `storage.CapField` under this package's name; no writer restates a
  number.** A value one writer stores but another's validation rejects is a
  field the app cannot edit. `internal/service` shares `FieldLimit` only,
  since it refuses an over-long edit where the others truncate.
- **Author names are sanitised one at a time and re-joined.** The join
  character is itself a newline.
- **A description keeps its line breaks and is capped at two consecutive
  newlines by `storage.CapBlankLines`, which `CapField` applies.** The same
  cap runs on the way in, through `PlainDescription` in `internal/epub` and
  `annotationText` in `internal/fb2`, so the property belongs to the column.
  A person's edit is not capped: `normalizeField` shapes no further, since
  the blank lines someone typed are their own.
- **CRLF is folded to LF first.** A Windows-authored description otherwise
  keeps a stray carriage return mid-paragraph.
- **`sanitizeValue` does not flatten markup.** Google's description is HTML
  and `internal/googlebooks` flattens it through `storage.PlainDescription`
  before it leaves that package; Open Library's is plain, and a blanket
  strip would answer for a source that never sends markup.

## Covers

- **A cover is resolved like any other field but returned separately as
  `CoverURL`/`CoverSource`, kept out of `Values`.** `Values` carries only
  strings that go straight into a column, and a cover's path does not exist
  until the image is stored, I/O `Resolve` never performs.
- **A book whose `cover_path` is set is never handed a cover URL.**
  Missing-set membership and the early stop apply to the cover too.
- **Providers name a cover URL and never download it; the worker fetches
  through `FetchCover` on its own client, `coverFetchTimeout`.** Fetching
  inside the provider would spend a download on every lookup and put image
  bytes into `WithCache`. The URL may also name a host unrelated to the
  provider.
- **The read is capped at `MaxCoverBytes`, 512 KiB, before any decoding, a
  scheme other than `http`/`https` is refused, and `CheckCoverRedirect`
  re-applies the scheme check and bounds the hop count on every redirect.**
  A redirect's target is chosen by whichever host answered, so checking only
  the first URL guards nothing.
- **The cover request carries the same descriptive `User-Agent` both
  provider clients set.** The answering host is most often Open Library's
  own, and a throttle there arrives as a fetch failure naming no cause.
- **`netguard.RefusePrivateAddress` refuses loopback, private, link-local,
  multicast and unspecified addresses at dial time, through
  `netguard.DialContext`'s `net.Dialer.Control`, on every hop.** Without
  it the fetch is a blind GET at any address the deployment can reach,
  chosen by whichever host answered the hop before.
- **The guard lives in `internal/netguard`, which `importer.Fetcher` dials
  through too.** A check deciding what the server may connect to must
  exist once.
- **The guard runs at dial, not on the URL's host.** `Control` runs per
  candidate address immediately before the connect, so a hostname resolving
  to a private address only at connect time is caught, and so is a redirect
  to a bare private IP.
- **It is a deny list of the ranges unreachable from the internet, not an
  allow list of public ranges or hosts.** An allow list needs revising every
  time IANA assigns a block, and `covers.openlibrary.org` legitimately
  redirects to archive.org backends.
- **A refused dial surfaces as an ordinary fetch failure, with the address at
  Debug.** The worker already tolerates that failure, and a deliberate local
  mirror can be diagnosed.
- **The `Worker` holds the guard as a field that only a `_test.go` helper
  replaces; production has no switch.** The cover tests fetch from an
  `httptest.Server` on loopback, which the guard refuses by design.
- **`enrich.MaxCoverBytes` and `cover.MaxCoverBytes` are separate constants,
  never aliases.** This one is a network bound, and `internal/googlebooks`' cover-size
  choice is calibrated against exactly this figure.
- **The image goes through `cover.Store`, named by the book's content hash,
  and the path is folded into `Values` under `storage.FieldCover`.**
- **A fetch or store failure loses only the cover.** It is logged and the
  field left out; it must not fail a job whose text fields resolved.

## Job outcomes

- **`failed` means the job itself went wrong: the book gone, a write failed,
  `Asked > 0 && Failed == Asked`, a lost cover with nothing else written, or
  a recovered panic. "Nothing to add" is `done`.** A provider having nothing
  to say is the ordinary case, and rendering it as a failure teaches people
  to distrust a working feature.
- **`Asked` counts providers actually called, not configured; the rule is
  `Failed == Asked`, not `Failed > 0`, and `Asked > 0` keeps the
  zero-provider successes out.** One throttled provider beside one that
  answered cleanly is still a run that learned something, where a run in
  which every provider answered 429 must not read as an honest no-match.
- **A lost cover fails the job with `coverLostReason` only when nothing else
  was written, and the check sits after the store.** For a book whose only
  missing field was the cover, tolerating the loss would report "Nothing to
  add" for a cover the run found and dropped.
- **A step failing with `ctx` already cancelled leaves the row `running` for
  `storage.RequeueInterruptedEnrichment`; never `MarkEnrichmentFailed`
  there.** Running the job again lands the same values, where a send's
  effect is not repeatable.
- **Every terminal branch, the all-providers-failed one included, guards
  itself with its own `ctx.Err()` check.** During a shutdown every provider
  fails, which is indistinguishable from every provider being unable to
  answer.
- **A panic inside `process` is recovered and the row written `failed` with
  `crashedReason`; the panic value and stack go to the log at Error, never
  the status box.** A row left `running` is requeued at the next start and
  drained immediately, so the same input panics again in a loop with no
  exit. The panic's own text can carry a URL or a key fragment.
- **The recover is per job, in `process`, not in `Run`'s loop.** In the loop
  the job id is out of hand and the row is left in exactly the state the
  requeue turns into the loop. `internal/sender` carries the same guard
  (`docs/notes/sending.md`).
- **`enrichment_jobs.book_id` cascades on delete.** A job is a pending
  intention about a book and is meaningless once the book is gone;
  `bookGoneReason` exists only for the claim-versus-delete race.

## Providers: Open Library

- **`ByISBN` uses the edition-scoped Read API and reads both `data` and
  `details.details`.** An ISBN names one edition; `data` is the only place
  author names appear, `details.details` the only place language and
  description do.
- **`Search` stays on `/search.json`, which answers about works, and returns
  neither language nor publication date.** That endpoint's `language` is
  every language any edition was published in and its date is the work's
  first; a wrong value reads as answered and is never reconsidered.
- **An unknown ISBN is a bare `[]`, checked before unmarshalling.** Decoding
  it into the response struct fails with a type error, which would make an
  obscure book look like a broken provider.
- **`description` is a `{"type", "value"}` object or a bare string in the
  same position; `textValue` tries both.**
- **MARC language codes are mapped to ISO 639-1 by `marcToISO639`; a code it
  does not list passes through unchanged.** The column otherwise holds `eng`
  for one book and `en` for the next.
- **Both cover URLs come from one `coverURL` helper and carry
  `?default=false`.** Without it a stale `cover_i` answers `200` and a
  placeholder nothing downstream can tell from a cover, stored under this
  provider's name and never reconsidered; with it, a `404` the worker already
  handles.
- **The client sets a descriptive `User-Agent`.** Open Library throttles the
  generic Go default, and a block there is indistinguishable from any other
  transient failure.

## Providers: Google Books

- **`enrichVolume` makes a second request to `/volumes/{id}` for any matched
  volume naming an id, and refuses a reply whose own `id` is not that one,
  an absent `id` included.** The list endpoint's `thumbnail` is about 195px,
  under `internal/cover`'s 400px target, and the larger sizes and the fuller
  description exist only on the detail endpoint. Letting `""` pass makes the
  check opt-out by the party being checked.
- **A detail-request failure marks the answer `Metadata.Partial` and leaves
  the list answer standing; it never fails the lookup.** A lookup holding six
  good text fields must not fail over a nicety.
- **`Metadata.Partial` describes the answer, not the book; only `WithCache`
  reads it, and only to decline storing.** Without the mark one transient
  failure is remembered as a complete answer for the life of the process.
  `Resolve` and `IsEmpty` ignore it, so a partial answer lands in `Asked`.
- **`WithRateLimit` gates the method, not the HTTP call.** One token covers
  both requests, and the second is made before `plausibleMatch` sees the
  answer, so a rejected hit has already paid for it.
- **`best()` prefers `medium`, then `large`, `small`, `thumbnail`, and omits
  `extraLarge`.** Every size from `medium` up clears the 400px target, while
  `extraLarge` runs 350 to 800 KB against `enrich.MaxCoverBytes`, where a
  cover past the cap is refused rather than downsized.
- **Never rewrite a thumbnail URL's `zoom` parameter.** For a size a volume
  lacks, Google answers `200 image/jpeg` with a placeholder nothing
  downstream can tell from a cover.
- **A rejected key is 400, an exhausted quota is 429, a service not enabled
  is 403; 400 and 403 are not retried, while 429, 5xx and transport failures
  wrap `enrich.ErrRetryable`.** These are the observed answers, not the documented ones.
  A per-day 429 is therefore retried over a quota that will not clear for
  hours; Google sends no `Retry-After`, and telling the two apart is left to
  whatever revisits `WithRetry`.
- **`intitle:`/`inauthor:` values are quoted.** The API binds a qualifier to
  the single token after it.
- **BCP-47 tags are cut to their primary subtag by `baseLanguage`.** The API
  answers `pt-BR` and `zh-CN`, and the column is one short code that three
  other writers fill without any subtag.
- **The description is HTML and is flattened through
  `storage.PlainDescription` before it leaves the package.** Nothing
  downstream treats a description as markup, so a tag left in shows a reader
  a literal `<p>`. The derivation lives in `internal/storage` because
  `internal/epub` needs the same one.
- **`apiKey` is scrubbed from every returned error's text by `redactKey`, in
  raw and percent-encoded form, and the redacting error keeps `Unwrap`.** A
  transport error embeds the full request URL, and `errors.Is(err,
  context.Canceled)` must work whether or not a key is configured.
- **The shared redirect policy refuses a hop leaving the starting host; on
  Google that guards the key.** net/http sets `Referer` from the previous
  request's full URL on every hop but https to http, so an ordinary redirect
  hands `?key=` to whichever host answered. A header would not substitute,
  since Go forwards non-sensitive headers across hosts.
- **`GOOGLE_BOOKS_API_KEY` is optional: startup only warns without it, and
  the key never reaches a log line.** The anonymous quota is shared across
  every keyless caller and is routinely exhausted, so a keyless deployment
  sees this provider answer 429 and be skipped.

## Shared provider contract

- **Both clients build their own `*http.Client` with an 8-second
  `Timeout`.** Enrichment is a background nicety nobody is waiting on.
- **`ByISBN` normalises its argument through `storage.NormalizeISBN`.** The
  stored ISBN is normalised the same way, so the lookup key round-trips.
- **Both return their top hit unchecked.** Judging whether a ranking's first
  result is the book in hand is the resolver's `plausibleMatch`.
- **A 200 with no results and a 404 are a zero `Metadata` with a nil error; a
  429, any 5xx and a transport failure wrap `enrich.ErrRetryable`.** A
  missing record is an answer and the common case, and an error log that
  fires on most books is one nobody reads.
- **Both carry one redirect policy, `enrich.CheckLookupRedirect` over
  `enrich.SameHost`, never a copy per package.** It bounds the hops, checks
  every hop's scheme, and refuses a hop that leaves the starting host or
  drops off TLS; a check deciding whether a credential leaves the host is
  not a thing to let drift. The same-host downgrade is a clause of its own
  because a `Location` writing the port out compares equal under
  `SameHost`'s port normalisation.
- **`SameHost` compares as a host, not a string: case-insensitively, with the
  scheme's default port and an explicit one alike, and a trailing dot
  ignored.** A refusal is a lookup failure, so a `Location` that merely
  spells the same host differently would leave enrichment answering nothing.
- **The host clause holds on Open Library too, where there is no key.** A
  cross-host hop makes the client adopt the answering host's whole response,
  gated by nothing at all on the ISBN path. Redirects are still followed,
  since an ISBN is often an alias for the canonical edition key, and every
  hop that API issues stays on `openlibrary.org`.
- **A refused redirect is not retryable: every return in the policy wraps
  `enrich.ErrRedirectRefused`, and each client tests for it before its
  retryable wrap.** A refusal comes back from `Do` as a transport error, and
  the policy is a pure function of URLs that do not change between attempts.

## Decorators and registry

- **The three decorators in `decorator.go` satisfy `Provider` themselves.**
  The resolver cannot tell they are there, and each is tested against a fake
  with no HTTP.
- **`WithRateLimit` spaces `ByISBN`/`Search` calls at least
  `DefaultRateLimitInterval` apart across both methods, honours `ctx` while
  waiting, hands back a slot a caller abandons, and runs nothing between
  calls.** Nothing that builds a limiter ever stops one.
- **`WithCache` is a bounded LRU, `DefaultCacheSize`, that caches a no-match
  and never an error.** A shelf of obscure books would otherwise re-ask the
  same negative on every run; an error is transient by contract.
- **`WithRetry` retries only `ErrRetryable`, up to `DefaultRetryAttempts`
  total with doubling backoff and a `ctx` check between attempts.** A
  no-match is an answer and is never retried.
- **`internal/providers` lives outside `internal/enrich`.** Both provider
  packages import `internal/enrich`, so a registry there importing them
  back is a cycle.
- **Each provider is composed as `WithCache(WithRetry(WithRateLimit(p)))`:
  cache outermost, rate limit innermost.** A cached answer then spends
  neither a token nor a retry attempt, and every attempt `WithRetry` makes
  takes a token of its own rather than sending a provider that just answered
  429 three requests inside one token.
- **An unknown `METADATA_PROVIDERS` name fails startup, naming it and
  listing the valid ones.** Running with fewer providers than configured is
  a shortfall nobody notices for months; an unset `RESEND_API_KEY` only
  warns because it means "not set up yet".
- **A repeated name is kept once, at its first position.** Two chains would
  mean two caches and two rate-limit budgets.
- **`METADATA_PROVIDERS=` resolves to an empty, non-nil slice and disables
  enrichment, and the enrichment worker then makes no outbound requests.**
  The worker still runs with every
  job a no-op, the way an unset Resend key makes the sender one.

## The service surface

- **`EnrichBook` enqueues, pokes the worker through `NotifyEnrichment`, and
  reads the state back rather than synthesising it.** `EnqueueEnrichment` is
  idempotent while a job is queued, and the caller wants the job the book
  actually has either way.
- **`EnrichmentState` and `LatestEnrichment` shape through
  `enrichmentStateFrom`, collapsing time to one `At` field as `sendAt`
  does.**
- **`NotifyEnrichment` is a second function field beside `Notify`, not one
  multiplexed hook.** Poking the wrong worker leaves a job waiting for its
  poll tick.
- **Sending and enrichment stay two parallel surfaces; do not abstract over
  exactly two cases.** They differ in precisely the part that would have to
  be generic: a send's terminal detail is an address and a reason, an
  enrichment's is the list of fields it wrote.
- **`enrichEnabled` is whether any provider resolved, not a config flag of
  its own; without one the control renders disabled and `NotifyEnrichment`
  is nil.** A control offering to fetch metadata from nowhere cannot do what
  it says.

## Test fixtures

- **Every fixture is a live capture except `internal/openlibrary`'s
  `search_*.json`, and each test file says which; never hand-edit a fixture
  to make a test pass.** A fixture adjusted until the code passes tests the
  parser against its author's expectations rather than the API.

## What is deliberately absent

- **Automatic enrichment, on scan or on a schedule.** It is easier to add
  than to take back, and it needs a ceiling on how many times a book is asked
  about, which does not exist
  (`docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`).
- **A library-wide enrich.** The missing piece is an honest progress display
  for something that takes hours behind a rate limiter.
- **An enrichment history page.** A send is an irreversible outbound act you
  may need to prove happened; enrichment is repeatable and its result is
  visible in the fields.
- **Editing provenance.** A source is a fact about where a value came from,
  not a setting.
- **Provenance markers for `embedded` and `manual`.** A marker is a caveat,
  and only a third-party guess changes how much to trust a value.
