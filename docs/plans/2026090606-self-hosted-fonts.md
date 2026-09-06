# Step: self-host the web fonts

## Position in the sequence

Independent. It touches one template, one stylesheet and a new directory of
binary assets, and shares no code with anything else planned.

## Context

`docs/backlog/2026083116-self-hosted-fonts.md`, re-validated —
`partials.html` still pulls three typefaces from Google's CDN:

```html
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500&family=IBM+Plex+Sans:wght@400;500;600&family=Newsreader:opsz,wght@6..72,300..600&display=swap">
```

DESIGN.md's Constraints say the app "ships as a **single container** with
**no external process dependencies**" and "no JS build step". Templates,
CSS and JS are embedded via `go:embed` precisely so there is nothing to
fetch. The fonts are the one asset that breaks it: a machine with no route
to the internet — a plausible setup for a deliberately LAN-only book server
— blocks on a render-blocking stylesheet request until it times out, then
falls back to the local stacks in `--serif` / `--sans` / `--mono`. It is
also the only outbound request the browser makes on the user's behalf, to a
third party, for an app whose stated assumption is a trusted internal
network.

It degrades gracefully, which is why it was backlog. It is being done now
because the trade is decided: **remove the dependency, accept the repo
weight, ship the full charset.**

## Scope

In scope: WOFF2 files under `internal/web/static/fonts/`, `@font-face`
rules in `app.css`, the three `<link>` tags removed, licences committed.

Out of scope, with reasons:

- **Trimming unused weights.** See Decision 2 — this step ships exactly
  what the `css2` URL requests today, so rendering is unchanged. Deciding
  a weight is unused is a separate, measurable change and mixing it in
  would make "did self-hosting change how it looks?" unanswerable.
- **`unicode-range` subsetting.** See Decision 3.
- **A build step to generate subsets.** DESIGN.md forbids one, and it
  would be needed to maintain any subset over time.
- **Changing the fallback stacks.** They are what makes this safe to get
  wrong, and they stay.
- **A separate cache policy for `/static/fonts/`.** See Decision 4.

## Decision 1: full charset, not Latin-only

The backlog flagged this as the judgement call and it is the one that
matters most here. Book titles in a personal library are exactly the
content likely to contain Cyrillic or accented Latin, and DESIGN.md notes
the FB2 files are overwhelmingly Russian-language. A Latin-only subset
would render Russian titles in a mismatched fallback face **mid-page**,
beside Latin titles in the real one — visibly worse than either the CDN
today or shipping no fonts at all.

So: for each family, ship every subset the foundry publishes that the
collection could plausibly need — at minimum `latin`, `latin-ext`, and
`cyrillic`/`cyrillic-ext` where the family has them.

**One thing to check before writing any CSS, because it may make half of
this moot:** the three families do not have the same coverage. IBM Plex
Sans and IBM Plex Mono ship Cyrillic. **Newsreader may not** — and
Newsreader is `--serif`, the face book titles are set in. If it has no
Cyrillic, then Russian titles already fall back to Georgia today, the CDN
is already not serving them, and self-hosting changes nothing for them.
That is a fine outcome, but it must be *known* rather than assumed, because
it is the difference between "we shipped what was available" and "we shipped
a subset and broke half the library". Record what each family actually
covers in the commit message.

The cheap way to know what the collection needs, rather than guessing:

```sql
-- Titles and author names outside plain ASCII, by what they contain.
SELECT title FROM books WHERE title GLOB '*[^ -~]*' LIMIT 50;
SELECT name  FROM authors WHERE name GLOB '*[^ -~]*' LIMIT 50;
```

Run it against the real library and look at what comes back before
choosing files.

## Decision 2: ship exactly the weights the current URL requests

The `css2` URL asks for IBM Plex Sans 400/500/600, IBM Plex Mono 400/500,
and Newsreader as a variable font over `wght 300..600` with optical sizing
`opsz 6..72`. Ship all of them.

It is tempting to trim, and the stylesheet appears to invite it:
`app.css` sets `font-weight` in exactly four places and every one of them
is `400`. But that is not evidence the other weights are unused — it is
evidence they are reached through **UA defaults**. Headings and `<strong>`
are bold by the user agent; the four explicit `400`s are the places that
were deliberately reset *away* from bold, which means every heading the
author did not reset is asking for 700 and being served the nearest
available face.

Reproducing today's rendering exactly means serving the same set of faces,
so the browser makes the same choice. Deciding which are genuinely reached
means auditing every heading in four templates against the cascade, which
is a typography change wearing a bundle-size costume. Ship the set; if
someone later wants it smaller, that is a change with a before-and-after
screenshot attached.

The variable Newsreader file is the right form for the same reason: it is
what is served today, and a 300..600 axis in one file is smaller than four
static instances would be.

## Decision 3: one file per face, no `unicode-range` splitting

Google's `css2` response splits each family into a dozen `@font-face`
blocks with `unicode-range` descriptors, so a Latin-only page downloads
only the Latin file. Reproducing that means committing a dozen files per
family and hand-writing the ranges, and keeping them correct as the fonts
are updated.

Take the whole-family files instead. The cost is real — a first page load
fetches Cyrillic outlines a Latin-only library never renders — and it is
bounded and one-time, on a LAN, against a browser cache. The saving is a
directory of two or three files per family instead of thirty, no ranges to
transcribe, and a stylesheet a person can read.

If the total lands somewhere uncomfortable, the lever to pull is Decision 2
(fewer weights), not this one: splitting by range is where the maintenance
burden is.

**`font-display: swap` on every face**, without exception. Without it a
self-hosted font can look *worse* than the CDN on a cold cache — text stays
invisible during the block period instead of painting in the fallback —
which would be the exact opposite of this step's purpose.

## Decision 4: the existing static cache policy, unchanged

`internal/web` gives embedded static assets a content-derived `ETag` plus a
five-minute `max-age`, deliberately short so a deploy is picked up
promptly. Fonts inherit it. That means a handful of conditional GETs
returning 304 every five minutes per open tab.

Leave it. One policy for `/static/` is worth more than a special case, the
extra requests are a few hundred bytes on a local network, and the `ETag`
means the font bytes themselves are sent once. A longer `max-age` for
fonts specifically would be right if these were served over the internet;
they are not.

Worth noting for whoever builds it: `buildStaticETags` reads every embedded
asset into memory at startup to hash it. Adding a few hundred KB of fonts
makes that a few hundred KB of transient allocation, once, at boot. Not a
problem — stated so nobody rediscovers it as one.

## Changes

- `internal/web/static/fonts/`: the WOFF2 files, plus `OFL.txt` for each
  family (both IBM Plex and Newsreader are SIL Open Font License; the
  licence file ships with the fonts and must be committed with them).
- `internal/web/static/css/app.css`: `@font-face` rules at the top, above
  the `:root` token block, each with `font-display: swap`, the correct
  `font-weight` (a range for the variable face), `font-style: normal` and
  a `src` of `url("/static/fonts/…") format("woff2")`.
- `internal/web/templates/partials.html`: the three `<link>` tags removed.
  Nothing replaces them — the fonts are declared in the stylesheet the page
  already loads.
- No Go change. `//go:embed static` already takes subdirectories, and the
  `/static/` route already serves whatever is in the tree. (One caveat to
  respect rather than work around: the bare `//go:embed static` form skips
  files whose names begin with `.` or `_`. Font filenames must not.)

## Tests

There is not much here a Go test can assert, and pretending otherwise
would be worse than saying so. What is worth pinning:

- Every `url(…)` in `app.css` under `/static/fonts/` resolves to a file
  present in the embedded FS. A small test walking the stylesheet for font
  URLs and calling `staticFS.Open` on each catches the whole class of
  failure this step can actually produce — a renamed or forgotten file,
  silently falling back to Georgia in production.
- `partials.html` contains no `fonts.googleapis.com` or `fonts.gstatic.com`
  reference. One `strings.Contains` assertion over the embedded template,
  which is what stops the tags coming back in a later merge.
- The existing static-asset tests (ETag, `Cache-Control`, no directory
  listing) keep passing, now with a font path as one of the cases.

## CLAUDE.md

`internal/web`'s paragraph describes CSS/JS embedded via `go:embed` with
"no build step". Add the fonts and the two decisions a reader would
otherwise undo: the full charset (and why a Latin subset is wrong for this
collection specifically), and that the shipped weight set mirrors the old
CDN request rather than the weights `app.css` names explicitly — the four
`font-weight: 400` rules are resets, not an inventory.

## DESIGN.md (on `init`)

The Constraints section's "no external process dependencies" now holds
without qualification for the browser as well as the server. Worth one
sentence in the Web UI status note, since the previous state was a known
exception rather than an oversight.

## Verification

- Load every page (library, a book, `/history`) with the network throttled
  and DevTools blocking `fonts.googleapis.com` and `fonts.gstatic.com`.
  Nothing requests them; type renders in the intended faces.
- Compare before/after screenshots of the library grid and a book detail
  page at the same width, in both light and dark. Headings, card titles and
  the mono lines must be indistinguishable — this is what Decision 2 buys
  and the only way to confirm it.
- Load a book whose title is Cyrillic and one whose title has accented
  Latin. Confirm which face each renders in, and that it matches what
  Decision 1's coverage check predicted.
- Pull the machine off the network entirely and reload. The page paints
  immediately with no blocked request.
- Check the total added repository size and record it in the commit
  message. If it is far from what Decision 3 assumed, that is a reason to
  revisit Decision 2 before merging, not after.
