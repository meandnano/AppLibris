# Backlog: a description's paragraph breaks reach the column but not the page

## Problem

`books.description` can hold newlines, and the detail page collapses them.

`internal/googlebooks`' `plainText` renders the Volumes API's HTML
description into plain text with `\n\n` between paragraphs, and
`docs/plans/completed/2026090608-googlebooks-live-fidelity.md` justified
the second request partly on that: "the paragraph structure of Defect 4
comes back". It comes back into the column, not onto the screen.

`internal/web/templates/partials.html:367` renders the read view as a
`<span>` inside `.detail__description`, and `app.css` sets no
`white-space` for it — the only two `white-space` declarations in the
stylesheet are `nowrap`, on unrelated selectors. So HTML's ordinary
whitespace collapsing turns every run of newlines into a single space. The
breaks are observable only in the edit textarea, which is a `<textarea>`
and preserves them.

The same applies to any multi-paragraph description, whichever writer
produced it — a hand-typed one included, since `description` is the one
editable field that permits newlines (`internal/service`'s
`normalizeField` rejects CR/LF in the other six).

## Why this is backlog, not a plan

Nothing is lost or wrong: the text is all there, in the right order, with
the right words. It reads as one paragraph instead of three. No data is
corrupted, nothing is blocked, and the page is not visibly broken — a
reader who has never seen the source cannot tell.

It is also a real design decision rather than a one-line fix, which is the
other reason it is not a plan. `white-space: pre-wrap` is the obvious
change and it does more than add paragraph breaks: it preserves *every*
run of whitespace a provider happened to send, including the leading
indentation and stray double spaces that publisher blurbs are full of.
Whether that is an improvement depends on what the corpus actually
contains, and nobody has looked.

## Re-validate before acting

- Whether `.detail__description` still sets no `white-space`.
- Whether any book in a real library actually has a multi-paragraph
  description. Only the Google Books detail endpoint produces them today
  (Open Library's edition descriptions are plain and mostly single-block),
  so the answer depends on how much of the library that provider has
  enriched.

## Sketch

Three options, in ascending cost:

- **`white-space: pre-wrap`** on `.detail__description`. One line. Also
  preserves whatever else the provider sent, per above.
- **Normalise on the way in**, in `internal/enrich`'s `sanitizeValue`:
  collapse runs of spaces and tabs, keep at most two consecutive
  newlines. Then `pre-wrap` renders exactly the structure and nothing
  else. Costs a rule in the sanitiser and applies to values already
  stored only on re-enrichment.
- **Render paragraphs as elements** — split on blank lines in the handler
  and emit a `<p>` each. The most faithful, and the only one that also
  fixes the grid card's excerpt if that ever wants it; also the only one
  that touches the template's markup, so it interacts with the inline
  editor's swap target.
