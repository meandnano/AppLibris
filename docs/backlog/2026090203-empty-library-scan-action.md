# Backlog: the empty-library state is missing its scan action

## Problem

Plate 02e of the handoff (`ui-handoff/mockups/Bookshelf Mockups.dc.html`
on the `init` branch, with notes in `SCREENS.md` §02) draws the
empty-library state as four things:

1. the search control dimmed and inert,
2. a dashed box with a serif heading, "No books yet",
3. a line naming **the library path in mono** — "Drop EPUB or FB2 files
   into `/srv/books` and run a scan",
4. a primary **"Scan library"** button.

Items 1 and 2 are built. Items 3 and 4 are not: the page says "Drop EPUB
or FB2 files into your library directory. The next scan picks them up",
which names no path and offers no action.

Both gaps are backing features rather than markup:

- **The path** — `web.Routes` is given `coversDir` but not `LIBRARY_DIR`,
  so the transport genuinely does not know what to print. `cmd/server`
  resolves it (default `./library`); passing it through is the change.
- **The button** — there is no way to trigger a scan. Scanning is a
  goroutine in `cmd/server` driven by `SCAN_INTERVAL` and a startup sweep;
  nothing exposes it, and a `POST /scan` would need to reach the scan loop
  from a handler, which today has no channel to it.

## Why this is backlog, not a plan

It is visible only on a library with zero books, which is the state a
working install passes through once, for as long as the first sweep takes.
Nothing is wrong with what renders there — the words are accurate, just
less helpful than the design intends — and the periodic rescan makes the
button a convenience rather than the only way forward. The path is
arguably the more useful half, and it is the cheaper one.

Worth noting for whoever picks it up: the button is not simply "wire a
handler to `scanner.Scan`". Two sweeps must not run concurrently over the
same database, so a manual trigger needs to poke the existing loop (a
buffered channel the loop selects on, alongside its ticker) rather than
start a second sweep of its own. That is the same shape the send-to-Kindle
worker's poke uses, and the two should look alike.

## Sketch

- Pass `libraryDir` into `web.Routes` alongside `coversDir`, and render it
  in the empty-library block in mono, per the plate.
- For the button: give the scan loop a `notify chan struct{}` (capacity 1,
  non-blocking send), expose `POST /scan` that pokes it and re-renders the
  grid, and show a "scanning …" state while a sweep is in flight. That
  last part needs the loop to publish whether it is running, which it
  currently doesn't.

## Validate before planning

- Check whether the send-to-Kindle step has already introduced a
  poke-a-background-worker pattern (`docs/plans/2026090201-send-to-kindle.md`
  proposes one); if so, follow it rather than inventing a second shape.
- Confirm the plate still shows the button — the handoff is the authority,
  and this note is a snapshot of it.
