# Backlog: metadata read out of a file bypasses every length cap edits enforce

## Problem

`internal/service.normalizeField` caps a person's edit: 1,024 bytes for a
title or an author name, 4,096 for the other scalars, 64 KiB for a
description, 100 author names. `internal/enrich.sanitizeValue` restates
the same numbers for a provider's answer, with CLAUDE.md recording why
they must match: a value the app stores but `normalizeField` would
reject is a field the app can no longer edit, because opening the editor
and pressing Save unchanged fails validation on a value nobody typed.

The third writer, the scanner, applies no cap at all. `internal/epub` and
`internal/fb2` return whatever the file holds, and `createBook` stores
it. A 10 MB `<dc:description>`, or five thousand `<dc:creator>` elements,
go straight into `books`, `authors`, `book_authors` and the FTS index.

Consequences, in order of how soon a person sees them:

- The field cannot be edited, per the rule above.
- The detail page renders the whole description; the grid card renders
  "X and 4999 others".
- The FTS row and index grow by the same amount, for every such book.

## Why this is backlog, not a plan

It takes a malformed or hostile file to trigger, the result is a slow
page and a stuck editor rather than lost data, and the memory side of
"a huge file" is handled by `2026090704`, which caps what a parser will
read in the first place. What is left after that plan lands is a
consistency gap between three writers, not a hazard.

## Re-validate before acting

- Whether `createBook` still stores parser output uncapped.
- Whether `2026090704` has landed, since its caps change what "huge"
  can mean here.

## Sketch

Apply the caps at extraction time, in the scanner, truncating scalars on
a UTF-8 boundary and cutting the author list at the same 100
`normalizeAuthors` uses. Do not add a third copy of the constants: this
is the change that justifies moving them somewhere all three writers can
import, placed the way `SortTitle` is. `internal/service` imports
`internal/storage` and `internal/enrich` imports it too, so the constants
belong in `internal/storage` beside `MetadataField`, with `service` and
`enrich` referring to them rather than restating.
