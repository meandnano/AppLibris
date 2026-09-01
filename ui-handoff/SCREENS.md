# Screen notes

One section per plate in `mockups/Bookshelf Mockups.dc.html`. Each says what
the plate is, what the backend has to provide, and the layout rules that are
easy to get wrong. Values referenced here are defined in `TOKENS.md`.

Plate 01 is already implemented in `library-ui.patch`; treat it as the
reference for handler shaping and template structure.

---

## 01 — Library, default

The home screen. Masthead, pinned search, then the cover grid.

**Data:** `service.ListBooks` — id, title, authors, format, cover path. The
handler flattens authors to one line and maps the cover path to a `/covers/`
URL; the template does no logic.

**Layout:**
- Grid: `auto-fill` over `--card-width`, gap `34px 22px`.
- Each card is cover shelf → serif title → sans author → badge row.
- The shelf is a fixed-height flex box, `align-items: flex-end` and
  `justify-content: flex-start`, so covers bottom-align to a shelf line and
  titles start at the cover's left edge.
- Cover image: `max-height: 100%; max-width: 100%; width: auto`, 1px `--rule`
  border. No fixed aspect ratio.
- Missing cover: dashed box at `aspect-ratio: 0.66`, "no cover" in mono.
- Format badge is mono 9.5px, uppercase, bordered.
- Masthead is sticky, `--bg-raised`, one hairline bottom border. Active nav
  item carries a 2px `--accent` underline.
- Count and scan age sit right-aligned in mono `--fg-faint`.

**Multi-location indicator:** the mockup shows a dotted-underline "2 paths"
marker beside the badge when one book is on disk in more than one place.
`BookSummary` carries no location count yet, so this is drawn but not built —
add the count to the summary before wiring it.

**Paging:** the mockup shows a mono "Loading next 48 of 1,284" line under the
grid. Intended as an htmx `revealed` trigger appending the next page of cards.

---

## 02 — Search, four states

One input above the grid, four rendered outcomes. The input is never itself
re-rendered — only the grid below it is the swap target, so keystrokes are
never lost mid-request.

**Data:** FTS5 over title, sort title and author names. Not built yet.

**States:**
- **a · idle** — placeholder "Search title or author", `/` shortcut hint in a
  bordered mono box. Grid shows everything.
- **b · typing** — accent border plus the `--accent-soft` halo, a spinner at
  the right edge of the input, "filtering …" in mono below. Debounce before
  firing; the spinner is the only motion on the page.
- **c · results** — result count and matched fields in mono ("4 of 1,284 ·
  matched title, author"), a "clear ×" affordance in the input, then the same
  card grid unchanged. Two files of one work stay separate cards — the EPUB and
  the FB2 are separate entries, matching the schema.
- **d · no matches** — dashed box, serif heading quoting the query verbatim,
  one sans line naming the searched fields so the user can reformulate.
- **e · empty library** — distinct from no-matches. Search control dimmed and
  inert, serif heading, the library path in mono, and a primary "Scan library"
  action.

Do not collapse d and e into one template. "Nothing matched" and "nothing
exists" need different actions.

---

## 03 — Book detail, read state

Its own page with a back link, not a panel or modal.

**Data:** full book metadata, authors, file size, format, location count, send
recipients, last send result.

**Layout:**
- Two columns: `300px` cover rail, then `minmax(0, 1fr)` content, gap `44px`,
  page padding `40px 44px 48px`.
- Left rail: cover at `aspect-ratio: 0.66`, then a mono definition list of
  file facts (format, size, locations) with hairline rows.
- Right column, in order: title and author, the send panel, the description,
  then the metadata table.
- **Send sits above the description on purpose** — it is the reason the page
  gets opened. Don't demote it below the prose.
- Title is serif 40px, `max-width: 26ch`, negative tracking.
- Description is serif 17px/1.55 at `max-width: 62ch`.
- Metadata table is a two-column grid of hairline rows: mono label left, sans
  value right. ISBNs render in mono.
- Location count is a dotted-underline accent affordance — the paths are
  revealed on demand, not listed inline.

---

## 04 — Book detail, sparse metadata

The same page for a book with no cover, no author, no ISBN and no description.
This is the common case for FB2 files, so it is a first-class state.

**Rules:**
- Missing fields stay visible as empty rows with an em dash, not hidden. A
  hidden field can't be filled in.
- Missing author and description render as italic `--fg-faint` invitations
  ("Author unknown — click to add", "No description — click to add one"),
  which double as the inline-edit trigger.
- No cover: dashed box, "no cover" in mono, same footprint as a real cover.
- The send panel is unchanged. Sparse metadata never blocks sending.
- Titles may be non-Latin — the layout must not assume Latin metrics or word
  breaks.

---

## 05 — Inline metadata editing

Read view swaps for edit view in place, four plates: title read/editing and
description read/editing.

**Data:** metadata write endpoints returning the read-view fragment on success.
Not built yet.

**Rules:**
- Same box model both ways. Same font, size, line height and padding, so
  nothing shifts on swap. This is the single most important rule here.
- Read view: no input chrome at rest. Hover reveals a `--bg-sunken` ground and
  a mono "edit" label in accent. Negative margins keep the hover ground from
  shifting text.
- Edit view: accent border, `--accent-soft` halo, Save (primary) and Cancel
  (bordered), plus a mono "⏎ save · esc cancel" hint.
- The field keeps its display typography while editing — a title stays serif
  32px in the input. It never becomes a generic form field.
- Description edit is a textarea at `min-height: 108px` so short values don't
  make the box jump.

---

## 06 — Send control, four states

One block, swapped whole by a poll. All four states are drawn at
`min-height: 148px` so the page never jumps between them.

**Data:** a `recipients` table, a `send_log` table, the SMTP integration. None
built yet.

**States:**
- **a · idle** — recipient select (label plus address in mono), primary Send,
  "Not sent yet" in mono, "+ add address".
- **b · sending** — select dimmed and inert, button becomes a bordered accent
  "Sending" with a spinner, a 2px progress rule and mono status ("queued 3s
  ago · handing to SMTP"). This is the polling state.
- **c · delivered** — Send becomes a bordered, de-emphasised "Send again";
  a `--ok` left rule with "Delivered" and the timestamp and address in mono.
  The address is repeated because "delivered" is meaningless without knowing
  where.
- **d · failed** — primary "Retry", an `--err-soft` block with an `--err` left
  rule, and the raw SMTP reply in mono ("552 attachment too large (28.4 MB)").
  Show the real error; a self-hosting user is the one who can fix it.
- **e · add address** — the "+ add address" affordance expands inline into a
  mono field, an Add button and an "esc" hint. Not a dialog.

Status colour is the only colour in this block besides the accent.

---

## 07 — Send history

Built to answer one question: is this book already on the Kindle?

**Data:** `send_log` joined to books and recipients. Not built yet.

**Layout:**
- Four columns: `minmax(0, 2.4fr)` title, `minmax(0, 1.3fr)` recipient,
  `120px` status, `140px` timestamp, gap `24px`, `align-items: baseline`.
- Rows separated by `--rule-faint` hairlines, `16px 0` padding. No zebra
  striping, no card per row.
- Title leads in serif 18px — it is what the user scans for.
- Recipient and timestamp in mono; timestamp right-aligned.
- Status is the only coloured cell: `--ok` Delivered, `--accent` Sending,
  `--err` Failed, `--fg-muted` Queued.
- A failure puts its SMTP reason on a second line under the title in mono
  `--err`, so the history explains itself without a click.
- Scope line in the masthead ("last 30 days") in mono `--fg-faint`.

---

## Cross-screen notes

**Theme.** `data-theme` on `<html>`, persisted in `localStorage`, applied by an
inline script in `<head>` before first paint so a dark-mode reader never gets a
light flash. With no stored value the CSS follows `prefers-color-scheme` on its
own. Toggle lives at the right end of the masthead.

**Mobile.** Responsive CSS, no separate mobile screens. The grid reflows on its
own; one 640px breakpoint shrinks the shelf, card width and padding. Search
stays pinned at the top of the grid, full width. Tap targets are at least 34px.
The detail page's two columns should stack under 700px, cover rail first.

**Fragments.** Every state above is a fragment, sized so its siblings share its
height. Wrap each swappable block in an element with a stable id and swap the
whole thing — that is what keeps the layout still.

**Templates.** Named partials, not base/block inheritance: `ParseFS` puts every
template in one set, so two pages each defining `content` would collide.
`partials.html` holds `document-head`, `site-header` and `site-scripts`; each
page includes them.
