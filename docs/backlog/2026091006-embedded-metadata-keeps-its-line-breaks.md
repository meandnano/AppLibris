# Backlog: embedded metadata keeps line breaks the editor refuses

## Problem

`internal/scanner`'s `capMetadata` bounds embedded metadata to
`storage.Max*` before `createBook` stores it, so no value reaches a metadata
column longer than `internal/service`'s `normalizeField` accepts. Length is
not the only thing that function refuses, though: every field but
description is rejected outright when it contains `\r` or `\n`
(`internal/service/metadata.go`, "This field cannot contain line breaks").

Neither parser prevents one. `internal/epub`'s `first` and `internal/fb2`'s
`authorName` apply `strings.TrimSpace` and nothing else, so a `<dc:title>`
or `<book-title>` wrapped across two lines in the source XML — which is
legal, and which a generator that pretty-prints its output produces — is
stored with the newline intact. `TrimSpace` removes one at either end, so
only a break *inside* the text survives.

That stored value never reaches the validation error, and what happens
instead differs by control:

- **Scalars** (title, publisher, ISBN, …) are edited through
  `<input type="text">`. A browser strips CR and LF from such a value before
  submitting, so Save unchanged succeeds and silently rewrites the field:
  the break disappears and provenance flips to `manual`. The person is not
  told, and the value they saved is not the value they were shown.
- **Authors** use a `<textarea>`, which keeps the break. `normalizeAuthors`
  splits on newlines, so saving unchanged **splits one author into two**.
  The name also already renders as two lines in the editor before any save.

`internal/enrich`'s `sanitizeValue` collapses this for a provider's answer
(`strings.Join(strings.Fields(value), " ")` for every field but
description). Only the scanner's path lacks it.

## Why it is not urgent

It corrupts nothing on its own and blocks nothing: the value renders
acceptably everywhere, since every single-line rendering collapses
whitespace. Reaching either symptom needs a file whose XML wraps a metadata
element's text across lines *and* a person who then edits that field. The
authors split is the more visible of the two and the reason this is worth
doing eventually rather than never.

## Sketch of a fix

Fold the same whitespace collapse into `capValue` for every field but
description, before the length cut rather than after — the collapse can only
shorten the value, and cutting first would let a truncation boundary decide
whether a break survives. Then `docs/notes/scanner.md`'s paragraph can claim
the whole property ("every value in `books` is one the editor accepts")
rather than the length half of it, and CLAUDE.md's `capMetadata` invariant
can say so too.

Re-validate first: check whether `internal/epub` and `internal/fb2` have
since been given a shared normalisation of their own, in which case this
belongs there instead.
