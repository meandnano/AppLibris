# Backlog: self-host the web fonts

## Problem

`internal/web/templates/partials.html` pulls three typefaces from Google's
CDN:

```html
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:...&family=IBM+Plex+Sans:...&family=Newsreader:...">
```

This sits awkwardly against DESIGN.md's constraints. The app "ships as a
single container with no external process dependencies", is bound to an
internal network, and embeds its templates, CSS and JS via `go:embed`
specifically so there is no build step and nothing to fetch. The fonts are
the one asset that breaks that: a machine with no route to the internet —
a plausible setup for a deliberately LAN-only book server — blocks on a
render-blocking stylesheet request until it times out, then falls back to
the local stack in `--serif` / `--sans` / `--mono`.

It is also the only outbound request the browser makes on the user's behalf,
to a third party, for an app whose design explicitly assumes no external
dependencies.

## Why this is backlog, not a plan

It degrades gracefully. The CSS already declares real fallback stacks
(`Georgia`, `system-ui`, `ui-monospace`), so an offline box gets a
perfectly usable page in system fonts — visibly plainer than the mockups,
but not broken. On a normal home network with internet access, which is the
common case, it works exactly as designed and the fonts are likely already
in the browser's cache from another site.

So this is a "the design says X and the code does Y" inconsistency with a
mild real-world cost, not a defect anyone will hit as a failure.

## Sketch

Download the WOFF2 files for the weights actually used — `IBM Plex Sans`
400/500/600, `IBM Plex Mono` 400/500, and `Newsreader` as a variable font —
into `internal/web/static/fonts/`, and add `@font-face` rules to `app.css`
pointing at `/static/fonts/…`. The existing `//go:embed static` directive
picks the directory up with no change, and the existing `/static/` route
serves it.

Two things to get right:

- **`font-display: swap`** on each face, so text paints in the fallback
  immediately rather than staying invisible while the font loads. Without
  it, self-hosting can look *worse* than the CDN on a cold cache.
- **Subsetting.** The full Newsreader variable font is a few hundred KB.
  Latin-only subsets cut that substantially, but book titles in a personal
  library are exactly the content likely to contain Cyrillic, accented
  Latin, or CJK — the FB2 files DESIGN.md mentions are overwhelmingly
  Russian-language. Do not subset to Latin without checking the actual
  collection, or titles will render in a mismatched fallback face.

Weigh the repository cost: several hundred KB of binary assets committed to
git, against removing the last external dependency. For a project whose
stated constraints are this explicit, that trade is probably worth making —
but it is a judgement call, which is why it is written down rather than
done.

## Validate before planning

- Check the actual weights referenced by `app.css` before downloading —
  the `css2` URL requests `IBM Plex Sans` 400/500/600 and `IBM Plex Mono`
  400/500 today, but the stylesheet may not use all of them, and shipping
  unused weights is pure cost.
- Check the licence terms ship with the fonts (both families are OFL;
  include the licence file).
- Confirm the collection's character coverage before choosing a subset.
