# Backlog: embedded metadata keeps line breaks the editor refuses

## Problem

Found while implementing `docs/plans/completed/2026091003-first-sweep-fidelity.md`,
which closed the length half of the same asymmetry and did not look at
this one.

`internal/scanner`'s `capMetadata` now bounds embedded metadata to
`storage.Max*` before `createBook` stores it, so no value reaches a
metadata column longer than `internal/service`'s `normalizeField` accepts.
Length is not the only thing that function refuses, though: every field but
description is rejected outright when it contains `\r` or `\n`
(`internal/service/metadata.go`, "This field cannot contain line breaks").

Neither parser prevents one. `internal/epub`'s `first` and `internal/fb2`'s
`authorName` apply `strings.TrimSpace` and nothing else, so a
`<dc:title>` or `<book-title>` wrapped across two lines in the source XML —
which is legal, and which a generator that pretty-prints its output
produces — is stored with the newline intact. Opening that book's title
editor and pressing Save unchanged then fails validation on a value nobody
typed, which is exactly the failure the length caps were added to prevent.

`internal/enrich`'s `sanitizeValue` already collapses this for a provider's
answer: `strings.Join(strings.Fields(value), " ")` for every field but
description. Only the scanner's path lacks it.

## Why it is not urgent

It corrupts nothing and blocks nothing. It needs a file whose XML wraps a
metadata element's text across lines *and* a person who then tries to edit
that field; the value renders acceptably in the meantime, since every
single-line rendering collapses the whitespace anyway.

## Sketch of a fix

Fold the same whitespace collapse into `capValue` for every field but
description, before the length cut rather than after — the collapse can
only shorten the value, and cutting first would let a truncation boundary
decide whether a break survives. Then `docs/notes/scanner.md`'s paragraph
can claim the whole property ("every value in `books` is one the editor
accepts") rather than the length half of it, and CLAUDE.md's `capMetadata`
invariant can say so too.

Re-validate first: check whether `internal/epub` and `internal/fb2` have
since been given a shared normalisation of their own, in which case this
belongs there instead.
