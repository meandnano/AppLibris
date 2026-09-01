Design me a website mockup:

## App context

A self-hosted, single-user ebook library server. You browse a personal
collection of EPUB/FB2 books and send a title to a Kindle by email. Those
two actions — see what I have, send one to my Kindle — are the entire
point; everything else is secondary. No login, no multi-tenancy, no public
access: it's bound to an internal network and trusts it.

## Audience and environment

One primary user, plus a second person (a family member) as an occasional
send-to-Kindle recipient — never a second *account*, just a second address
in a short list. Used mostly from a desktop/laptop browser at a normal
window size, but should hold up reasonably on a tablet or phone browser too
(same home network, couch use). Design desktop-first, don't break on
narrower viewports.

## Product tone

A personal bookshelf, not a SaaS admin panel. Cover-forward, calm, a little
warm — closer to a nice personal media library than an enterprise dashboard
or a spreadsheet-with-a-UI. Should feel pleasant to browse idly, not just
functional. Open to a light/dark mode pair if it's easy to carry through
the mockups, but that's a nice-to-have, not a requirement.

## Screens to design

### 1. Library / browse view (the home screen)

- A cover-forward grid of books: thumbnail, title, author(s), a small
  format badge (EPUB / FB2).
- A search box at the top that's meant to filter the grid live as you type
  (no separate results page) — show what the "actively filtering" and
  "results" states look like.
- A small indicator on any book that has more than one file location on
  disk (byte-identical duplicates get merged into one entry with multiple
  known paths — the UI just needs to flag that it happened, not manage it).
- The same logical book can legitimately appear as two separate cards if
  it exists in two formats (e.g. an EPUB and a converted MOBI) — each
  card labeled by its own format badge, not merged.
- Needs to look right with a handful of books, and with a library in the
  hundreds-to-low-thousands — assume the grid has to scale, not just demo
  nicely with 6 items.
- Empty states worth mocking: an empty library (nothing scanned yet), and
  a search with no matches.

### 2. Book detail (expanded card, row, or side panel — your call on pattern)

- Larger cover, full metadata: title, author(s), publisher, published
  date, language, ISBN, description, format, file size.
- Metadata fields are inline-editable in place (click a field, it becomes
  an input, save/cancel) — show both the read state and the editing state
  for at least title and description.
- The send-to-Kindle control lives here: a recipient picker defaulting to
  whichever address was used most recently, with adding a brand-new
  address available inline but clearly secondary (with only ever one or
  two saved recipients in practice, this should never feel like a full
  "manage recipients" page).
- Next to the send control, a status area that will be swapped in by
  polling once a send is queued. Mock its four states: idle (nothing sent
  yet / ready to send), sending (in flight), delivered, and failed (with a
  short reason and a retry action).

### 3. Send history (secondary view, lower priority than the two above)

- A simple scannable log: book title, recipient, status, timestamp, and
  failure reason when relevant. Its whole purpose is answering "did I
  already put this on the Kindle" — favor scannability over density.

## Real data available (don't invent fields beyond this)

**Book:** title, sort_title, publisher, published_date, language, isbn,
description, cover image (~400px on the long edge, JPEG, aspect ratio
varies by book — don't assume a fixed aspect), format (`epub` or `fb2`),
file_size, added_at. A book can have zero authors, no cover, no ISBN, no
description — the design needs to hold up with sparse metadata, not just
fully-populated example books.

**Authors:** a list per book (zero, one, or several).

**Recipients:** address, a short label, last_used_at.

**Send log entry:** book, recipient address, status
(`queued`/`sending`/`delivered`/`failed`), timestamps, failure reason.

## Interaction states to cover, even in static mockups

The real build is server-rendered Go templates with htmx doing partial
swaps for exactly three things — search-as-you-type, the send button
swapping into its status indicator, and inline metadata editing. The
mockup doesn't need working JS, but should give a distinct piece of markup
for each state below so wiring htmx up later is a matter of connecting
existing markup, not designing new states mid-build:

- Search: idle box, actively-typing/filtering, results, no-results.
- Send control: idle, sending, delivered, failed+retry.
- Inline edit: read view, edit view, per editable field.

## Out of scope for this round

- Any login/auth screen — there isn't one.
- Metadata-provider/enrichment UI (Open Library / Google Books results,
  provenance display) — not built yet, don't design for it.
- A full recipients management page — the inline add-a-recipient control
  in the send panel is the entire feature.
- Format conversion controls — conversion exists in the data model but
  isn't a user-facing feature yet.
