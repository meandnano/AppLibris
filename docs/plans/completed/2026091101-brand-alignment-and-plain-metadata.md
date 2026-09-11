# Brand name, `added` alignment, plain-text descriptions, single-line scalars

## Context

Three unrelated defects visible on the shipped UI, found while using the app,
plus one deferred backlog item folded in because it lands in the same
function as the third:

1. **The masthead still says "Bookshelf".** That was the mockups' working
   name; the application is AppLibris. It also leaks into the `<title>` of
   every page and into `edit.js`, which rebuilds that title after an inline
   title edit.
2. **The `added` row's value is right-aligned** while every other row in the
   same `<dl>` reads flush-left after its label. Not a deliberate treatment —
   it falls out of `.detail__meta-row` being `justify-content: space-between`
   while only the *editable* rows neutralise it.
3. **A description read out of an EPUB can carry HTML and is shown raw.**
   `html/template` escapes it, so the reader sees a literal `<b>…</b><p>`,
   and the edit textarea then offers them the same markup to hand-fix.
   Penguin and similar publishers ship marketing copy as escaped HTML in
   `<dc:description>`; nothing between the parser and the page flattens it.
4. **Embedded metadata keeps line breaks the editor refuses** —
   `docs/backlog/2026091006-embedded-metadata-keeps-its-line-breaks.md`.
   Adjacent to (3): both are about what the scanner is allowed to store in a
   metadata column, and both are fixed on the path between the parsers and
   `createBook`.

Descriptions are to be plain text, not rich text — the only markup that
carries meaning is a line break.

### Scope decision: stripping happens on write only

No backfill. Books already in the database keep the HTML already stored:
the scanner reads embedded metadata only when it creates a book, so a rescan
will not rewrite them, and saving such a description unchanged stores the
markup back (a person's edit is stored as typed). The practical fix for an
existing library is to delete the database and rescan, or hand-edit the
field. The same applies to (4).

## 1. Brand: Bookshelf → AppLibris

Four live occurrences, no Go source and no test asserts the string today:

- `internal/web/templates/partials.html:5` — `<title>{{.Title}} · Bookshelf</title>`
- `internal/web/templates/partials.html:25` — `<span class="masthead__brand">Bookshelf</span>`
- `internal/web/static/js/edit.js:49` — `document.title = title.textContent + " · Bookshelf";`
- `internal/web/static/css/app.css:1` — header comment

The first and third have to agree or an inline title edit silently renames
the tab. Add a small test in `internal/web` that reads the embedded
`edit.js` and asserts the suffix it appends is the one `document-head`
renders — the same shape as the existing tests that parse `app.css`.

Leave `docs/plans/completed/` and the `init`-branch mockup filenames alone;
completed plans are immutable.

## 2. `added` row alignment

`internal/web/static/css/app.css`.

Mechanism: `.detail__meta-row` (line 986) is `display: flex;
justify-content: space-between`, so with two children the `<dd>` is pushed to
the right edge. The four editable rows escape it only because
`.editable--meta dd` is `width: 100%` (line 1254), which makes the `<dd>`
absorb the free space so its `inline-flex` anchor renders flush-left. The
`added` row (`internal/web/templates/book.html:54-57`) is hand-written plain
markup with no `editable--meta` class, so its `<dd>` shrink-wraps.

Fix it on the row rather than on the one exception — give every `<dd>` the
free space:

```css
.detail__meta-row dd {
  flex: 1;
  min-width: 0;
  margin: 0;
  font-size: 13.5px;
}
```

`min-width: 0` so a long unbroken value cannot push past the grid column,
which `.detail__meta` already guards with `minmax(0, 1fr)`.

That makes the `dd` half of the `.editable--meta` rule redundant; narrow it
to `.editable--meta form { width: 100%; }` rather than leaving a second rule
saying the same thing. `editable--meta` is applied only by
`{{define "book-field-meta"}}` (`partials.html:419-433`), always on a
`.detail__meta-row`, so nothing else depends on it.

## 3. Descriptions stored as plain text

### Where the markup enters

- **EPUB** — `internal/epub/epub.go:105`, `Description: first(pkg.Metadata.Description)`.
  The XML decoder unescapes `&lt;p&gt;` into a literal `<p>`, and it reaches
  the column verbatim. This is the whole gap.
- **FB2** — already flattened structurally: `paragraph.UnmarshalXML`
  (`internal/fb2/fb2.go:106-120`) drops inline markup and `annotationText`
  (`:519`) joins paragraphs with `\n\n`. No change.
- **Google Books** — already flattened by `plainText`
  (`internal/googlebooks/googlebooks.go:562`). No behaviour change.
- **Open Library** — plain by construction (`textValue`, `openlibrary.go:304`).
  Unchanged, and the reasoning in `docs/notes/enrichment.md` for not
  stripping in `internal/enrich`'s `sanitizeValue` still stands.

### Reuse, don't reimplement

`internal/googlebooks` already has the exact stripper, with tests: block
tags (`br`, `p`, `div`, `li`, `tr`, `h1`–`h6`) become a newline, every other
tag is dropped, a `<` that starts nothing tag-shaped survives (`tagAt`), and
entities are unescaped only *after* the tags are gone so escaped markup
survives as the characters an author wrote. Keep that behaviour exactly —
it is what makes a publisher's blurb read as paragraphs.

Move it down to `internal/storage`, beside `SortTitle` and `NormalizeISBN`,
which is where this repository already puts a derivation more than one
writer of a metadata column shares — `internal/epub` and
`internal/googlebooks` both import `internal/storage` today (both call
`NormalizeISBN`), so there is no new dependency and no cycle.

In `internal/storage/metadata.go`, export as `PlainDescription` and bring
with it the unexported `blockTags`, `tagAt`, `collapseBlankLines`,
`trimBlank` and `zeroWidth` from `internal/googlebooks/googlebooks.go:540-660`.

Then:

- `internal/googlebooks/googlebooks.go:385` and `:485` call
  `storage.PlainDescription` instead of the removed local `plainText`.
- `internal/epub/epub.go:105` becomes
  `Description: storage.PlainDescription(first(pkg.Metadata.Description))`.

Order matters at the one place both run: the scanner's `capMetadata`
(`internal/scanner/scanner.go:864`) caps *after* the parser has flattened,
so the 64 KiB cut applies to the plain text that is actually stored. That
falls out of doing it in the parser and needs no change to `capValue`.

Note the one behavioural edge worth stating in the notes: a plain EPUB
description containing `&` now gets one unescape pass, so a literal
`&amp;` written as text becomes `&`. That is the same trade
`internal/googlebooks` already makes, and the fast path skips it entirely
when the value holds neither `<` nor `&`.

## 4. Scalars reach the column on one line

Folding in `docs/backlog/2026091006-embedded-metadata-keeps-its-line-breaks.md`.

**Re-validated against the current code, and it still holds.**
`internal/epub`'s `first` (`epub.go:328`) and `internal/fb2`'s `authorName`
(`fb2.go:494`) apply `strings.TrimSpace` and nothing else, so a metadata
element whose text is wrapped across two lines in the source XML — legal,
and what a generator that pretty-prints produces — keeps the interior break.
`internal/service`'s `normalizeField` rejects `\r`/`\n` in every field but
description (`internal/service/metadata.go`, pinned by
`TestNormalizeFieldRejectsLineBreaksExceptInDescription`,
`metadata_test.go:187`), so such a value can be stored but not saved back:
a scalar is silently rewritten by the browser stripping CR/LF from
`<input type="text">` and its provenance flips to `manual`, and an author
name in the `<textarea>` is **split into two authors** by
`normalizeAuthors`.

The backlog item asks to re-validate one thing before acting: whether
`internal/epub` and `internal/fb2` have since gained a shared normalisation
of their own, in which case the fix would belong there. They have not — and
section 3 above gives them one for *description*, which is precisely the
field exempt from this rule. So the fix stays where the item sketched it.

Fold the collapse into `capValue` (`internal/scanner/scanner.go:888`) for
every field but description, **before** the length cut: the collapse can
only shorten the value, and cutting first would let a truncation boundary
decide whether a break survives.

```go
if field != storage.FieldDescription {
    value = strings.Join(strings.Fields(value), " ")
}
```

That is the same expression `internal/enrich`'s `sanitizeValue`
(`resolver.go:103`) applies to a provider's answer, which `capValue`'s
doc comment already cites as its peer — extend that comment rather than
adding a second one. It runs per author name, since `capMetadata` calls
`capValue` element by element (`scanner.go:872`), so a wrapped name stays
one name. Keep the truncation log on truncation only; a collapsed break is
not worth a line.

Delete the backlog file in the same change, as CLAUDE.md's backlog
lifecycle requires.

## Tests

- Move `TestPlainText` (`internal/googlebooks/googlebooks_test.go:291`) and
  the `trimBlank` property test (`:1661`) into `internal/storage`, renamed
  to the exported function. The googlebooks tests that exercise it through
  the client API — `TestListEndpointDescriptionCarriesNoMarkup` (`:255`),
  and those at `:1044`, `:1220`, `:1290`, `:1335` — stay where they are and
  keep passing unchanged.
- Add an `internal/epub` case beside the plain-description test at
  `epub_test.go:208`: a `<dc:description>` carrying escaped HTML comes back
  with the tags gone and `<p>`/`<br>` as line breaks.
- Extend `internal/scanner/capmetadata_test.go`. `TestCreateBookCappedValuesAreEditable`
  (`:150`) is the right home for the folded item: it already asserts a
  stored value is one the editor accepts, and the line-break case makes that
  claim whole. Add a title with an interior newline (stored on one line), an
  author name with one (stays a single author), and a description with one
  (keeps it).
- Existing `internal/web/web_test.go:1191` and `:1224` (the `pre-line` rule
  and the paragraph break reaching the markup) must stay green — they are
  what make the preserved newlines visible.

## Documentation

Per CLAUDE.md, these describe the current state only — replace sentences,
never append a correction.

- **`CLAUDE.md`** — add `PlainDescription` to the storage bullet listing the
  shared derivations; rewrite the enrichment invariant that says the HTML
  flattening lives in `internal/googlebooks` to name its new home; add a
  formats invariant that a format package hands the scanner a plain-text
  description; widen the scanner's `capMetadata` invariant from the length
  half to the whole property — every value it stores is one the editor
  accepts.
- **`docs/notes/formats.md`** — why the EPUB parser flattens (`dc:description`
  legally holds escaped HTML; nothing downstream renders markup) and why FB2
  needs no such pass.
- **`docs/notes/storage.md`** — the derivation's rationale, in the paragraph
  that already covers `SortTitle`/`NormalizeISBN`.
- **`docs/notes/enrichment.md`** — the paragraph at ~433 currently says
  the stripper "lives here rather than in `sanitizeValue`". Rewrite it in the
  present tense: the derivation is shared from `internal/storage`, `enrich`'s
  `sanitizeValue` still does not call it, and Open Library's plain
  description is still the reason.
- **`docs/notes/scanner.md`** — the `capMetadata` paragraph claims the length
  property; rewrite it to claim the whole one.
- **`docs/notes/web.md`** — the `pre-line` and `.detail__meta` paragraphs;
  state that every `<dd>` in the row takes the free space, so the one
  non-editable row is not the odd one out.

Write the implementation plan itself to
`docs/plans/2026091101-brand-alignment-and-plain-metadata.md` before
starting (today's sequence is free — `2026091005`/`2026091006` are the 10th),
and move it to `docs/plans/completed/` in the commit that finishes the work.

## Verification

1. `go test ./...` — the moved tests, the new EPUB case, the new scanner
   cases and the new brand test all pass; nothing in `internal/googlebooks`,
   `internal/web` or `internal/scanner` regresses.
2. `go vet -race ./...`, as CI runs it.
3. `make run` against `./library` with an EPUB whose `<dc:description>`
   holds escaped HTML (the Penguin file from the report is one). On a **fresh
   database** — delete `./data/library.db` first, since nothing rewrites an
   already-stored value — confirm on `/books/{id}`:
   - masthead and browser tab read AppLibris;
   - the description shows no tags, with paragraph breaks intact;
   - `added` sits flush-left level with `publisher`, `language`,
     `published` and `isbn`.
4. Open the title editor and save — the tab title still reads
   `… · AppLibris`, which is the `edit.js` half.
5. Open the description editor — the textarea holds the same plain text,
   and saving unchanged is a no-op.
6. For (4): an EPUB whose `<dc:title>` is wrapped across two lines in the
   OPF renders on one line, and opening that field and saving unchanged
   leaves it unchanged rather than flipping provenance to `manual`.
7. `docs/backlog/2026091006-embedded-metadata-keeps-its-line-breaks.md` is
   gone.

---

## Correction found while implementing

The Tests section says `TestCreateBookCappedValuesAreEditable` is "the right
home for the folded item". It is not. That test is built on `overlongEPUB`,
whose every field is a `straddling()` filler sized past a limit; a wrapped
title has to be readable prose for the assertion to say anything, and adding
line breaks to the filler would have made one fixture answer two unrelated
questions and neither clearly.

The line-break property is a sibling test instead,
`TestCreateBookWrappedValuesAreEditable` over its own `wrappedEPUB`, next to
the capping one and making the same round-trip claim. Mutation-checked:
removing the collapse from `capValue` fails it on the title, the publisher
and the split author name.
