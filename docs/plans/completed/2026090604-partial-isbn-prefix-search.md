# Step: a partially typed hyphenated ISBN should match while it is typed

## Position in the sequence

Independent of everything else here. It touches one function in
`internal/storage` and nothing else, and can be built at any point.

## Context

`docs/backlog/2026090202-partial-isbn-prefix-search.md`, re-validated:
`SanitizeFTSQuery` and `normalizeIfISBNShaped` are unchanged since it was
written, and the function's own doc comment already names the item.

Search matches word prefixes as you type — the whole reason for the 300ms
debounce — and it matches a *complete* ISBN in either punctuation, which
`2026090106`'s review round fixed on both sides: `SanitizeFTSQuery`
normalises an ISBN-shaped whole query, and `syncBookFTSTx` indexes
`replace(replace(b.isbn, '-', ''), ' ', '')`, so the index holds one bare
token however the source file punctuated it.

Both halves key on the *finished* string. `normalizeIfISBNShaped` takes its
path only when the whole query is exactly 10 or 13 characters once hyphens
and spaces are stripped, so every intermediate state of typing a hyphenated
ISBN takes the ordinary per-word path:

| typed so far | becomes | matches `9780857059985`? |
|---|---|---|
| `978085` | `"978085"*` | yes — a prefix of the one indexed token |
| `978-0-85705` | `"978-0-85705"*` | **no** |
| `978-0-85705-998-5` | `"9780857059985"*` | yes — the ISBN path takes it |

The middle row's reason is not the one it looks like. `strings.Fields`
splits on whitespace only, so the hyphenated input stays one token and is
quoted whole — it does not become three AND-ed prefix terms. Inside those
quotes `unicode61` still tokenizes on the hyphens, so FTS5 reads it as the
**phrase** `978 0 85705`, and the single indexed token `9780857059985` does
not contain that phrase.

So the results go empty mid-typing and fill back in on the last character.
The unpunctuated form works throughout, and pasting either form whole
works.

## Scope

In scope: widening the ISBN branch so a partially typed **hyphenated** ISBN
takes it.

Out of scope, with reasons — these are the backlog's other two sketches,
recorded as ruled out rather than unconsidered:

- **Stripping separators from any digit-and-hyphen token before quoting.**
  It changes what a hyphen means for every query, including hyphenated
  titles and double-barrelled author names, where splitting is the wanted
  behaviour. The blast radius is the whole search box for a gain confined
  to one field.
- **Indexing the raw `books.isbn` alongside the normalised one.** It makes
  a hyphenated prefix match hyphenated *storage* only, so it does not fix
  the cross-punctuation case this is a sequel to — a query hyphenated the
  way `internal/fb2` stored it still misses an EPUB's bare digits. It
  costs an index column to solve half the problem.
- **Space-separated partial ISBNs** (`978 0 85705`). See Decision 2.

## Decision 1: widen on hyphen count, not on length alone

The backlog called "take the ISBN path on a whole query that is only
digits, hyphens and spaces, of any length" the most promising option. It is
nearly right and has one regression in it worth designing around.

Dropping the length test alone makes any hyphen-separated numeric query an
ISBN query, including `1984-2001` — a date range in a title, which today
becomes the phrase `1984 2001` and matches *Collected Essays 1984–2001*,
and which under the naive widening becomes `"19842001"*` and matches
nothing. Trading one dead input for another is not a fix.

The distinguishing signal is **how many groups the hyphens make**. A
hyphenated ISBN always has at least four groups — prefix, registration
group, registrant, publication, check digit for ISBN-13; four for ISBN-10 —
so three hyphens minimum in a complete one, and two by the time a person is
far enough through typing for this to matter. A two-group numeric query is
far more likely a range, a year pair or a volume number.

So the branch becomes:

```go
// normalizeIfISBNShaped reports whether input is a query about an ISBN,
// and its bare-digit form if so. Two shapes qualify:
//
//   - exactly 10 or 13 digits once hyphens and spaces are stripped (a
//     trailing X permitted) — a complete ISBN, however punctuated, which
//     is what a paste produces;
//   - digits with at least two hyphens between them and no more than 13
//     digits in total — a hyphenated ISBN partway through being typed.
//
// Two hyphens rather than one is what keeps "1984-2001" a title query:
// a hyphenated ISBN has at least four groups, so by the time one is worth
// prefix-matching it has two separators, while a two-group numeric query
// is a range far more often than an identifier.
```

Both shapes return the same thing — the stripped digits, upper-cased — and
the caller is unchanged: `"` + digits + `"*`.

Note what does **not** change: an unhyphenated partial (`978085`) already
works through the per-word path, and continues to, because it never enters
this branch. Widening the branch to cover it would produce the identical
expression by a longer route.

## Decision 2: hyphens only; a space keeps its ordinary meaning

The complete-ISBN shape strips spaces as well as hyphens, and that stays —
a full 10 or 13 digits split by spaces is unambiguous, and it is what
copying an ISBN out of a badly formatted page produces.

The **partial** shape accepts hyphens only. A space is the token separator
for the entire rest of the search box: treating `1984 2001` as one
seventeen-year number would break a legitimate two-term query to serve an
input nobody produces, since a person typing an ISBN by hand types the
hyphens that are printed on the book. The asymmetry is deliberate and is
worth the comment it costs.

## Decision 3: no change to the index

`syncBookFTSTx` already stores the bare normalised form. Both shapes above
produce a prefix of that token, so nothing about the index-side half needs
to move. This step is one function.

## Changes

`internal/storage/ftsquery.go` only:

- `normalizeIfISBNShaped` gains the second shape; the existing one is
  untouched so the paste case cannot regress.
- The `SanitizeFTSQuery` doc comment's paragraph beginning "Only a whole
  query of that shape takes the ISBN path" is replaced — it currently
  documents the gap and names the backlog file, and the file will be gone.

## Tests

`internal/storage/ftsquery_test.go`, extending the existing table:

Newly matching:

- `978-0-85705` → `"978085705"*`
- `978-0-8` → `"97808"*`
- Every intermediate state of typing `978-0-85705-998-5` from the second
  hyphen onward, asserted as a sequence — "it goes empty and comes back"
  is a property of the sequence rather than of any one input.
- `0-306-40615-2` (ISBN-10, hyphenated, complete) — already worked via the
  length rule; assert it still does.
- A trailing hyphen (`978-0-`), which a person types on the way through.

Unchanged, and these are the ones that matter:

- `1984-2001` → the per-word path, still `"1984-2001"*`.
- `978085` → the per-word path, unchanged.
- `Twenty-One Balloons` → per-word, unchanged; a hyphenated *title* is the
  behaviour any widening puts at risk and the backlog says to check it.
- `978-0-85705-998-5-1-2-3` (fourteen digits) → per-word: over the digit
  cap, so not an ISBN query.
- A query mixing letters and hyphens (`ISBN-978-0`) → per-word.
- Everything already in the table.

`internal/storage`'s search tests: one end-to-end assertion that a book
stored with a bare ISBN and a book stored with a hyphenated one are **both**
found by a hyphenated partial. That is the cross-punctuation property this
is a sequel to, and it is the one a purely unit-level table would not
catch.

## CLAUDE.md

The `books_fts` paragraph mentions `SanitizeFTSQuery` as "the one place raw
user input becomes [a valid MATCH expression]". Add a clause on the two
ISBN shapes and the two-hyphen rule — the rule reads as arbitrary without
its reason, and an arbitrary-looking constant is one somebody rounds to
one.

## DESIGN.md (on `init`)

No change. Search behaviour at this granularity is below what that document
describes.

## Verification

Against the real library, with the server running:

- Type `978-0-85705-998-5` one character at a time into the search box.
  Results appear from the second hyphen onward and never go empty again.
- Repeat with a book whose ISBN was stored hyphenated by `internal/fb2`
  and one stored bare by `internal/epub`; both must be reachable from a
  hyphenated partial.
- Search a hyphenated title (`Twenty-One`) and confirm it still matches.
- Search `1984-2001` and confirm it behaves as it does today.
