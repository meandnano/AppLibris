# Backlog: a partially typed hyphenated ISBN matches nothing

## Problem

Search matches word prefixes as you type — that is the whole point of the
300ms debounce — and it matches a *complete* ISBN in either punctuation,
which is what `2026090106`'s review round fixed: `SanitizeFTSQuery`
normalizes an ISBN-shaped whole query, and `syncBookFTSTx` indexes
`replace(replace(b.isbn, '-', ''), ' ', '')`, so the index holds one bare
token however the source file punctuated it.

Both halves of that fix key on the *finished* string. `SanitizeFTSQuery`
takes its ISBN path only when the whole query is 10 or 13 characters once
hyphens and spaces are stripped. So every intermediate state of typing a
hyphenated ISBN takes the ordinary per-word path instead:

| typed so far | becomes | matches a book whose ISBN is `9780857059985`? |
|---|---|---|
| `978085` | `"978085"*` | yes — a prefix of the one indexed token |
| `978-0-85705` | `"978-0-85705"*` | **no** |
| `978-0-85705-998-5` | `"9780857059985"*` | yes — 13 characters once stripped, so the ISBN path takes it |

The middle row is worth spelling out, because the reason is not the one it
looks like. `strings.Fields` splits on whitespace only, so the hyphenated
input stays a single token and is quoted whole — it does not become three
AND-ed prefix terms. Inside those quotes `unicode61` still tokenizes on
the hyphens, so FTS5 reads it as the *phrase* `978 0 85705`, and the
single indexed token `9780857059985` does not contain that phrase.

The result is a search that goes empty mid-typing and then fills back in
on the last character. Typing the unpunctuated form works the whole way
through, and so does pasting either form whole. Verified against the real
index: `978085` finds books stored in both punctuations, `978-0-85705`
finds neither, and the complete ISBN in either form finds both.

## Why this is backlog, not a plan

Nothing is wrong with the data, nothing is blocked, and the finished query
in both punctuations — the case a user actually reaches by pasting an ISBN,
which is how ISBNs are usually entered — works. What's left is a transient
empty state while typing one specific punctuated input by hand, on a
feature whose primary use is title and author. Nobody hand-types thirteen
digits with hyphens into a search box often enough for this to rank above
the unbuilt features in DESIGN.md.

It is also not obviously worth the complexity it invites: the fix touches
the one function that currently has a flat, provable contract ("no input
can reach MATCH invalidly"), and every option below widens what that
function decides.

## Sketch

Not a decision — the options, with what each costs:

- **Strip separators from any digit-and-hyphen-shaped token** before
  quoting, rather than only from a whole query of ISBN length. Simplest,
  but it changes what a hyphen means for every query, including
  hyphenated titles and double-barrelled author names, where splitting is
  the behavior that is wanted.
- **Take the ISBN path on a whole query that is only digits, hyphens and
  spaces, of any length**, treating it as a prefix of a normalized ISBN.
  Narrower — a query containing a letter is untouched — and it keeps the
  rule "an ISBN-shaped query is an ISBN query", just without the length
  test. Currently the most promising.
- **Index both forms**: add the raw `books.isbn` alongside the normalized
  one in the FTS `isbn` column. Makes hyphenated prefixes match hyphenated
  *storage* only, so it does not actually fix the cross-format case this
  is a sequel to. Mentioned to be ruled out.

## Validate before planning

- Re-read `SanitizeFTSQuery` first: if a later step has already widened
  the ISBN branch (the send-to-Kindle and detail-page steps do not touch
  it, but a metadata-editing one might), this may already be fixed or
  reshaped.
- Confirm the table above against the current index rather than trusting
  it — it was derived by reading `syncBookFTSTx` and the `unicode61`
  tokenizer's hyphen handling, and one throwaway query settles it.
- Check what a hyphenated *title* query does under whichever option is
  chosen, since that is the behavior any widening puts at risk.
