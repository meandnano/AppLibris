# Design tokens

Editorial direction: hairline rules instead of shadows, a serif for anything a
human wrote, mono for anything the machine is saying. Covers supply all the
colour; the interface stays near-neutral with one warm accent.

Everything below is expressed as CSS custom properties in `tokens.css`. Use the
variables, not the literals — dark mode is the same token names redeclared, so
hard-coded hex values break it.

## Colour

### Light (default)

| Token | Value | Use |
| --- | --- | --- |
| `--bg` | `#fbfaf8` | Page ground. Warm off-white, not pure white. |
| `--bg-raised` | `#ffffff` | Masthead, cards, panels sitting on the page. |
| `--bg-sunken` | `#f4f2ee` | Inset areas: the send panel, hover ground on editable fields. |
| `--fg` | `#17140f` | Titles and primary text. Warm near-black, never `#000`. |
| `--fg-muted` | `#57534b` | Authors, body copy, secondary labels. |
| `--fg-faint` | `#8d877d` | Machine text, counts, placeholders, "no cover". |
| `--rule` | `#ded9d1` | Structural hairlines: panel borders, cover borders, table rows. |
| `--rule-faint` | `#eae6e0` | Internal dividers inside a panel. |
| `--accent` | `#8a5a3c` | Send button, active nav underline, focus ring, links. |
| `--accent-soft` | `#f3ebe4` | The 3px focus halo behind a focused input. |

### Dark

Same names, redeclared. Applied via `prefers-color-scheme` by default and
overridden by `[data-theme="light"|"dark"]` on `<html>`.

| Token | Value |
| --- | --- |
| `--bg` | `#121110` |
| `--bg-raised` | `#1a1817` |
| `--bg-sunken` | `#221f1d` |
| `--fg` | `#f1ede5` |
| `--fg-muted` | `#a9a29a` |
| `--fg-faint` | `#7b756c` |
| `--rule` | `#322e2a` |
| `--rule-faint` | `#262322` |
| `--accent` | `#c98a5f` |
| `--accent-soft` | `#2b211a` |

The dark accent is lighter than the light one — `#8a5a3c` on `#121110` fails
contrast for text. Don't reuse one value for both.

### Status colours

Used only for send state. Status is the only place colour carries meaning, so
nothing else in the UI may use these.

| Token | Light | Dark | Use |
| --- | --- | --- | --- |
| `--ok` | `#3f6b52` | `#7fae90` | Delivered. |
| `--err` | `#9a3b34` | `#d98b81` | Failed, SMTP error text. |
| `--err-soft` | `#f7ece9` | `#2c1e1c` | Ground behind a failure message. |

Queued and Sending use `--fg-muted` and `--accent` respectively — no separate
tokens.

## Type

Three families, loaded from Google Fonts with full local fallbacks so a
self-hosted instance with no outbound network degrades rather than breaks.

| Token | Stack | Use |
| --- | --- | --- |
| `--serif` | Newsreader, Georgia, "Times New Roman", serif | Book titles, descriptions, empty-state headings. Weight 400 only. |
| `--sans` | "IBM Plex Sans", system-ui, -apple-system, "Segoe UI", sans-serif | Interface text: authors, buttons, labels, body copy. |
| `--mono` | "IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, monospace | Anything the machine says: counts, timestamps, file sizes, SMTP codes, ISBNs, field labels, badges, keyboard hints. |

### Scale

| Role | Family | Size / line-height | Notes |
| --- | --- | --- | --- |
| Detail page title | serif | 40px / 1.08 | `letter-spacing: -0.015em`, `max-width: 26ch`. |
| Empty-state heading | serif | 21–26px / 1.2 | |
| Grid card title | serif | 16.5px / 1.25 | `text-wrap: pretty`, `overflow-wrap: anywhere`. |
| History row title | serif | 18px / 1.25 | |
| Description body | serif | 17px / 1.55 | `max-width: 62ch`. |
| Body / interface | sans | 15px / 1.5 | Base. |
| Metadata values | sans | 13.5px | |
| Card author | sans | 12.5px | `--fg-muted`. |
| Section label | mono | 10px | `letter-spacing: 0.16em`, uppercase, `--fg-faint`. |
| Button label | mono | 11px | `letter-spacing: 0.1em`, uppercase. |
| Machine text | mono | 11–12px | Counts, timestamps, errors. |
| Format badge | mono | 9.5px | `letter-spacing: 0.1em`, uppercase, 1px `--rule` border, 2px 5px padding. |

Mono at these sizes is always uppercase with tracking. Sans is never uppercase.
Serif is never bold, never tracked, never uppercase.

## Geometry

| Token / rule | Value |
| --- | --- |
| `--shelf-height` | `210px` desktop, `168px` under 640px |
| `--card-width` | `148px` desktop, `132px` under 640px |
| Grid | `repeat(auto-fill, minmax(var(--card-width), 1fr))`, gap `34px 22px` |
| Page | `max-width: 1500px`, padding `32px` desktop / `16px` mobile |
| Panel padding | `22px 24px` |
| Input padding | `10px 14px` |
| Button padding | `12px 22px` primary, `8px 14px` compact |
| Border radius | `0` everywhere, except the theme toggle pill (`999px`) |
| Focus ring | `1px solid var(--accent)` + `box-shadow: 0 0 0 3px var(--accent-soft)` |
| Minimum tap target | `34px` |

No shadows, no radii, no gradients. Elevation is expressed by a hairline and a
background step, nothing else.

## Layout rules that matter

**Cover shelf alignment.** Covers vary in aspect ratio, so each card's cover
hangs from a fixed-height box (`--shelf-height`), bottom-aligned within it and
left-aligned within the card. The grid gets a shelf line across each row while
every title stays flush with its own cover's left edge. Set
`max-height: 100%; max-width: 100%; width: auto` on the image — never a fixed
aspect ratio, which would crop or letterbox real covers.

**Missing covers** get a dashed `--rule` box at `aspect-ratio: 0.66` with the
words "no cover" in mono, centred. They occupy the same shelf slot; the grid
never collapses around them.

**Constant swap height.** Every state of a swappable block is drawn at the same
height as its siblings — all four send states, both inline-edit modes. An htmx
swap must not move the page. Where content differs in length, the container
carries a `min-height`.

**Read/edit parity.** An editable field's read view and edit view use the same
box model: same padding, same font, same line height. The read view shows
`--bg-sunken` on hover with an "edit" affordance in mono; the edit view adds
the accent border and focus halo. Nothing shifts on click.

**Grid reflow.** The grid is `auto-fill` over a fixed card width, so it goes
from desktop to phone with no breakpoint. The single 640px breakpoint only
shrinks the shelf, the card width and the page padding.

**Search stays put.** The search input is pinned above the grid and is never
itself re-rendered — only the grid below it is the swap target. On mobile it
stays at the top of the grid, full width.
