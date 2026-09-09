# Backlog: an EPUB's declared ISBN is stored without checking it looks like one

## Problem

`internal/epub` recognises an ISBN three ways, in order: an identifier
with `opf:scheme="ISBN"`, a `urn:isbn:` identifier, or a bare
ISBN-shaped one. `normalizeISBN` strips hyphens and spaces and
upper-cases a trailing `x`. Only the third branch checks the shape
(`isBareISBN`); the first two return whatever remains after
normalisation.

Publishers write things like:

```xml
<dc:identifier opf:scheme="ISBN">ISBN 978-0-00-000000-0 (ebook)</dc:identifier>
<dc:identifier opf:scheme="ISBN">Not available</dc:identifier>
```

which store as `ISBN9780000000000(ebook)` and `Notavailable`. That value
is shown on the detail page, indexed under the `isbn` FTS column, and
used as the lookup key for both providers' `ByISBN`, which answer
no-match and fall through to `Search` on every run. Because the field is
filled, `Resolve` never treats it as missing, so enrichment can never
correct it either (and CLAUDE.md records that enrichment cannot write
`isbn` by any route in any case).

`internal/fb2` stores `<isbn>` verbatim, hyphens included, so the column
already holds two spellings across formats; `SanitizeFTSQuery` and the
FTS row's own `replace()` paper over that for search but not for the
provider key.

## Why this is backlog, not a plan

One visible, editable field holding a slightly wrong value, from a file
that wrote it wrongly. The provider fallback still runs, so enrichment
still works for the book, just with one wasted request. Nothing is
corrupted and nothing blocks.

## Re-validate before acting

- Whether the two marked branches in `internal/epub` still skip
  `isBareISBN`.
- Whether `internal/fb2` still stores the raw string.

## Sketch

Run the candidates from the scheme and URN branches through the same
shape check the bare branch uses, and on failure fall through to the
next identifier rather than accepting. Better still, extract the first
ISBN-shaped run (10 or 13 digits with a possible trailing X, hyphens
allowed) from the value, which recovers the `ISBN 978-… (ebook)` case
instead of discarding it.

Route `internal/fb2`'s `<isbn>` through the same normaliser so the
column holds one spelling. The normaliser is duplicated in
`internal/epub`, `internal/storage` and both provider clients already;
if this is the change that makes a fourth copy, it is also the moment to
place one shared derivation the way `SortTitle` is placed.
