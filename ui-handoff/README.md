# Bookshelf UI handoff

Everything needed to translate the interface mockups into Go templates and CSS.

## Contents

| Path | What it is |
| --- | --- |
| `mockups/Bookshelf Mockups.dc.html` | The mockups themselves — seven labeled plates, every state side by side. Open in a browser. Header button toggles dark mode. |
| `mockups/support.js` | Runtime the mockup file needs. Keep it next to the HTML. |
| `TOKENS.md` | Colour, type, spacing and border values, with the reasoning. Read this first. |
| `tokens.css` | The same values as a ready-to-use stylesheet. Drop-in for `internal/web/static/css/`. |
| `SCREENS.md` | Per-plate build notes: what each state is, what it needs from the backend, layout rules that matter. |
| `original-brief.md` | The brief the mockups were designed against. |
| `library-ui.patch` | The library-grid plate already translated — Go templates, handler view model, `/covers/` serving, tests. Apply with `git apply`. Reference implementation for the rest. |

## How to use this

1. Read `TOKENS.md`. Every value in the mockups comes from that list; nothing is ad hoc.
2. Copy `tokens.css` in, or take the `library-ui.patch` version of `app.css` which already contains it.
3. For each screen you're building, read its section in `SCREENS.md`, then open the mockup file and look at the plate.
4. Follow the patterns the patch establishes: shape data in the handler into a per-page view model, keep templates logic-free, name partials rather than using base/block inheritance.

## What is and isn't implemented upstream

Only plate 01 (library grid, plus its empty state) is in `library-ui.patch`. The rest are designed but need backing features first:

- **Search** (plate 02) — needs FTS5 over title, sort title and author names.
- **Book detail** (plates 03, 04) — needs a detail route and full metadata reads.
- **Inline editing** (plate 05) — needs metadata write endpoints returning HTML fragments.
- **Send to Kindle** (plates 06, 07) — needs a `recipients` table, a `send_log` table and the SMTP integration.

The mockups are drawn as static states on purpose: each one is a fragment an htmx swap can target, so wiring them up is connecting markup, not designing it.

## Known gap

Covers in the mockups are striped placeholders. They stand for real cover images the scanner extracts; nothing about the layout depends on their content, only on the shelf-alignment rule in `TOKENS.md`.
