# Step: make the title/author fallback safe, then use it more

## Position in the sequence

**Second of the enrichment trio.** It depends on step 01 only for
readability — `Resolve`'s signature becomes a struct there, and this step
adds fields to it rather than a sixth and seventh return value. Build it
after.

The two halves inside this step have a strict order of their own, and it
is the whole reason they share a plan: **the guard must land before the
fallback.** The fallback makes the title/author search reachable for a
much larger set of books; shipping it first would widen exactly the path
that has no correctness check on it.

## Context

Two backlog items, both re-validated against the current code:

- `docs/backlog/2026090310-search-fallback-accepts-the-top-hit.md`
- gap 3 of `docs/backlog/2026090402-enrichment-has-no-attempt-ceiling.md`

They were filed separately and each ends by pointing at the other. They
are the same mechanism seen from two sides.

**The guard.** `enrich.Resolve` falls back to `Search(ctx, book.Title,
authors)` when the book has no ISBN. Both providers answer that with their
first result and no similarity test at all — `internal/openlibrary`
returns `c.toMetadata(parsed.Docs[0])`, `internal/googlebooks` returns
`c.toMetadata(parsed.Items[0])`. Whatever the remote ranking puts first
becomes the answer.

The books that reach this path are the ones with the least to match on. A
book with no ISBN is the sparse-metadata case, and
`internal/scanner`'s `filenameTitle` means such a book's stored title is
frequently the filename with its suffix stripped — `Tolkien_Hobbit_ru`,
`[lib.rus.ec] Хоббит`, `01 - Fellowship`. So the query is a filename, and
a provider that returns *anything* for it supplies `publisher`,
`published_date`, `language`, `isbn`, `description` and a cover for a
different book.

The wrong match is then permanent. `ApplyEnrichedFields` records the
provider's name in `field_sources`, so those fields are no longer empty
and `isMissing` never reconsiders them; only a hand edit undoes it. A
wrong `isbn` is the worst of the six, because it is also the lookup key
every subsequent run would use — a fuzzy title match writes an identifier
that then looks authoritative.

Step 06 shipped the button, so this is reachable by a person today. It was
not when the item was filed.

**The fallback.** `Resolve` picks one path per provider and never
reconsiders:

```go
if book.ISBN != "" {
	answer, perr = p.ByISBN(ctx, book.ISBN)
} else {
	answer, perr = p.Search(ctx, book.Title, authors)
}
```

`Search` is reachable only for a book with **no ISBN at all**. A book whose
ISBN is simply absent from a catalogue — common for Russian FB2 editions,
and for anything published outside the two catalogues' coverage — is never
searched by title, even though the title and author are right there. It
comes back "Nothing to add" forever while looking, in the job log, like an
ordinary completed run.

## Scope

In scope, in this order:

1. A confidence gate on any `Search`-sourced answer, applied in the
   resolver.
2. A `Search`-sourced answer may never fill `isbn`.
3. A clean "no match" from `ByISBN` falls back to `Search` **on the same
   provider**, within the same iteration.
4. Logging the matched title alongside the provider name, so a bad match
   is diagnosable from the record.

Out of scope, with reasons:

- **Falling back after an ISBN *error*.** A 429, a 5xx or a timeout is
  not evidence the ISBN is wrong. Falling back there would accept a
  weaker, fuzzy-matched answer *because the network hiccupped*, and
  record it permanently under the provider's name. The provider that
  erred is skipped exactly as it is today, and step 01's accounting is
  what makes that visible.
- **Withholding `published_date` too.** Tempting — a near-miss edition
  year is wrong in a quiet way. But unlike `isbn` it is not self-
  reinforcing: nothing keys off it, a person can see it is wrong on the
  detail page, and the gate is what is supposed to stop near-misses in
  the first place. Withholding a second field would be hedging against
  the gate rather than trusting it. If the gate proves too loose in
  practice, tighten the gate.
- **Scoring or ranking beyond the first result.** Asking a provider for
  five results and picking the best is a genuinely better design and a
  much larger change: both provider clients would need their response
  types widened, and `Metadata` would need to become a slice somewhere.
  The gate below achieves most of the protection — a wrong top hit is
  rejected rather than mis-selected — at a fraction of the surface.
- **Fuzzy string distance (Levenshtein, trigram).** A tunable threshold
  is a number nobody can defend, tested against whichever examples the
  author thought of. The rules below are exact predicates that either
  hold or do not, which is what makes them table-testable.

## Decision 1: the gate lives in the resolver, not in the providers

Both providers would otherwise need their own copy, tested against their
own fixtures, drifting apart. `enrich.Resolve` already owns which fields
to keep from an answer, already has the book's title and authors in hand,
and — decisively — already knows *which path it chose*, which is the one
fact a provider cannot know about itself.

It is also what DESIGN.md's Registration section asks for: "the resolver
logic is kept separate from the providers so ordering and merging are
testable without any real provider." A gate tested against fakes is a gate
whose every branch is reachable in a unit test.

## Decision 2: title match required; author overlap required when both sides have one

```go
// plausibleMatch reports whether answer is plausibly about the same book
// as the one described by title and authors. It gates Search-sourced
// answers only: an ISBN names one edition, so a ByISBN answer needs no
// such test.
func plausibleMatch(title string, authors []string, answer Metadata) bool
```

Two predicates, composed:

- **`titlesMatch`** — normalise both (lowercase; every non-alphanumeric
  rune becomes a separator; collapse and trim), split into tokens, and
  accept when one token slice is a **contiguous run** of the other. Exact
  equality is the degenerate case. Contiguous-run containment is what
  makes `The Hobbit` match `The Hobbit: 75th Anniversary Edition` and
  `Хоббит` match `Хоббит, или Туда и обратно`, while refusing the
  substring accidents a plain `strings.Contains` accepts (`It` inside
  `Italy` is not a token run; `it` inside `italy` is not either, because
  `italy` is one token).
- **`authorsOverlap`** — normalise the same way; accept when any of the
  book's names equals any of the answer's. Applied **only when both sides
  have at least one author**: a book with no authors cannot contradict an
  answer, and an answer with no authors (common on Open Library edition
  records, where authorship belongs to the work) is silence, not
  disagreement.

The composition:

```go
if !titlesMatch(...)          { return false }
if bothHaveAuthors && !overlap { return false }
return true
```

**Author overlap is a veto, never a pass.** This is the part that is easy
to get backwards, and getting it backwards is worse than having no gate.
`Resolve` calls `Search(ctx, book.Title, authors)`, and both providers bind
the author into the query (`inauthor:"…"` for Google). So an author match
merely confirms the provider honoured a constraint we supplied — it says
nothing about *which* of that author's sixty books came back first. A rule
of "titles match **or** authors match" would accept any Stephen King novel
for any Stephen King file.

**The gate will reject most filename-titled books, and that is the
intended outcome, not a shortfall.** A book stored as `01 - Fellowship`
will not match `The Fellowship of the Ring`, and will come back "Nothing
to add". That is the correct answer: nothing available can establish they
are the same book, and getting nothing leaves the fields empty, which
means a person can fill them and a later run can still try. Getting a
plausible-looking wrong answer leaves them filled, provenanced, and never
reconsidered. Empty is recoverable; wrong is not.

A rejected answer is treated as **no match** — the zero `Metadata`, no
error — which the four-case contract already has a shape for, and which
lets the chain continue to the next provider normally.

## Decision 3: a Search-sourced answer never fills `isbn`

Even after passing the gate. Two properties make this field different from
the other five:

- It is the **lookup key for every future run**. A wrong ISBN written from
  a fuzzy title match makes every subsequent enrichment of that book ask
  an edition-scoped endpoint about a different book, and answer
  confidently. The error compounds instead of sitting still.
- It is an **identifier, not a description**. A near-miss publisher is
  approximately right; a near-miss ISBN is not approximately anything.
  There is no partial credit to bank.

Implementation is one line in the merge loop — skip `storage.FieldISBN`
when the answer came via `Search` — and it composes cleanly with Decision 4:
a book that *fell back* to search necessarily already has an ISBN, so
`isbn` is not in its missing set anyway. The rule only ever bites for the
no-ISBN book it was written for, and costs nothing for the other.

## Decision 4: fall back on a clean no-match, on the same provider

Inside the existing loop, replacing the `if/else`:

```go
viaSearch := false
if book.ISBN != "" {
	answer, perr = p.ByISBN(ctx, book.ISBN)
	// A clean no-match means this catalogue does not hold that edition.
	// The title is still worth asking about — subject to the gate. An
	// *error* is not a no-match: it says nothing about the ISBN, and
	// falling back on it would take a fuzzy answer because a host was
	// briefly unreachable.
	if perr == nil && answer.IsEmpty() && book.Title != "" {
		answer, perr = p.Search(ctx, book.Title, authors)
		viaSearch = true
	}
} else if book.Title != "" {
	answer, perr = p.Search(ctx, book.Title, authors)
	viaSearch = true
}
```

Notes on the shape:

- **`Metadata.IsEmpty()`** is a new method: every string field empty and
  no authors. It has to mean "the provider had nothing", not "the
  provider had nothing *we wanted*" — an answer carrying only a
  `CoverURL` is an answer, and searching past it would spend a call to
  replace something real with something guessed.
- **`book.Title != ""`** guards both branches. Searching on an empty
  string asks a provider for its idea of a popular book, and the gate
  would reject whatever came back — so the guard saves a round trip and a
  rate-limit token rather than changing an outcome. A book with no title
  is not reachable through the UI today (title is required on edit) but
  is constructible in storage.
- **Same provider, same iteration.** Open Library's ISBN path reads the
  Read API and its search path reads `/search.json`; asking both of one
  provider before moving on keeps "ask each provider for what is still
  missing, in order" intact.
- **Cost.** Worst case doubles the calls for a book whose ISBN neither
  catalogue knows. `WithRateLimit` paces them at one a second and
  `WithCache` caches negative answers, so a repeat run on the same shelf
  costs nothing. This is a background nicety nobody is waiting on.
- **Step 01's accounting is unaffected.** `Asked` and `Failed` count
  providers, not calls. A provider whose ISBN lookup came back clean and
  whose search then errored counts as one asked, one failed — which is
  true: that provider did not answer.

## Decision 5: log what was matched, not just who answered

The backlog's third bullet, and the cheapest of the five. On accepting a
`Search`-sourced answer:

```go
slog.Info("enrichment matched by search", "provider", p.Name(),
	"book_id", book.ID, "query_title", book.Title, "matched_title", answer.Title)
```

and on rejecting one, the same at `Debug` with the reason. Without this, a
bad match is diagnosable only from the result — someone noticing a wrong
publisher weeks later and having nothing to trace it with. `Info` for the
accept because it is rare (only the search path reaches it) and it is the
line that explains a field's value; `Debug` for the reject because a
gate doing its job on a shelf of filename-titled books would otherwise
produce a warning per book per run, which is the "error log nobody reads"
DESIGN.md's four-case table exists to avoid.

## Changes

- `internal/enrich/match.go` (new): `normalizeForMatch`, `titlesMatch`,
  `authorsOverlap`, `plausibleMatch`. No imports beyond `strings`,
  `unicode` and the package's own `Metadata`.
- `internal/enrich/provider.go` (wherever `Metadata` lives): `IsEmpty`.
- `internal/enrich/resolver.go`: the `viaSearch` flag; the gate applied
  before the merge; `storage.FieldISBN` skipped when `viaSearch`; the
  fallback; the two log lines.
- No provider changes, no storage changes, no schema change, no UI change.

## Tests

`internal/enrich/match_test.go` — table tests, which is what the exact-
predicate design buys:

- `titlesMatch`: equality; case and punctuation differences; subtitle
  extension both directions; Cyrillic; `It` versus `Italy` (must not
  match); one-token versus many; empty either side (must not match).
- `authorsOverlap`: single match; multiple names one match; case and
  spacing differences; no overlap; empty either side.
- `plausibleMatch`: title match with no authors on either side (pass);
  title match with disjoint authors (**reject** — the veto); title match
  with an authorless answer (pass); title mismatch with overlapping
  authors (**reject** — the rule that must not be inverted).

`internal/enrich/resolver_test.go`, against fakes:

- A search answer that fails the gate leaves every field missing and the
  chain continues to the next provider, which is asked for the full
  missing set.
- A search answer that passes the gate fills its fields.
- A search answer carrying an ISBN never writes `isbn`, even for a book
  with none and even when the answer otherwise passes. This is the test
  that pins Decision 3, and it is exactly the one a later "why is this
  field being dropped?" cleanup would delete.
- A `ByISBN` answer *does* write `isbn` — so the skip is scoped to the
  search path and not to the field.
- `ByISBN` clean no-match → `Search` called on the same provider, with the
  book's title and authors.
- `ByISBN` error → `Search` **not** called; that provider is skipped and
  the next one is asked. The negative assertion matters more than the
  positive one here.
- `ByISBN` answers → `Search` never called.
- Empty title → no `Search` call on either branch.
- Step 01's `Asked`/`Failed` still count providers, not calls, across a
  fallback.

## CLAUDE.md

`internal/enrich`'s paragraph needs the gate and the fallback: that a
`Search`-sourced answer passes `plausibleMatch` before merging, that
author overlap is a veto and never a pass and why, that `isbn` is never
written from a search answer, and that a clean ISBN no-match falls back
while an ISBN error does not. The `internal/openlibrary` /
`internal/googlebooks` paragraph should note that both still return their
top hit unchecked and that the check is deliberately the resolver's.

## DESIGN.md (on `init`)

The provider-chain section's four-case table gains a fifth row in prose
rather than in the table: an answer the resolver judges implausible is
treated as case one, "200 with no match" — an answer, not a failure. The
Provider-interface section's note about what the live APIs taught should
record the third lesson: a search endpoint answers with a ranking, and a
ranking is not an identification.

## Verification

With `METADATA_PROVIDERS=openlibrary` (Google Books stays unverified until
a key exists — `docs/backlog/2026090403-…`):

- A book with a real title and no ISBN — press Fetch, confirm the fields
  filled are for the right book, and check the `enrichment matched by
  search` line names a matched title that reads as the same book.
- A book whose title is a filename (`rename` one to `xk92_draft` in a
  scratch library and rescan). Press Fetch: expect "Nothing to add", and
  a `Debug` line showing the rejected candidate. Confirm no field was
  written and `field_sources` gained no row.
- A book with an ISBN neither catalogue holds (an obscure FB2). Press
  Fetch: confirm from the log that `ByISBN` came back clean and `Search`
  followed on the same provider.
- Confirm no book acquires an `isbn` it did not have, by checking
  `field_sources` for an `isbn` row naming a provider after a run over the
  scratch library's no-ISBN books.
