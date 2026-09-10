# Backlog: an ISBN followed by a space and a digit is lost

## Problem

`storage.NormalizeISBN` matches a maximal run over digits, separators and
`X`, then validates it. A space is one of those run bytes, because an ISBN
may be written `0 306 40615 2`. The consequence is that a digit-led word
after the number joins the run and the whole thing fails to validate:

| identifier | result |
|---|---|
| `978-0-306-40615-7 2nd ed.` | `""` |
| `ISBN 0306406152 2nd edition` | `""` |
| `ISBN-10 0306406152` (no colon) | `""` — the `10` is absorbed |
| `9780306406157 0306406152` | `""` — read as one 23-digit run |
| `ISBN 0306406152, 2005` | `0306406152` — a comma ends the run |
| `ISBN-10: 0306406152` | `0306406152` — a colon ends the run |

Maximality is deliberate and worth keeping: it is what makes `030640615X7`
one refused run rather than a valid ISBN-10 with a stray digit after it. The
space is the part that overreaches.

## Why it is not urgent

It only ever loses an ISBN, never yields a wrong one — the field stays
empty, enrichment still finds the book by `Search`, and a person can type
the number in. Every spelling the common cataloguing tools emit ends the run
with a bracket, comma or colon, so the affected shapes are the handwritten
ones.

## Sketch of a fix

When a maximal run fails to validate, retry its space-delimited prefixes
longest-first before moving past it: `978-0-306-40615-7 2nd` would then
yield the 13-digit prefix. Keep hyphen grouping maximal, since a hyphen
never separates an ISBN from a following word.

Re-validate first, and check the table above against the code as it stands —
the `two ISBNs separated by a space` and `a digit-led word after the ISBN`
cases are pinned in `internal/storage/isbn_test.go`, so a fix must update
them rather than being caught by them.
