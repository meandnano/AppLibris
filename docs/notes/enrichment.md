# Metadata enrichment

Rationale for `internal/enrich`, `internal/openlibrary`, `internal/googlebooks`,
`internal/providers` and the enrichment surface of `internal/service`. The
package map and the invariants that must hold are in CLAUDE.md.

## Sources, in order

A book's metadata comes from its own file first. The OPF package in an EPUB
and the description block in an FB2 give title, authors, language, often an
ISBN and a cover, and many books need nothing further. Providers are the
second source, chained in the order `METADATA_PROVIDERS` lists them, and
enrichment never blocks a book from appearing: the scanner and the index are
the source of truth, and enrichment is a background queue running against
records that already exist.

Enrichment is per book and on request, from the detail page. Nothing
enriches on scan or on a schedule, and there is no library-wide run.

## The resolver and the missing rule

`Resolve(ctx, book, authors, sources, providers)` takes no database and no
clock. Everything about the book's current state is passed in, so the merge
logic is testable against fakes with no real provider anywhere.

A field is worth asking a provider for only when it is **both** empty **and**
not `manual` (`isMissing`, one function with both halves in one place).
Dropping the emptiness half makes re-enrichment overwrite good embedded
metadata with a guess. Dropping the `manual` half refills a field someone
deliberately cleared. An empty value with a `manual` source is a decision,
not missing data, and a resolver inferring provenance from emptiness would
undo it.

Providers are asked in order, each only for what is still missing when its
turn comes. Once the missing set is empty the loop stops without calling the
rest. The test for this asserts the un-called provider is never called, not
merely that its answer goes unused.

The same rule is enforced a second time at the write. `Resolve`'s snapshot
can be minutes stale by the time a provider answers, and `DB.Write`'s single
connection means a concurrent manual edit has already committed. So
`storage.ApplyEnrichedFields` re-reads each field's value and provenance
inside its own transaction and skips any that is no longer missing. Values
travel as `map[storage.MetadataField]string`, authors newline-joined, the
same representation the web layer's author textarea uses, so the join-table
write is storage's job and not this package's. `Resolution.SourceName`
carries each field's own answerer, since field-level merging means one job
can legitimately fill fields from two providers.

## ISBN versus search

A provider is asked by ISBN when the book has one, by title and author
otherwise. An ISBN lookup that comes back a clean no-match means that
catalogue lacks the edition, so the same provider is then asked by title in
the same iteration. An ISBN lookup that *errors* is not followed up: a 5xx
says nothing about whether the ISBN is right, and searching on it would
accept a fuzzy answer because a host was briefly unreachable.

A cover-only reply counts as an answer (`Metadata.IsEmpty` includes
`CoverURL`), so nothing searches past one. A book with no title never
searches at all, since the gate below would reject whatever came back.

A `Search`-sourced answer never fills `isbn`, even after clearing the gate.
An identifier has no partial credit, and it is the lookup key every later
run would use, so a wrong one compounds instead of sitting still. It is
withheld by dropping `isbn` from the missing set as soon as the search path
is taken, which also lets the early stop fire for a book with no ISBN: a
field that can never be filled would otherwise keep the set non-empty and
spend a call on every remaining provider, on every run. The consequence
reads as a bug and is not: enrichment cannot write `isbn` by any route. The
field is only missing for a book that has none, such a book only reaches a
provider through `Search`, and there the value is withheld.

## The plausibility gate

A search endpoint answers with a ranking, and a ranking is not an
identification. The books that reach the search path are the ones with the
least to match on, and their stored title is frequently the filename. So a
`Search`-sourced answer must clear `plausibleMatch` (`match.go`) before any
of it is merged. A `ByISBN` answer never does: an ISBN names one edition, so
the answer is about this book by construction, and gating it would reject
correct data over a differing title string.

The gate lives in the resolver, not in the provider clients, because it is
the one place that knows which path was taken, and because it has to be
testable against fakes.

**Title match is required; author overlap is a veto, never a pass.** Both
providers bind the author into the query, so an author match only confirms
they honoured a constraint this package supplied. "Titles match *or* authors
match" accepts any Stephen King novel for any Stephen King file. Overlap is
consulted only when both sides have authors; an authorless answer is silence
rather than disagreement.

Titles are compared as **delimited segments**. A title is split at
`:;,()[]{}—–/|`, and at `-.?!` only when the separator also carries
whitespace, since each of those has a word-internal meaning: `" - "` is a
dash where `"Twenty-One"` is a compound, `". "` ends a segment where
`"J.R.R."` does not. The period is there for the Russian `"Series. Title"`
convention, and its cost is that an abbreviation splits a title, so `"No"`
matches `"Dr. No"`. Two titles match when their segments agree pairwise, or
when a one-segment title equals a segment of a title with at most
`maxSegments` (2) parts. Each of three cheaper rules gets a real case wrong:

- Substring matching lets "It" match "Italy", so words are compared whole.
- Undelimited containment lets "Dune" match "Dune Messiah": a delimiter is
  the only thing separating a subtitle from a sequel, since "The Hobbit"
  stands behind a `:` in "The Hobbit: 75th Anniversary Edition" and behind
  nothing but a space in "The Hobbit Companion".
- Matching any delimited segment lets "Hamlet" match "Shakespeare: Hamlet,
  Othello, Macbeth". A contents list is not a subtitle, and the segment
  count is what tells them apart.

The author veto rescues none of these: a sequel and a collected edition both
share their author. One-word and series titles are the shapes to test
against, since they are common in exactly the sparse population this path
serves. The matched segment may be either of the two, so a series-prefixed
answer matches on its last part.

A known limit: a two-segment answer whose second part *describes* the book
still matches, so "The Hobbit" accepts "The Hobbit: A Study Guide".
Separating an edition note from a companion volume needs the words' meaning,
not their punctuation.

Text is case-folded with `cases.Fold` rather than `strings.ToLower`, which
maps `Σ` to `σ` unconditionally and never matches natively written Greek
ending in `ς`. It is then NFD with combining marks dropped, so decomposed
text (what macOS filenames produce) equals composed text and diacritics fold
away as `books_fts`'s `remove_diacritics 2` does. Spacing marks (`Mc`/`Me`)
continue a word rather than splitting it, or Indic vowel signs shred a title
into one-letter fragments. One leading English article is optional per
segment, not per title, since a segment is not always at its title's edge.
Neither the folding nor the normalisation can be hand-rolled, which is why
`match.go` is the one file in the package importing outside the standard
library; `golang.org/x/text` is already in the module through
`golang.org/x/image`.

`maxTitleTokens` (64) refuses an absurd title outright. The matcher is
quadratic and runs on a provider's raw title before `sanitizeValue` caps
anything, while a provider client bounds only the whole response.

A rejected answer is a no-match, not a failure: the chain continues with the
missing set intact, no provenance is recorded, no cover is taken, and
`Failed` is unchanged. The rejection is logged at `Debug` with a `reason`
(`title_mismatch` or `author_veto`), since an author veto otherwise shows two
titles that look like a fine match. The accepting `Info` line is emitted
after the merge and only when the answer contributed something, because it
exists to explain a field's value.

Most filename-titled books therefore resolve to nothing, and that is the
intended outcome. An empty field is recoverable; a plausible wrong answer is
written, provenanced under the provider's name, and by the missing rule
never reconsidered.

**A `language` from a search answer is accepted**, though the gate says
nothing about language and a correctly identified volume can carry a wrong
one — a captured Portuguese *O Alquimista* credited to Paulo Coelho, its
language `en`. Withholding it the way `isbn` is withheld would drop the
four correct values measured over the Russian no-ISBN population this path
exists for to avoid that one. The wrong value is a single visible field,
it carries the provider marker the detail page renders for any
provider-supplied value, and correcting it by hand makes it `manual` and
therefore untouchable afterwards. Refusing the cross-language mismatches
it *can* see — a transliterated title is a title mismatch — is the gate
working as designed.

## Sanitising values

Every value a provider supplies passes through `sanitizeValue` before it
reaches the result map: trimmed, capped, and stripped of line breaks for
every field but description. `ApplyEnrichedFields` is a second writer to the
same columns `internal/service`'s `normalizeField` guards for a person's
edit, and it never passes through that function, so this is the only thing
bounding what a remote source can store.

The limits are `internal/storage`'s `Max*` constants — one definition,
below all three writers of those columns: a person's edit in
`internal/service`, a provider's answer here, and what a file had embedded
in it in `internal/scanner`. None of them restates a number, because a value one
writer stores but another's validation would reject is a field the app can
no longer edit: opening the editor and pressing Save unchanged then fails
on a value nobody typed. Author names are sanitised one at a time and
re-joined, since the join character is itself a newline.

A description keeps its line breaks — it is the one field that does — and
is additionally capped at two consecutive newlines: at most one blank line
between paragraphs. `.detail__description` renders those breaks, so what a
provider sends is what a reader sees, including the four blank lines a
scraped blurb arrives with. The cap is `storage.CapBlankLines`, the call
`PlainDescription` also ends on, so the property belongs to the column
rather than to one client — which is where the next provider will need it.
CRLF is folded to LF first, so a Windows-authored description is not left
with a stray carriage return mid-paragraph.

## Covers

A cover is resolved like any other field but kept out of the value map.
`Resolve` returns it separately as `CoverURL`/`CoverSource`, because `Values`
carries only strings that go straight into a column and a cover's path does
not exist until the image has been downloaded and stored, I/O `Resolve`
never performs. Missing-set membership, first-answer-wins and the early stop
all apply, so a book whose `cover_path` is set is never handed a cover URL
and nothing is downloaded for it.

The worker fetches through `FetchCover` on its own `*http.Client`
(`coverFetchTimeout`), since the URL may name a host unrelated to the
provider that answered (Open Library's covers live on a separate domain).
The read is capped at `MaxCoverBytes` (512 KiB) before any decoding, and a
scheme other than `http`/`https` is refused. The client re-applies that check
on every redirect hop and bounds the hop count (`CheckCoverRedirect`),
because a redirect's target is chosen by whichever host answered, so
checking only the first URL guards nothing. The request carries the same
descriptive `User-Agent` both provider clients set: the answering host is
most often Open Library's own, and a throttle there would arrive as an
ordinary fetch failure with nothing naming the cause.

**The fetch refuses private and local addresses**, at dial time, on every
hop: loopback, RFC 1918 and the IPv6 unique-local range, link-local
unicast (where a cloud metadata endpoint lives), multicast, and the
unspecified address (`RefusePrivateAddress`, applied through the cover
client's `net.Dialer.Control`). A cover URL is chosen by whichever host
answered a lookup, and each hop after it by whichever host answered the
one before, so without this the fetch is a blind GET at any address the
deployment can reach — another container on the same Docker network, a
router's admin page, the metadata service. Only image bytes are ever kept,
so the exposure is small; small is not bounded.

At dial rather than on the URL's host, because a URL check catches only
the literal a provider named. `Control` runs once per candidate address
after resolution and immediately before the connect, so a hostname that
resolves to a public address when the URL is inspected and a private one
when the connection is made is caught, and so is a redirect to a bare
private IP, without `CheckCoverRedirect` having to parse it. Resolving by
hand and then dialing the hostname would reopen exactly the window the
check exists to close. A refused dial surfaces as an ordinary fetch
failure, which the worker already tolerates, with the address at Debug so
a deliberate local mirror can be diagnosed.

It is a deny list of the ranges unreachable from the internet rather than
an allow list of public ones: an allow list needs revising every time IANA
assigns a block, and a cover host is an ordinary public server. A host
allowlist was the other candidate and is worse here —
`covers.openlibrary.org` legitimately redirects to archive.org backends
and Google's image hosts vary, so it would refuse real covers to close
what an address check closes precisely.

Every cover test in `internal/enrich` runs an `httptest.Server` on
loopback, which the guard refuses by design, so the `Worker` holds the
predicate as a field and the tests replace it through a helper that only
`_test.go` files can reach. Production has no switch that turns it off.

`MaxCoverBytes` here is deliberately smaller than `cover.MaxCoverBytes`
(8 MiB) and not an alias of it. It is a network bound, and
`internal/googlebooks` chose which cover size to request by measuring
against exactly this figure, so raising it would silently make that choice
wrong. A downloaded cover is under the store's cap by construction.

The image is converted exactly as the scanner converts an embedded one
(`cover.Store`: resized, JPEG, named by the book's content hash, never by
the URL) and the path is folded into `Values` under `storage.FieldCover`.
A fetch or store failure loses only the cover: it is logged and the field
left out, since it must not fail a job whose text fields resolved.

## Job outcomes

`failed` is reserved for the job itself going wrong: the book vanished
between enqueue and claim, a write failed, no provider the run asked could
answer, or a run whose only result was a cover it could not save. A provider
having nothing to say is the ordinary case for most books against most
providers and is a `done` job with an empty field list, which the UI renders
as "Nothing to add". Rendering that as a failure would train people to
distrust a working feature.

**Every provider asked failed** is `failed` with `allProvidersFailedReason`.
`Resolve` returns `Asked` (providers actually called, not configured, since
the early stop routinely skips some) and `Failed`, and the worker's rule is
`Asked > 0 && Failed == Asked`. `Failed == Asked` rather than `Failed > 0`
because one throttled provider beside one that answered cleanly and had
nothing is still a run that learned something. `Asked > 0` keeps the two
honest zero-provider successes (nothing missing, and `METADATA_PROVIDERS=`)
out of it. Without this rule a run in which every provider answered 429 is
stored identically to an honest no-match and shown in the success
treatment, a false statement to the one person who asked.

**A lost cover fails the job only when nothing else was written**
(`coverLostReason`, which names the cover rather than reusing the
all-providers reason, since a provider did answer and the failure is on this
side). A run that resolved text fields and lost its cover stays `done`. For
a book whose only missing field was the cover there are no text fields to
protect, so the tolerance above would report "Nothing to add" for a cover
the run found and dropped. The check sits after the store, because a
successful store has just put `FieldCover` into `Values`.

A step failing while `ctx` is already cancelled is an abandoned attempt, not
a verdict. The worker leaves the row `running` for
`storage.RequeueInterruptedEnrichment` to requeue at startup, the inverse of
`internal/sender`'s recovery. An enrichment job's only effect is writing
fields a pure function computed from data already in the database, so
running it again lands the same values, where a send's effect is not
repeatable. A permanent `failed` row here has no "retry is a new row"
affordance behind it and would leave a book silently never reconsidered.
The all-providers-failed branch guards itself with its own `ctx.Err()` check
for the same reason: during a shutdown every provider fails, which is
indistinguishable from every provider being unable to answer.

**A panic inside `process` is recovered and recorded as `failed`** with
`crashedReason`, the panic value and `debug.Stack()` going to the log at
Error. The row must not stay `running`: `RequeueInterruptedEnrichment`
puts a running job back on the queue at the next start and `Run` drains
immediately, so the same input panics again — under a container restart
policy, a loop with no exit but editing the row by hand. `failed` breaks
that and puts Retry on the page, which is where the decision belongs once
the log has said why. The reason is a sentence rather than the panic's own
text, which carries a Go type name and sometimes a URL or a key fragment
and never anything a person can act on.

Recovered per job rather than in `Run`'s loop, where the job id is no
longer in hand and the row would be left in exactly the state the requeue
turns into the loop. `internal/sender` carries the same guard against a
smaller version of the same shape (`sending.md`).

`enrichment_jobs.book_id` cascades on delete, unlike `send_log.book_id`. A
job is a pending intention about a book and is meaningless once the book is
gone, where a send log entry is the record that a thing happened. The
`bookGoneReason` the worker records exists only for the narrow race where a
claim and the deletion interleave.

## Providers: Open Library

Open Library models a **work** (the book as written) separately from an
**edition** (one publication of it), and the two lookup paths hit different
endpoints because of it. An ISBN names one edition, so `ByISBN` uses the
edition-scoped Read API (`/api/volumes/brief/isbn/{isbn}.json`) and reads
both blocks of its response: `data` is the only place author names appear,
`details.details` the only place language and description do.

`Search` stays on `/search.json`, which answers about works, and therefore
returns **neither language nor publication date**. That endpoint's `language`
is every language any edition was ever published in (31 of them, starting
`bul`, for an English printing of *The Hobbit*), and its date is the work's
first publication. Leaving both empty keeps them missing, so the chain offers
them to the next provider and a person can still fill them by hand. A wrong
value reads as answered and is never reconsidered.

Two response shapes are easy to get wrong. An unknown ISBN is answered with a
bare `[]`, a JSON array where a match is an object, so the body is checked
before unmarshalling: decoding it into the response struct fails with a type
error, and reporting the ordinary no-match as a parse failure would make an
obscure book look like a broken provider. And `description` is either a
`{"type", "value"}` object or a bare string in the same position
(`textValue` tries both).

MARC three-letter language codes are mapped to ISO 639-1 (`marcToISO639`),
listing only the languages this library plausibly contains and passing
anything else through unchanged, so the column does not hold `eng` for one
book and `en` for the next. This does not make the column consistent on its
own: `internal/epub` and `internal/fb2` pass a file's own value through, and
EPUB's `dc:language` is BCP-47 by specification.

Both cover URLs are built by one `coverURL` helper and carry
`?default=false`, so a cover id with no image behind it is a `404` rather
than a `200` and a stand-in. Measured against id 999999999: bare, it
answers `200` and 43 bytes of 1x1 GIF; with the parameter, `404`. Nothing
downstream can tell a stand-in from a cover — `FetchCover` sees a 200 and
some bytes, and whatever survives `cover.Store` is written under this
provider's name and, by the missing rule, never reconsidered — while a 404
is a failed fetch the worker already handles, leaving the book an honest
empty cover and a place in the next provider's missing set. A stale
`cover_i` is ordinary in search results, so this is the common case rather
than a corner. One helper for both call sites, or the parameter goes on one
path and not the other.

The client sets a descriptive `User-Agent`. Open Library's terms ask for one
and throttle the generic Go default, and a block there is indistinguishable
from any other transient failure, so the resolver would silently skip the
provider for every book.

## Providers: Google Books

The list endpoint (`GET /volumes?q=`) names only `smallThumbnail` and
`thumbnail` however large the volume's art is, and `thumbnail` is about
195px on the long edge, under `internal/cover`'s 400px target, which never
upscales. The sizes `small` through `extraLarge` exist only on
`GET /volumes/{id}`, so `enrichVolume` makes a second request for any volume
that matched and named an id, refusing a reply whose own `id` is not that
one (an absent `id` included, since letting `""` pass would make the check
opt-out by the party being checked). It takes the detail response's
description while it is there: the list endpoint's description arrives with
its markup already flattened, paragraph breaks and all, where the detail
endpoint's is fuller text. It fails silently, leaving the list answer's
thumbnail and description as they were, since a lookup holding six good text
fields must not fail over a nicety.

Two costs are priced in. The second request is made before `plausibleMatch`
sees the answer, so a rejected hit has already paid for it, and it is made
whether or not the resolver needs a cover or description, since a provider
has no view of the missing set. `WithRateLimit` gates the method, not the
HTTP call, so one token covers two requests; the worst case for one book is
three requests on two tokens, times `DefaultRetryAttempts` on a 429.

`best()` prefers `medium`, then `large`, then `small`, then `thumbnail`, and
`extraLarge` is absent from the struct. Every size from `medium` (~880px) up
clears the 400px target, so a bigger one only decides how many pixels
`cover.Store` throws away, while `extraLarge` runs 350 to 800 KB against
`enrich.MaxCoverBytes`' 512 KiB ceiling, where a cover past the cap is
refused rather than downsized. Taking the largest link turns a good cover
into no cover. Rewriting the thumbnail URL's `zoom` parameter is the other
shortcut to avoid: for a size a volume lacks, Google answers `200
image/jpeg` with an "image not available" placeholder that nothing
downstream can tell from a cover. Only a URL Google itself named is safe to
fetch. Open Library has the same placeholder in a form it can be asked not
to send, which is what `?default=false` does there.

Status classification is measured rather than read from the docs: a
**rejected key is 400** (`API_KEY_INVALID`), an **exhausted quota is 429**
for both the per-day and per-minute limits, and **403 is the service not
being enabled** for the project. 400 and 403 are configuration and are not
retried; 429, 5xx and transport failures wrap `enrich.ErrRetryable`. A
per-day 429 is therefore retried three times over a quota that will not
clear for hours; Google names the limit in the body and sends no
`Retry-After`, so telling the two apart is possible and left to whatever
revisits `WithRetry`.

`intitle:`/`inauthor:` values are quoted, which is load-bearing: the API
binds a qualifier to the single token after it, so an unquoted multi-word
title constrains only its first word. BCP-47 tags are cut to their primary
subtag (`baseLanguage`), since the API answers `pt-BR` and `zh-CN` and the
primary subtag is already ISO 639-1 in every observed value. That drops
script and variant too, so `zh-Hant` and `zh-Hans` both become `zh`, accepted
because the column is one short code that three other writers fill without
any subtag.

Google's description is HTML on the detail endpoint and is rendered to plain
text through `storage.PlainDescription` before it leaves the package: block
tags become line breaks, inline ones are dropped, entities are unescaped only
afterwards so text that was itself escaped markup survives as the characters
an author wrote. Nothing downstream treats a description as markup, so a tag
left in shows a reader a literal `<p>` and offers it back in the edit
textarea. The derivation sits in `internal/storage` because `internal/epub`
needs the same one for `dc:description`; `storage.md` carries why, and it is
called here rather than in `sanitizeValue` because Open Library's edition
description is plain to begin with and a strip applied to every provider
answers for a source that never sends markup. Paragraph breaks reach the
column and the page alike: `.detail__description` renders with
`white-space: pre-line`.

A detail-request failure leaves the list answer exactly as it was and marks
it `Metadata.Partial`. `WithCache` declines to store a partial answer, so
the next lookup for that key asks again and the second request gets another
chance. Without the mark one transient failure would be remembered as a
complete answer for the life of the process, costing that book its larger
cover and fuller description until a restart. Every failure counts — a
non-200, a malformed body, a body naming another volume — because each
leaves the same two fields unfilled and each might not happen next time.
Nothing else reads `Partial`: `Resolve` counts the answer as answered, so
it lands in `Asked` and not in `Failed`.

The optional `apiKey` travels in the query string and is scrubbed from every
returned error's text (`redactKey`), in raw and percent-encoded form, since
a transport error embeds the full request URL. The redacting error keeps an
`Unwrap`, so `errors.Is(err, context.Canceled)` works whether or not a key is
configured. The same credential is the stronger of the two reasons the
shared redirect policy refuses a hop that **leaves the host the lookup
started against**: net/http sets `Referer` on every hop from the previous
request's full URL, suppressing it only on https→http, so an ordinary
https→https redirect hands `?key=…` to whichever host answered. Moving the
key to a header would not substitute, since Go forwards non-sensitive
headers across hosts. The weaker reason applies to both clients and is
under Shared provider contract.

`GOOGLE_BOOKS_API_KEY` is optional in the sense that startup only warns
without it, but the anonymous quota is shared across every keyless caller
and has been observed exhausted on every attempt. A keyless deployment should
expect this provider to answer 429 and be skipped; it degrades quietly
because Open Library still answers. The key never reaches a log line.

## Shared provider contract

Both clients build their own `*http.Client` with an 8-second `Timeout`,
sized short because enrichment is a background nicety nobody is waiting
on. `ByISBN` normalises its argument exactly as `internal/epub` normalises
a stored ISBN so the lookup key round-trips. Both return their **top hit
unchecked**: judging whether a ranking's first result is the book in hand is
the resolver's `plausibleMatch`.

Both implement the same four-case contract. A 200 with no results and a
defensive 404 are a zero `Metadata` with a nil error, since a missing record
is an answer and the common case. A 429, any 5xx and a transport failure are
errors wrapping `enrich.ErrRetryable`. Folding the first two into the third
would turn "this book is obscure" into a logged error on most books, and an
error log that fires constantly is one nobody reads.

Both carry the same redirect policy, and it is one function rather than a
copy each: `enrich.CheckLookupRedirect`, over `enrich.SameHost`. Two copies
of a check that decides whether a credential leaves the host is not a thing
to let drift. It bounds the hops, checks every hop's scheme rather than
only the first URL's, and refuses a hop that leaves the starting host or
drops off TLS. A same-host downgrade is a clause of its own, because a
`Location` writing the port out on both sides compares equal under
`SameHost`'s default-port normalisation.

`SameHost` compares as a host and not as a string: case-insensitively, with
the scheme's default port and an explicit one treated alike, and a fully
qualified trailing dot ignored. A byte compare admits nothing wrong, but a
refusal is a lookup failure, so a `Location` that merely spells the same
host differently would leave enrichment quietly answering nothing for that
book.

The host clause earns its place twice over, once per client. On Google it
guards the credential above. On Open Library there is none to guard, and
the reason is the other one the clause closes for both: a cross-host hop
makes the client adopt the answering host's whole response, gated by title
and author on the search path and by nothing at all on the ISBN path.
Following redirects is not optional there — an ISBN is frequently an alias
for the canonical edition key — but every hop that API was observed to
issue stays on `openlibrary.org`: the Read API answers ISBNs directly, and
the `/isbn/{isbn}` aliases hop once or twice, same-host each time. The
check costs nothing that was ever seen to work.

**A refused redirect is not retryable**, on either client. Every return in
the policy wraps `enrich.ErrRedirectRefused`, and each lookup path tests
for it before the retryable wrap that would otherwise catch it along with
real transport failures — a refusal arrives as a transport error, since it
comes back from `Do`. The classification follows from the policy being a
pure function of URLs that do not change between attempts: a second attempt
reaches the identical refusal, the same argument already written beside 400
and 403. Retrying one spends three lookups to be told the same thing three
times.

A matched result's cover is **named, not downloaded**: it comes back as
`Metadata.CoverURL` and the fetch is the worker's. Fetching inside the
provider would spend a round trip and up to `MaxCoverBytes` on every lookup,
including the common case of a book whose embedded cover makes the answer
discarded, and would put image bytes into `WithCache`'s map, where 512
entries times two providers is hundreds of megabytes held for the process's
lifetime. A `Metadata` of nothing but strings keeps that cache kilobytes.

## Decorators and registry

The three decorators in `decorator.go` wrap a `Provider` and satisfy
`Provider` themselves, so the resolver cannot tell they are there and each is
tested against a fake with no HTTP. `WithRateLimit` gates `ByISBN`/`Search`
on a shared ticker-fed token (`DefaultRateLimitInterval`, one call a second,
conservative since Open Library's limit is a courtesy ask) and honours `ctx`
while waiting rather than blocking a shutdown. `WithCache` serves a repeat
lookup out of a bounded LRU (`DefaultCacheSize`), caching a no-match too,
since a shelf of obscure books would otherwise re-ask the same negative on
every run; an error is never cached, since the contract treats it as
transient. `WithRetry` retries only `ErrRetryable`, up to
`DefaultRetryAttempts` total with doubling backoff and a `ctx` check between
attempts. A no-match is never retried, since it is an answer.

`internal/providers` is the compile-time name to constructor map. It lives
outside `internal/enrich` because both provider packages import that package
for `Provider` and `Metadata`, so a registry there importing them back would
be a cycle. Each provider is composed once as
`WithCache(WithRetry(WithRateLimit(client)))`, and the order reads
outermost-first because the outermost wrapper is what a call reaches first.
**Cache outermost**, so an answer already in memory spends neither a
rate-limit token nor a retry attempt. **Rate limit innermost**, so every
attempt `WithRetry` makes takes a token of its own. Rate limiting outside
would make a cached hit wait a full interval, and would leave retries paced
only by their backoff, sending a provider that just answered 429 three
requests inside one token.

`METADATA_PROVIDERS` (default `openlibrary,googlebooks`) lists names in
order. An unknown name **fails startup**, naming it and listing the valid
ones, where a missing `RESEND_API_KEY` only warns: an unset key means "not
set up yet", a misspelled provider means "asked for something specific and
did not get it", and running with fewer providers than configured is the
kind of shortfall nobody notices for months. A repeated name is kept once at
its first position, since two chains would mean two caches and two
rate-limit budgets. `METADATA_PROVIDERS=` resolves to an empty, non-nil
slice and is the documented way to disable enrichment and make no outbound
requests; the worker still runs, every job a no-op, the same way an unset
Resend key makes the sender one.

## The service surface

`internal/service` mirrors its send trio: `EnrichBook` enqueues, pokes the
worker through `NotifyEnrichment` and reads the state back rather than
synthesising it, since `EnqueueEnrichment` is idempotent while a job is
queued and what the caller wants either way is the job the book actually
has. `EnrichmentState` and `LatestEnrichment` shape through
`enrichmentStateFrom`, collapsing when-did-this-happen to one `At` field the
same way `sendAt` does. `NotifyEnrichment` is a second function field beside
`Notify` rather than one multiplexed hook: two queues, two workers, and
poking the wrong one leaves a job waiting for its poll tick.

The symmetry with sending stays as two parallel surfaces on purpose. An
abstraction over exactly two cases has no third instance to test its shape
against, and the two differ in precisely the part that would have to be
generic: a send's terminal detail is an address and a failure reason, an
enrichment's is the list of fields it wrote.

`enrichEnabled`, whether any provider resolved, is what the UI receives
rather than a config flag of its own. A control offering to fetch metadata
from nowhere cannot do what it says, so with no providers the enrichment
control renders the same disabled treatment the send control shows without
Resend, and `NotifyEnrichment` is left nil.

## Test fixtures

Every fixture under `internal/googlebooks/testdata` and the two
`edition_*.json` under `internal/openlibrary/testdata` are live captures of
the real APIs, including the three Google error bodies that settle the
status classification above and the same volume captured from both
endpoints. Only `internal/openlibrary`'s `search_*.json` are shaped after
the documented response format. Each `_test.go` names which of its fixtures
is which. Nothing is hand-edited to fit a change: a fixture adjusted until
the code passes tests the parser against its author's expectations rather
than against the API, which is how an edition lookup can be years and a
language wrong with every test green.

## What is deliberately absent

- **Automatic enrichment**, on scan or on a schedule. The first thing a
  person sees should be enrichment they asked for, on a book they chose. It
  is much easier to add than to take back, and it needs a ceiling on how
  many times a book is asked about, which does not exist yet
  (`docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`).
- **A library-wide enrich.** The queue supports it; the missing piece is an
  honest progress display for something that takes hours behind a rate
  limiter.
- **An enrichment history page.** `/history` exists for sends because a send
  is an irreversible outbound act you may need to prove happened. Enrichment
  is repeatable and its result is visible in the fields themselves.
- **Editing provenance.** A source is a fact about where a value came from,
  not a setting.
- **Provenance markers for `embedded` and `manual`.** Every field has a
  source, and rendering all of them would say "embedded" seven times to
  convey nothing. A marker is a caveat, and only a third-party guess changes
  how much to trust a value.
