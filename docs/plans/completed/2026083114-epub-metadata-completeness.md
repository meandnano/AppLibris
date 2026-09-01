# Step: EPUB metadata completeness

## Context

`internal/epub` extracts title, authors, language, ISBN, description and
cover. Three gaps, all of which cost the book detail page real content.

**1. `dc:publisher` and `dc:date` are never parsed.** `opfPackage.Metadata`
has no fields for them, so `books.publisher` and `books.published_date` are
**always empty strings** — despite being columns in the schema since the
first migration, fields in DESIGN.md's data model, and two of the rows
UI.md's book detail screen is designed around. Most EPUBs carry both. This
is the cheapest metadata win available and it is a prerequisite for the
detail page being worth building: three of its eight metadata rows are
currently guaranteed blank.

**2. ISBN is only found via a `scheme` attribute.** `findISBN` walks
`dc:identifier` elements looking for `opf:scheme="ISBN"`. That is the EPUB 2
idiom. EPUB 3 dropped `opf:scheme` and expresses the identifier's type
either as a `urn:isbn:` URN in the element text, or via a refining meta
(`<meta refines="#pub-id" property="identifier-type">`). Neither is matched,
so ISBNs are missed on most modern files. There is even a test
(`TestReadMetadataNoScemeISBN`) pinning the current behaviour for a
`urn:uuid:` identifier — correct as far as it goes, but it reads as though
scheme-less identifiers are *supposed* to yield nothing.

Additionally, when a scheme'd identifier's value *is* `urn:isbn:9780…`, the
prefix is returned raw. Downstream, ISBN is the lookup key for DESIGN.md's
provider chain (Open Library, Google Books), and `urn:isbn:9780…` is not a
key either provider accepts.

**3. Cover hrefs are not URL-unescaped.** `readCover` does
`path.Join(path.Dir(opfPath), href)` and opens that directly. A manifest
href is a URI reference, so a cover stored as `cover art.jpg` appears as
`cover%20art.jpg` and the zip lookup fails. `readCover` returns nil on any
open failure by design, so the book gets no cover and nothing is logged —
it just silently looks like a book with no embedded cover.

## Scope

In scope: the three gaps above, in `internal/epub`, plus threading publisher
and published date through `internal/scanner` into the columns that already
exist. Out of scope: FB2 (`docs/backlog/2026083119-fb2-metadata.md`),
provider enrichment, per-field provenance, and any UI — the detail page is
its own step and wants these fields populated before it starts.

## Publisher and date

Add to `opfPackage.Metadata`: `Publisher []string \`xml:"publisher"\`` and
`Date []string \`xml:"date"\``. Add `Publisher` and `PublishedDate` to
`Metadata`, populated with the existing `first()` helper.

**Date needs care.** `dc:date` is not a year — EPUB 2 allows an
`opf:event` attribute (`publication`, `modification`, `creation`) on
repeated `dc:date` elements, and EPUB 3 defines `dc:date` as the
publication date in ISO 8601, with modification tracked separately as
`dcterms:modified`. So:

- Parse `dc:date` with its `event` attribute, same shape as `Identifier`.
- Prefer the element with `event="publication"`; fall back to the first
  element with no `event` (the EPUB 3 case); ignore `creation` and
  `modification` outright — storing a file's modification date in a column
  called `published_date` is worse than leaving it empty.
- Store the value **as found**, without normalising to a year. Values in the
  wild range from `2011` to `2011-05-01` to
  `2011-05-01T00:00:00+00:00`, and DESIGN.md's column is `published_date
  TEXT` precisely so it can hold whatever the source says. Formatting for
  display is the template's job; normalising here would throw away
  precision that a provider merge might later want to compare against.

## ISBN

Rewrite `findISBN` to try, in order:

1. An identifier with `scheme` equal to `ISBN`, case-insensitively (today's
   behaviour, kept — it is correct for EPUB 2).
2. Any identifier whose value, lower-cased and trimmed, starts with
   `urn:isbn:`.
3. Any identifier whose value is a bare ISBN-10 or ISBN-13 — digits,
   hyphens and spaces only, normalising to 10 or 13 characters, with a
   trailing `X` permitted for ISBN-10.

Then normalise the winner: strip a `urn:isbn:` prefix, remove hyphens and
spaces, upper-case a trailing `X`. Return `""` if nothing qualifies.

**Do not validate the check digit.** A malformed ISBN in the file is still
the best identifier the file offers, and rejecting it means falling back to
a title+author provider search that is strictly less precise. Validation
belongs in the provider step, which can decide whether to trust it.

`TestReadMetadataNoScemeISBN` keeps passing unchanged — a `urn:uuid:`
identifier matches none of the three rules — but rename it to say what it
now means (`TestReadMetadataIgnoresNonISBNIdentifier`), because the current
name will read as a contradiction once rule 2 exists.

The `refines`-based EPUB 3 form is deliberately **not** implemented: it
requires resolving `refines="#id"` back to the identifier element, and rules
2 and 3 already catch the overwhelming majority of real files, since a
publisher who bothers to mark an identifier as an ISBN almost always writes
it as a `urn:isbn:` URN too. Revisit if real files in the library turn out
to need it.

## Cover href unescaping

In `readCover`, before joining:

```go
decoded, err := url.PathUnescape(href)
if err != nil {
	decoded = href // not a valid escape sequence; treat it as a literal name
}
```

Then also strip any fragment (`#...`) — a manifest href should not carry
one, but a malformed file with `cover.jpg#page1` currently fails the lookup
for a reason nobody will guess from the symptom.

Falling back to the raw href on a decode error rather than giving up: a
filename containing a literal `%` that is not a valid escape is legal in a
zip and would otherwise regress from "works" to "no cover".

While here, add a Debug log when a cover href is declared but the entry
can't be opened. `readCover`'s silence is right for control flow — a bad
cover must not fail the whole parse — but "declared a cover and we couldn't
read it" is a genuinely different state from "declared no cover", and at
present they are indistinguishable from outside.

## `internal/scanner`

- `bookMeta` gains `Publisher` and `PublishedDate`; `extractMetadata`
  passes them through.
- `createBook` sets `Publisher` and `PublishedDate` on the `storage.Book`.
- `storage.CreateBook`'s INSERT already lists both columns, so no storage
  change is needed — they have been in the statement all along, receiving
  the zero value.

**Existing books do not get these fields retroactively.** Metadata is
parsed only on first sight of a content hash, so every book already indexed
keeps its empty publisher and date. That is consistent with how cover
extraction behaved before `2026083111-cover-regeneration`, and the honest
fix is a re-parse pass — which is really the same machinery a provider
enrichment queue needs (walk existing rows, fill missing fields, record
provenance) and should be built once, there, rather than twice. Say so in
the commit message; the alternative — deleting the database to re-scan — is
a legitimate answer for a personal library at this stage and worth stating
as the interim workaround.

## Tests

`internal/epub`, extending the existing `buildTestEPUB` fixtures:

- Publisher and a plain `dc:date` are extracted.
- With multiple `dc:date` elements, `event="publication"` wins over
  `event="modification"`.
- With only `event="modification"`, `PublishedDate` is empty — the case
  that matters, since the naive `first()` would take it.
- ISBN from `opf:scheme="ISBN"` (existing behaviour, keep).
- ISBN from `urn:isbn:9780306406157` → `9780306406157`.
- ISBN from a bare hyphenated `978-0-306-40615-7` → `9780306406157`.
- A `urn:uuid:` identifier yields no ISBN (the renamed existing test).
- A cover whose manifest href is percent-encoded (`cover%20art.jpg` for a
  zip entry named `cover art.jpg`) is found. This test fails today.

`internal/scanner`: a scanned EPUB's `books` row has non-empty `publisher`
and `published_date`.

## CLAUDE.md

Update the `internal/epub` bullet: publisher and publication date are read
(preferring `opf:event="publication"` among repeated `dc:date` elements),
ISBN is recognised in `opf:scheme`, `urn:isbn:` and bare forms and stored
normalised, and cover hrefs are percent-decoded before the zip lookup.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: point `LIBRARY_DIR` at a handful of real EPUBs from different
  sources (a Project Gutenberg file, a store-bought one, a Calibre export),
  scan into a fresh database, and inspect
  `SELECT title, publisher, published_date, isbn FROM books` — the point is
  to see the variety of real `dc:date` formats land intact rather than to
  match one expected string.
