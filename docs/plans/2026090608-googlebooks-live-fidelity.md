# Step: correct what the live Google Books API actually answers

## Position in the sequence

The enrichment trio this was drafted alongside has since shipped
(`2026090601`–`2026090603`, all in `docs/plans/completed/`), and the plan
was re-validated against that code rather than against the branch point.
Nothing in `internal/googlebooks` changed there — the captures and the
tests below apply unaltered — but three of those changes bear on the work
and are picked up where they land:

- `Metadata.IsEmpty()` and the per-provider ISBN→search fallback
  (`2026090601`, `2026090602`) change how many requests a lookup costs,
  which work item 2 has to account for.
- `plausibleMatch` (`2026090602`) is the gate the wrong-hit findings used
  to be routed to. It has shipped, and the live check was run against it:
  it holds up, including on the Russian editions the fallback exists for.
  See *Not a defect 5*.
- `ClearProviderCover` (`2026090603`) makes a lost provider cover
  re-fetchable, which changes Defect 2 from a one-time low-resolution
  store into a recurring one.

## Context

`docs/backlog/2026090403-googlebooks-has-no-live-verification.md`,
re-validated and then **run**: a `GOOGLE_BOOKS_API_KEY` now exists, and
the check that item was blocked on has been carried out. The item is
deleted in this change.

The check was run twice over, both ways the precedent
(`docs/plans/completed/2026090401-openlibrary-field-fidelity.md`) allows,
because this environment permits what that one did not:

- **`curl` against the live API**, capturing payloads that are now the
  package's fixtures. ~130 requests against
  `https://www.googleapis.com/books/v1/volumes` on 2026-09-06.
- **The real `*Client` against the real API**, over its own
  `*http.Client` — Go's HTTPS requests complete here, unlike in
  `2026090401`'s environment, so the transport was exercised too and not
  only the parser. That probe was a throwaway; it is not committed, since
  no test in this repo reaches the network and this step does not
  introduce the first one.

### What is right, and is now pinned by a capture rather than by a reading

Six of the seven claims the backlog listed came back correct. They are
recorded because "verified" is a different state from "unexamined", and
the tests that hold them now replay live bytes:

- **`volumeInfo.publishedDate` is the edition's.** ISBN `9780547928227`
  answers `"2012"` — the Mariner edition, not the work's 1937. This is
  the field Open Library got 75 years wrong, and the Volumes API returns
  one volume per edition, so it does not have that failure mode. Pinned
  in `TestByISBNMatchParsesFixture`.
- **`volumeInfo.language` is the edition's** — `en` for that English
  edition, where Open Library's work-level array began `bul`. Its
  *format* is a separate matter; see Defect 1.
- **The `intitle:`/`inauthor:` quoting binds the whole phrase.**
  `intitle:"Zorbaks Lament of the Quantum Ferret"` answers 0 results,
  and `intitle:"The Peregrine" inauthor:"J. A. Baker"` answers the right
  book — a quoted qualifier is a phrase filter, not a soft ranking hint.
- **The no-match shape is what the code assumes**: `200`, `totalItems: 0`,
  no `items` key at all. There is no Open-Library-style bare `[]` here.
- **The four-case network contract holds**, and the fixtures now carry
  the three real failures — see Defect 3 for the reasoning behind it,
  which does not.
- **The key is sent, works, and stays out of the error text.** With no
  key every request answers `429` (the shared anonymous consumer project's
  per-day quota is exhausted and evidently stays that way); with the key,
  `200`.

Two smaller things also came back clean and are pinned:
`industryIdentifiers` carries `OTHER`-typed entries (9 in a 20-volume
scan) that `bestISBN` correctly ignores, and the cover URL the provider
names is a direct `200 image/jpeg` with no redirect, so
`enrich.CheckCoverRedirect` has nothing to do on this host.

### Defect 1: `language` is BCP-47, not ISO 639-1

CLAUDE.md states that Open Library's MARC codes are mapped "to the ISO
639-1 form `internal/epub`, `internal/fb2` and Google Books all produce,
so the column doesn't hold `eng` for one book and `en` for the next."

Google Books does not all-produce that form. Across 188 live volumes:

```
en 62, pt-BR 50, zh-CN 32, ru 22, sv 11, ja 3, es 2,
de 1, fr 1, gl 1, id 1, da 1, eo 1
```

`pt-BR` and `zh-CN` are BCP-47 tags with a region subtag — 82 of 188
volumes, not a curiosity. So `books.language` holds `pt` for a book
`internal/epub`, `internal/fb2` or `internal/openlibrary` answered and
`pt-BR` for the next one Google did. That is precisely the
one-column-two-vocabularies split `marcToISO639` exists to prevent, and
it is worse than an unmapped code: `eng` at least looks foreign, while
`pt-BR` looks deliberate.

It sticks the same way every other provider answer sticks —
`enrich.Resolve`'s `isMissing` asks for a field only when it is empty and
not `manual`, so a filled one is never reconsidered.

Pinned as-is (not as it should be) by
`TestRegionalLanguageTagReachesMetadataUnchanged`, against
`volumes_regional_language.json`.

### Defect 2: every Google cover is the 128px thumbnail

`imageLinks.best()` walks `extraLarge → large → medium → small →
thumbnail`. On the endpoint this client calls, the four above `thumbnail`
**never appear**. Captured for one volume, both ways:

```
GET /volumes?q=isbn:9780547928227   -> imageLinks: smallThumbnail, thumbnail
GET /volumes/M1t9BgAAQBAJ           -> imageLinks: smallThumbnail, thumbnail,
                                                   small, medium, large
```

Same volume id `M1t9BgAAQBAJ` in the second pair, so this is a property
of the *endpoint*, not of the volume. Across every list response
captured — 188 volumes, several queries, `filter=ebooks` included — the
only two keys ever present were `smallThumbnail` and `thumbnail`.

So `best()`'s ladder is unreachable as the client is written, and the
answer is always `thumbnail`, which fetches at **128×192**.
`internal/cover.Store` targets 400px on the long edge and never upscales,
so it stores 128×192 and the grid and detail page show it at that size.
`internal/openlibrary` asks for `-L.jpg` and gets a proper one, which
makes this provider the outlier — and worse, the gap only bites in the
case where Google is the *only* answer, since first-answer-wins means a
Google cover is reached only when Open Library had none.

**The obvious shortcut does not work and must not be taken.** The
thumbnail URL carries a `zoom=1` parameter, and rewriting it to `zoom=2`
or `zoom=3` does return a larger JPEG — but for a volume that has no such
size, Google answers **`200 image/jpeg` with an "image not available"
placeholder**, verified by fetching and looking at it (`zoom=2` →
300×391 placeholder, `zoom=3` → 575×750 placeholder, for the very volume
whose `zoom=1` is a real cover). Nothing in `enrich.FetchCover` or
`cover.Store` can tell that from a cover, so the library would fill with
grey "image not available" thumbnails that read as successfully enriched.
A documented `large` link, by contrast, fetched real 800×1199 cover art.

`2026090603` sharpened the cost. A provider cover whose file is gone is
now *forgotten* (`ClearProviderCover`) rather than left dangling, so the
book becomes enrichable again and the next run re-fetches — the same
128×192 thumbnail, every time `COVERS_DIR` is lost. What was one
low-resolution store is now the durable answer for every book with no
embedded cover, rebuilt on demand at that resolution.

### Defect 3: the retry classification's stated premise is wrong

`googlebooks.go` reasons, and CLAUDE.md repeats:

```go
// A 403 is Google's over-quota and rejected-key answer as well as
// its forbidden one, and none of the three is helped by asking
// twice — only 429 and 5xx are.
```

Every clause of that is wrong except the conclusion. Provoked and
captured:

| condition | status | `errors[].reason` / `details[].reason` |
|---|---|---|
| malformed key | **400** | `badRequest` / `API_KEY_INVALID` |
| per-day quota exhausted (anonymous) | **429** | `rateLimitExceeded` / `RATE_LIMIT_EXCEEDED`, `quota_limit: defaultPerDayPerProject` |
| per-minute throttle (valid key, 120 concurrent) | **429** | `rateLimitExceeded` / `RATE_LIMIT_EXCEEDED`, `'Queries per minute per user'` |
| service not enabled for the project | **403** | `accessNotConfigured` / `SERVICE_DISABLED` |

A rejected key is 400, not 403. Over-quota is 429, not 403. The one thing
403 really is — a configuration failure — is the one case the comment
lists last and treats as incidental.

The **behaviour** is right anyway, by luck rather than by the stated
reasoning: 400 and 403 both fall to the non-retryable branch, and neither
is helped by asking twice. Only the reasoning has to change. But a
comment whose premise is false is how the next person derives the next
wrong thing from it, and this one is quoted verbatim in CLAUDE.md.

### Defect 4: `description` is not HTML on this endpoint

`plainText` exists because "the Volumes API documents
`volumeInfo.description` as HTML-formatted". The documentation is
describing the single-volume endpoint. Same volume, both endpoints:

```
GET /volumes?q=…            "Based on a comparison of early editions, …"
GET /volumes/M1t9BgAAQBAJ   "<p>Based on a comparison of early editions, …<br><br>…</p>"
```

Across 188 live list responses, **zero** descriptions contained a tag and
zero contained an HTML entity. So `plainText` is a no-op on every answer
this client actually receives — dead code kept alive by a docs reading.

It is not truncation: the two texts are the same length to within three
characters. The one real loss is structure — the list endpoint flattens
`<br><br>` to a single space, so a description that had paragraphs
arrives as one block, where `plainText` over the detail endpoint's markup
would produce the `\n\n` it is written to produce.

`plainText` should **stay**. It is correct, tested, and the moment
anything reads the single-volume endpoint (Defect 2's fix does) it stops
being dead. Only the claim about when it fires needs correcting.

### Not a defect 5: a provider's own `language` can be wrong

Drafted as a defect — "the shipped gate accepts a wrong-language
edition" — and demoted after measuring it. The first reading was wrong
and the correction is the useful part, so it is kept rather than deleted.

What prompted it: `intitle:"O Alquimista" inauthor:"Paulo Coelho"`
answers a volume titled in Portuguese, credited to Paulo Coelho, and
labelled `language: "en"` (captured as `volumes_wrong_edition.json`).
Title and author match exactly, so `plausibleMatch` accepts it and `en`
is written.

That looked like `2026090602`'s gate being too loose — the thing its own
out-of-scope list invited a check on. It is not. Running the shipped gate
over the population the search fallback exists for, Russian editions with
no ISBN:

| query | answer | gate | language |
|---|---|---|---|
| Пикник на обочине / Аркадий Стругацкий | Пикник на обочине | accepted | `ru` ✓ |
| Хоббит / Джон Толкин | Хоббит, или Туда и Обратно | accepted | `ru` ✓ |
| Мастер и Маргарита | Мастер и Маргарита | accepted | `ru` ✓ |
| Преступление и наказание / Фёдор Достоевский | *Prestuplenie i nakazanie (SokraScënnoe izdanie)* | **rejected**, `title_mismatch` | — |

The gate refuses the one genuine cross-language mismatch — a
transliterated German abridgement — because transliteration is a title
mismatch, which is exactly what it measures. It is doing its job.

And the two Latin-script cases that reach the column do not show what
they first appeared to:

- *Roadside Picnic* → a 2014 Hachette English edition, `en`. The stored
  title is **English**, so the file is almost certainly the English
  translation and `en` is the right answer. Calling this a wrong hit was
  the first reading's mistake.
- *O Alquimista* → `en` is Google's record being wrong, not the ranking
  picking a different book. The volume is a thin OCLC stub —
  `industryIdentifiers` is a lone `OCLC:1055765354`, no publisher, no
  description — and it genuinely is that title by that author. No gate
  over title and author can catch a catalogue mislabelling the language
  of a volume it has correctly identified.

So the residual is one wrong write out of five accepted answers, from
provider data error rather than hit selection — and the obvious remedy
makes it worse. Withholding `language` from search answers, the way
`isbn` is withheld, would sacrifice **four correct writes to avoid one
wrong one** over that set. `2026090602` declined to withhold
`published_date` on the reasoning that a field nothing keys off, visible
on the detail page and hand-fixable is not worth hedging the gate
against; `language` measures out the same way, and now with numbers
behind it rather than an argument.

Recorded as
`docs/backlog/2026090609-provider-language-can-be-wrong.md` — a known
limit worth writing down, not work worth scheduling. It is not this
step's, and it is not a plan.

### Observations that are not defects

- **`totalItems` is an estimate, not a count.** The same `isbn:` query
  answers `300` at `maxResults=1` and `1` at `maxResults=5`. Nothing
  reads it — the no-match test is `len(parsed.Items) == 0`, which is the
  right one — and `TestNoMatchIsDecidedByItemsNotTotalItems` now pins
  that so a future tidy-up cannot "simplify" it to `TotalItems == 0`.
- **The two 429s are distinguishable, and there is no `Retry-After`.**
  `2026090401` deferred "a per-day-quota retry" on the grounds that
  telling an exhausted daily quota from ordinary throttling "means
  reading `Retry-After` or parsing Google's error body". The header does
  not exist on either 429 — checked — but the body carries an unambiguous
  discriminator: `details[].metadata.quota_limit` is
  `defaultPerDayPerProject` with `quota_unit: "1/d/{project}"` for the
  daily one. That deferred item is now answerable. It is still not this
  step's; see *Not in scope*.

## Work

### 1. Map the language tag to its base subtag

In `internal/googlebooks`, add an unexported `baseLanguage(tag string)
string` that returns the part before the first `-`, lower-cased, and
apply it in `toMetadata`. `pt-BR` → `pt`, `zh-CN` → `zh`, `en` → `en`,
`""` → `""`.

Deliberately a subtag cut and **not** a mapping table: unlike Open
Library's MARC codes, Google's primary subtag is already ISO 639-1 in
every one of the 13 values observed, so there is nothing to translate —
only a region to drop. A table here would be a second thing to maintain
that agreed with the identity function.

Dropping the region loses information (`pt-BR` versus `pt-PT`). That is
the right trade for a column that is a short code on a metadata row and
that `internal/openlibrary` already fills region-free by construction,
its MARC codes having no region to carry.

Tests: `baseLanguage` as a table (`pt-BR`, `zh-CN`, `en`, `""`, and a
defensive `PT-br`), plus `TestRegionalLanguageTagReachesMetadataUnchanged`
rewritten — same fixture, now expecting `pt`, and renamed to say so.
`TestSearchCanAnswerAWrongLanguageForAMatchingTitle` is unaffected: `en`
has no region subtag to drop, and what it pins is Defect 5, not this.

**This does not close the column-consistency question, and the plan
should not claim it does.** `books.language` has four writers, and
normalising one of them leaves the other two that can carry a region:

| source | language handling |
|---|---|
| `internal/openlibrary` | MARC → ISO 639-1 via `marcToISO639`; region-free by construction |
| `internal/googlebooks` | pass-through today; region-free after this item |
| `internal/epub` | `first(pkg.Metadata.Language)`, untouched — and `dc:language` is BCP-47 **by specification**, so a region subtag is permitted |
| `internal/fb2` | `strings.TrimSpace(ti.Lang)`, untouched — conventionally two letters, but nothing enforces it |

Stated from the specification and from those two lines of code, not from
a sample: no EPUB in this library was available to check, so whether a
region subtag actually arrives from a file here is unmeasured. The
Google half is measured — 82 regional tags in 188 volumes — which is why
it is the half this step fixes.

The general fix, if it is wanted, is one derivation shared by every
writer, placed the way `storage.SortTitle` is and for the same stated
reason: two callers derive that column, so a second copy of the rule is a
library that disagrees with itself. Applying it inside `createBookTx` and
`updateBookColumnTx` would catch all four writers at once. That is a
separate plan — it touches the scanner, the edit path and a migration
question for rows already written — and it should be written only once
someone has actually seen a regional tag come out of a file, rather than
on the strength of the specification permitting one.

### 2. Fetch the larger cover from the single-volume endpoint

Give `Client` a second request, made **only** when a matched volume has
an id and its list `imageLinks` offer nothing above `thumbnail` — which
in practice is always, but the condition states the intent and costs one
comparison:

- `volume` needs an `ID string \`json:"id"\`` field first — it has only
  `VolumeInfo` today, so the list response is currently parsed without
  ever seeing which volume it names, and there is nothing to build the
  second URL from.
- `GET {baseURL}/volumes/{volumeID}` (key appended the same way, same
  `User-Agent`, same `maxResponseBytes` cap), decoded into that same
  `volume` type — the detail endpoint answers one volume object, the
  identical shape the list nests, so no second type is needed.
- On success, `best()` over the detail response's `imageLinks`. On
  anything else — non-200, malformed body, transport failure — **keep the
  list response's `thumbnail` and return no error.** A bigger cover is a
  nicety; a lookup that already has six good text fields must not fail
  for it. Log at Debug, not Warn: this fires per enriched book and a
  Warn-per-book teaches people to ignore Warns.
- The detail response's `description` is HTML, so `plainText` applies to
  it. Prefer it over the list one when it is non-empty — that is where
  the paragraph structure of Defect 4 comes back, and it is free once the
  request is being made anyway.

The cost is one extra request per *matched* volume — and only a matched
one, so a book this catalogue does not hold still costs what it costs
today. `2026090602` made the ceiling higher than it was when this was
first drafted: a provider now answers `ByISBN` and, on a clean no-match,
`Search` as well, so one provider already spends up to two requests on
one book. Adding a detail request takes the worst case to three: a
missed ISBN, a search that matches, and its cover.

That is still the right trade. Both decorators wrap the `Provider`
method rather than the HTTP call, so `WithRateLimit` paces the whole
lookup and `WithCache` serves a repeat of it without any of the
requests. The anonymous per-day quota is exhausted for everyone — this
provider is only usable with a key at all — and a keyed project's daily
allowance is not troubled by three requests for a book being enriched
once.

If it ever needs bounding, the lever is the condition above: skip the
detail request when the list `imageLinks` are absent entirely, since a
volume with no thumbnail has no larger size either. Not applied now
because it optimises the case that already costs nothing.

**Do not rewrite `zoom=`.** Record why in a comment at `best()`, with the
placeholder observation — it is exactly the shortcut a later reader
reaches for, and it silently poisons the cover directory.

Tests, all against `httptest.Server`:
- The detail request is made, hits `/volumes/{id}`, and its `large` link
  wins over the list `thumbnail` — replaying `volumes_match.json` then
  `volumes_detail.json`.
- A detail request that 500s, that times out, and that returns garbage
  each leave `CoverURL` at the upgraded list `thumbnail` and return a nil
  error with every text field intact.
- A volume with no id makes no second request.
- The detail description replaces the list one and arrives with its
  `\n\n` paragraph breaks; an empty detail description leaves the list
  one alone.
- `TestListEndpointOffersOnlyTheThumbnailSizes` stays as it is — it pins
  the finding this work responds to, and it is about the captures, not
  about `best()`.

### 3. Correct the reasoning, in both places

Replace the 403 comment in `search` with what was measured:

> A rejected key is 400, an exhausted quota — per-day or per-minute — is
> 429, and 403 is the service not being enabled for the project. Only the
> 429 and the 5xx are worth asking twice; the other two are configuration
> and answer identically however often they are asked.

And in CLAUDE.md, the `internal/openlibrary`/`internal/googlebooks`
paragraph:

- the clause "a 403 (Google's over-quota and rejected-key answer)" →
  a 400 for a rejected key and a 403 for a service not enabled, with 429
  named as the over-quota answer on the retryable side;
- the ISO 639-1 sentence — currently "mapped to the ISO 639-1 form
  `internal/epub`, `internal/fb2` and Google Books all produce" → Google
  answers BCP-47, its region subtag is dropped here, and the claim about
  the file parsers is withdrawn rather than restated: neither normalises,
  and `dc:language` is BCP-47 by specification. The corrected sentence
  should say what is true — the two provider clients agree on a
  region-free code — and not what was assumed about the other two;
- the "Google's `description` is documented as *HTML-formatted*" sentence
  → true of the single-volume endpoint, which is why `plainText` runs on
  the detail response; the list endpoint answers plain text already;
- the fixture-provenance sentence → for `internal/googlebooks`, **every**
  fixture is now a live capture. That sentence currently reads "The rest
  are shaped after each API's stable, publicly documented response format
  instead"; after this it covers only `internal/openlibrary`'s
  `search_*.json`.
- add the cover sentence: Google's list endpoint names only a 128px
  thumbnail, so a matched volume costs a second request to the
  single-volume endpoint for a cover worth storing, and `zoom=` rewriting
  is not an alternative.

### 4. Delete the backlog item

`docs/backlog/2026090403-googlebooks-has-no-live-verification.md`, in
this change, per CLAUDE.md's backlog lifecycle.

## Verification

- `go build ./... && go vet ./... && go test ./...`.
- The captures already committed with this plan's verification pass
  against the *current* code; after the work, the two tests named above
  change expectation and everything else must still pass unchanged. A
  test that needed a fixture edited to keep passing means the fixture is
  being fitted to the code, which is the failure this whole exercise
  exists to catch.
- One live re-run with `GOOGLE_BOOKS_API_KEY` set, through the real
  `*Client`, confirming a `pt-BR` volume comes back `pt` and that a
  matched volume's `CoverURL` fetches at more than 128px. Not a committed
  test — no test in this repo reaches the network.
- Check no capture carries the key or the project number: the two 429
  fixtures have `project_number:` and `projects/` rewritten to zeros
  already, and that must stay true of anything added.

## Not in scope, deliberately

- **`maxResults=1`.** Drafted as an open question; it is settled.
  `2026090602` weighed "scoring or ranking beyond the first result",
  called it "a genuinely better design and a much larger change" — both
  provider clients widened, `Metadata` becoming a slice somewhere — and
  ruled it out. Its gate accepts or rejects one candidate, which is
  exactly what one result gives it. So `maxResults=1` is correct as it
  stands rather than a gap, and raising it would be work for a design
  that has been declined.
- **The `language` a provider gets wrong.** Measured, demoted to a
  backlog item, and deliberately not acted on — the remedy costs more
  correct values than it saves wrong ones. See *Not a defect 5*.
- **The wrong-*book* problem.** Distinct from Defect 5, and now handled:
  `plausibleMatch` shipped, and the live check's other candidates —
  `intitle:"untitled document"` answering a real self-published book,
  and a nonsense title answering nothing at all — are the cases it was
  built for.
- **Distinguishing the two 429s.** Now demonstrably possible from the
  body (see *Observations*), and `2026090401`'s deferral of it stands on
  its own terms: it is a design decision about retry policy, not a
  fidelity correction, and it belongs with whatever revisits
  `WithRetry`. What this step contributes is the discriminator and two
  captured bodies to write it against.
- **`smallThumbnail`.** Never useful — it is smaller than `thumbnail`,
  and `best()` correctly has no case for it. Left out of the struct.
- **`isZeroMetadata`.** `2026090601` added `Metadata.IsEmpty()`, which
  both provider test packages now duplicate as a local helper. A real
  redundancy, equally in `internal/openlibrary`; collapsing it in one
  package only would be worse than leaving both, and it is not this
  step's concern either way.

## Cross-references

- `docs/plans/completed/2026090602-search-match-confidence.md` — the gate
  Defect 5 is about, and the decision that settles `maxResults`.
- `docs/plans/completed/2026090603-provider-cover-regeneration.md` —
  `ClearProviderCover`, which is why Defect 2 recurs.
- `docs/plans/completed/2026090401-openlibrary-field-fidelity.md` — the
  precedent for how this check is run and for keeping its findings out of
  the verification itself.
