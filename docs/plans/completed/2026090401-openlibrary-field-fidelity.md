# Step: Correct Open Library's work-versus-edition field fidelity

## Context

`2026090305-metadata-providers`' own Verification section called for one
manual check against the real APIs — "the only step whose correctness
depends on a third party's behaviour matching the docs" — and it earned
its place: the payloads `openlibrary.org/search.json` actually returns
disagree with what `internal/openlibrary`'s `toMetadata` assumes about two
of the seven fields.

The check was run by fetching the live payloads and replaying them through
the real client code against an `httptest.Server`. Both requests answered
`200`. What came back, for ISBN `9780547928227` (an English Houghton
Mifflin edition of *The Hobbit*) and for a title+author search for
*Roadside Picnic*:

```
ByISBN 9780547928227 -> Title:"The Hobbit" Authors:["J.R.R. Tolkien"]
                        PublishedDate:"1937" Language:"bul"
Search "Roadside Picnic"/"Arkady Strugatsky"
                     -> Title:"Roadside Picnic" PublishedDate:"1978"
                        Language:"pt"
                        Authors:["Аркадий Стругацкий" "Борис Стругацкий"
                                 "Arkady and Boris Strugatsky"]
```

Two of those values are wrong, and both are wrong in the way that sticks:
`enrich.Resolve`'s `isMissing` asks a provider for a field only when it is
empty and not `manual`, so a field filled with a wrong provider answer is
never reconsidered. There is also no UI yet that would show it — nothing
in production enqueues an enrichment job until step 06 — so these would
land silently the moment that step ships, which is why this is a plan
rather than a backlog item.

### Defect 1: `language` is read off a work-level array

`toMetadata` does:

```go
if len(doc.Language) > 0 {
	m.Language = isoLanguage(doc.Language[0])
}
```

`search.json`'s `language` is a **work**-level field: every language any
edition of the work was published in, in no documented order. The live
response for that English-edition ISBN begins:

```json
"language": ["bul", "dut", "tur", "por", "rus", "pol", "rum", "spa", ...]
```

— 31 entries. So `doc.Language[0]` is `bul`, and the answer for an English
book is Bulgarian. Searching by ISBN does not narrow it: the ISBN selects
the *document*, and the document still describes the work.

The defect compounds: `bul` is not in `marcToISO639`, whose comment says
it lists "only the languages this library plausibly contains" and passes
anything else "through unchanged, which is still better than a wrong
guess". That reasoning is sound for an unmapped code, but here the code is
*also* the wrong code, so `books.language` gets the literal string `bul` —
neither ISO 639-1 nor right. The `pt` in the search result is the same bug
wearing a mapped code, and is exactly as wrong (*Roadside Picnic* is
Russian).

### Defect 2: `published_date` is the work's first publication

`toMetadata` prefers `publish_date` and falls back to
`first_publish_year`:

```go
case len(doc.PublishDate) > 0:
	m.PublishedDate = doc.PublishDate[0]
case doc.FirstPublishYear > 0:
	m.PublishedDate = fmt.Sprintf("%d", doc.FirstPublishYear)
```

The ordering is right and the fallback never gets skipped, because
`search.json` **does not return `publish_date` in its default field
set** — it was absent from both live documents. So the fallback is not a
fallback, it is the only branch that ever runs, and every Open Library
answer carries the work's first publication year: `1937` for an edition
published in 2012.

`books.published_date` means when *this edition* was published. That is not
an inference — `internal/fb2` deliberately prefers `publish-info/year`
(this edition) over `title-info/date` (when the work was written) for
precisely this distinction, and CLAUDE.md records the reason. Open Library
is currently answering the question FB2 was careful not to answer.

### Not in scope, deliberately

- **Google Books could not be checked.** Both requests returned `429`,
  `"Quota exceeded for quota metric 'Queries' and limit 'Queries per day'"`
  for the anonymous consumer project — no key was configured. The client
  handles it correctly (429 wraps `enrich.ErrRetryable`, the resolver
  skips the provider), so there is nothing to fix, but it means
  `internal/googlebooks` still has no live confirmation and a keyless
  deployment can be blocked for a whole day. Re-run that half of the
  manual check with `GOOGLE_BOOKS_API_KEY` set; if it turns up field
  defects of its own they belong in their own plan, not folded in here.
- **The mixed-script author list**
  (`["Аркадий Стругацкий", "Борис Стругацкий", "Arkady and Boris
  Strugatsky"]` — three entries for two authors, one of them a combined
  string) is real but is the same weakness
  `docs/backlog/2026090310-search-fallback-accepts-the-top-hit.md` already
  records: the search fallback takes a provider's top hit with no
  similarity test. Fixing author fidelity without fixing hit selection
  would be polishing the wrong end, so it stays with that item.
- **A per-day-quota retry.** `WithRetry` spends its three attempts on a
  429 that will not clear for hours. It is harmless — paced by the rate
  limiter, and the resolver skips the provider either way — and
  distinguishing an exhausted daily quota from ordinary throttling means
  reading `Retry-After` or parsing Google's error body, which is a real
  design decision and not this plan's.

## Decision: the ISBN path uses the edition endpoint, not search

Both defects have the same root: `search.json` describes a **work**, and
the client reads its work-level fields as though they described the
edition the ISBN names. Two candidate fixes were checked against the live
API before choosing, because the first one does not work:

- **`search.json` with an explicit `fields` projection** — accepted
  (`200`, and the response carries exactly the requested keys), but the
  `editions` sub-selection it would need comes back
  `"editions": {"numFound": 0}` for an `isbn=` query. The projection
  cannot reach edition data on this query shape, so it fixes nothing.
- **`GET /isbn/{isbn}.json`** — the edition record, and the obvious
  endpoint for a lookup keyed by an edition identifier. It carries
  `languages`, `publish_date`, `publishers` and `description`, so it does
  fix both defects. Two things ruled it out anyway, both found by asking
  it rather than reading about it: it lists **no authors at all** for many
  editions (The Hobbit included — authorship belongs to the work, and the
  record holds only a `works` reference), which would trade two wrong
  fields for one missing one; and for an ISBN it does not know it answers
  **302**, so a redirect-following client lands on an HTML page and a
  routine no-match becomes a parse error.
- **`GET /api/volumes/brief/isbn/{isbn}.json`** — the Read API, which
  returns *both* halves in one response and is what this plan builds on:

  ```
  records["/books/OL33891995M"]
    .data            -> title, authors:[{name:"J.R.R. Tolkien"}],
                        publish_date:"2012", publishers:[{name:"Mariner Books"}]
    .details.details -> languages:[{key:"/languages/eng"}],
                        description:{type,value}, covers:[12003329],
                        publish_date:"2012", isbn_13, title
  ```

**So `ByISBN` moves to the Read API.** That fixes both defects at the
source — `eng` maps through the existing `marcToISO639` to `en`, and
`publish_date` is the edition's `2012` — and it fixes more than the two:
`search.json` returned **no publisher and no description at all** for this
ISBN, while this record carries both. The ISBN path has been leaving two
of the seven fields empty that were available for the asking, which no
test could have caught, since a fixture captured from `search.json` cannot
show what a different endpoint would have said.

Both blocks are read and neither is redundant: `data` is the only place
author **names** appear, `details.details` the only place the language and
description do. That is also why the plainer edition endpoint above is not
enough on its own.

Four details the implementation has to get right, all four verified
against the live API rather than assumed:

- `languages` is a list of `{"key": "/languages/eng"}` references, so the
  MARC code needs extracting from the key path. It is still MARC, which
  is what `marcToISO639` already expects — the map stays as it is, and
  its pass-through-unmapped behaviour is correct now that the code being
  mapped is the edition's own.
- `description` is `{"type": …, "value": …}` here, but Open Library's data
  model also permits a bare string in that position. Handle both; a decode
  that expects only the object shape silently drops every string-shaped
  description.
- **An ISBN the Read API does not know is answered `200` with a bare
  `[]`** — a JSON *array*, where a match is an object. Unmarshalling that
  into the response struct fails with a type error, so the body must be
  checked before decoding: reporting the ordinary no-match as a parse
  failure is exactly the error-log-nobody-reads failure the four-case
  contract exists to prevent.
- Redirects are followed under a bounded, every-hop-scheme-checked policy
  mirroring `CheckCoverRedirect`, not net/http's default — setting
  `CheckRedirect` at all replaces the standard library's own hop limit, so
  a policy that checked only the scheme would follow a chain forever.

**`Search` keeps using `search.json`**, because there is no edition to
address when the book has no ISBN. But it stops answering the two fields
it cannot answer: where the only available value is work-level,
`Language` and `PublishedDate` are **left empty**. An empty field is one
`isMissing` offers to the next provider in the chain and that a person can
still fill by hand; a wrong field is neither, since it reads as answered.
This is the rule the project already holds its file parsers to —
`internal/epub` never substitutes a `creation` or `modification` date for
a publication one, and `internal/fb2` prefers `publish-info/year` over
`title-info/date` — and a provider is not entitled to a looser standard
than a file parser.

## Changes

- `internal/openlibrary`: `ByISBN` fetches
  `/api/volumes/brief/isbn/{isbn}.json` and parses both blocks of the
  record — author names from `data`, and `title`, `languages`
  (dereferenced and mapped), `publish_date`, `publishers`, `description`
  (both shapes) and `covers` from `details.details`. A bare `[]` body is a
  no-match. Redirects followed under a bounded, scheme-checked policy.
- `internal/openlibrary`: `Search` continues on `search.json` but no
  longer emits `Language` or `PublishedDate` from work-level fields.
- `internal/openlibrary`: `testdata/` gains two **live-captured** Read API
  fixtures — `edition_match.json` and `edition_no_match.json` (the bare
  `[]`). `search_match.json` stays as the search-path fixture; it was
  never a capture (this package was written without outbound network
  access, as its own test-file comment says), so the comment is corrected
  to name which fixtures are which rather than describing all of them as
  approximations. Neither is hand-edited to fit the change — the plan that
  introduced them is explicit that an approximation "tests the parser
  against your imagination rather than the API", which applies with more
  force to a fixture edited until the code passes.
- CLAUDE.md: the `internal/openlibrary` bullet gains the work-versus-
  edition split (which endpoint each path uses and why), the "empty beats
  wrong" rule, the two shapes `description` arrives in, the bare-`[]`
  no-match, and the redirect policy. Its fixture sentence is corrected to
  distinguish the live captures from the documented-shape ones.

## Tests

Against `httptest.Server`, no live network, as the existing suite does:

- The captured edition fixture parses to `Language: "en"` (from
  `/languages/eng`) and `PublishedDate: "2012"` — the regression test for
  this whole plan, and the one that would have caught `bul` and `1937`.
- The same fixture yields the publisher and the description, the two
  fields the search-based ISBN path was silently dropping.
- `description` parses from both the `{"type", "value"}` object and a bare
  string, neither shape panicking on the other's test.
- The bare `[]` body is a no-match with a nil error, not a parse failure.
- Author names come from the `data` block, and fall back to the edition
  record's own when it carries named authors (a shape observed live).
- `ByISBN` requests the Read API path, so an edit that quietly moves it
  back to `search.json` fails here rather than in production.
- A redirect from the ISBN alias to the canonical edition key is followed,
  a redirect to a non-http(s) scheme is refused, and an endless chain is
  cut off.
- The search path yields an empty `Language` and an empty `PublishedDate`
  from a fixture whose only such values are work-level, while still
  yielding title and authors.
- The four network cases still hold on both paths — a 404 from the edition
  endpoint (an ISBN Open Library does not know) is a "no match", nil
  error, not a failure.

**Mutation checks:**

1. Point `ByISBN` back at `search.json` → the edition-fixture test fails.
2. Take `language[0]` from the work array in the search path → the
   empty-language test fails.
3. Fall back to `first_publish_year` in the search path → the empty-date
   test fails.
4. Handle only the object shape of `description` → the bare-string test
   fails.

## Verification

- `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test -race ./...`.
- The four mutations above.
- Manual, once, against the real API: re-run the ISBN and the search
  lookup and confirm the English edition reports `en` or nothing — never
  `bul` — and a year consistent with the edition or nothing. Record what
  came back.
- Note for whoever runs that: Go's HTTPS requests did not complete in the
  environment this defect was found in, while `curl` to the same URL
  worked. Fetching the payload with `curl` and replaying it through the
  client against an `httptest.Server` exercises the parser faithfully and
  is a valid substitute; it does not exercise the client's own transport,
  so say which of the two was done.
