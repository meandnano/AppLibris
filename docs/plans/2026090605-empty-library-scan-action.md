# Step: finish the empty-library state — the path and the scan button

## Position in the sequence

Independent of the enrichment trio. It touches `internal/web`,
`internal/service` and `cmd/server`; nothing it changes is read by
`internal/enrich` or `internal/scanner`.

## Context

`docs/backlog/2026090203-empty-library-scan-action.md`, re-validated:
`web.Routes(svc, coversDir, sendEnabled, enrichEnabled)` still has no
`libraryDir`, and there is still no route that triggers a sweep.

Plate 02e of the handoff (`ui-handoff/mockups/Bookshelf Mockups.dc.html` on
`init`, notes in `SCREENS.md` §02) draws the empty-library state as four
things: the search control dimmed and inert, a dashed box with a serif
"No books yet", **a line naming the library path in mono**, and a primary
**"Scan library"** button.

The first two are built. `partials.html`'s empty block says:

> Drop EPUB or FB2 files into your library directory. The next scan picks
> them up.

— which names no path and offers no action. Both gaps are backing features
rather than markup. The transport genuinely does not know the path, and
nothing anywhere can ask for a sweep.

The backlog's own note about the button is the constraint that shapes this:
two sweeps must never run concurrently over the same database, so a manual
trigger has to **poke the existing loop** rather than start a sweep of its
own. That pattern now exists twice — `sender.Worker.Notify` and
`enrich.Worker.Notify`, both wired to `Service` function fields by
`cmd/server` — so this follows it rather than inventing a third shape.

## Scope

In scope: the library path in the empty block; `POST /scan` poking the
existing scan loop; a "scanning …" state while a sweep is in flight,
which needs the loop to publish whether one is.

Out of scope, with reasons:

- **A scan button anywhere but the empty state.** Plate 02e draws it
  there and nowhere else. A permanent "rescan" control on a populated
  library is a second surface to specify, and the periodic sweep plus the
  filesystem watcher already make it unnecessary — that is the whole
  argument of DESIGN.md's Scanner section.
- **Scan progress — counts, a bar, a file name.** `scanner.Scan` returns
  its `Result` when it is done and reports nothing while running. Making
  it report progress means threading a callback through the walk, which
  is a real change to the scanner for a screen shown once per install.
  "Scanning …" plus a poll that ends when books appear is honest about
  exactly what is known.
- **Reporting a sweep's outcome to the browser** (files scanned, errors).
  Same reason, plus: the outcome of a successful sweep is the grid, which
  the poll already swaps in.
- **A cancel affordance.** There is no user-facing story for abandoning a
  sweep, and `scanCtx` belongs to shutdown.

## Decision 1: the poke is two nil-able function fields on `Service`

`internal/service` cannot import `internal/scanner` — nothing structural
forbids it today, but `Service.Notify` and `Service.NotifyEnrichment` are
function fields precisely because importing the workers would be a cycle,
and a third mechanism beside them would make the pattern read as
accidental. So:

```go
// RequestScan asks the scan loop to sweep now, and ScanRunning reports
// whether one is already in flight. Both are set by cmd/server and are
// nil in tests and wherever no scan loop exists — a nil RequestScan is
// how "this deployment cannot be asked to scan" reaches the transport,
// the same way a nil Notify reports sending being unconfigured.
RequestScan func()
ScanRunning func() bool
```

`Service.RequestScan()` is deliberately not wrapped in a service method
that returns a state, the way `QueueSend` returns a `SendState`: there is
no row, nothing to persist, and nothing to look up afterwards. The service
surface is `Service.Scan(ctx) (ScanState, error)` only in the sense that
the handler needs to know whether a sweep is now running — which
`ScanRunning` answers directly.

A note for whoever builds it: the two fields are read on an HTTP goroutine
and written once at startup, so `ScanRunning` must be backed by something
safe to read concurrently. An `atomic.Bool` in `cmd/server`, closed over,
is the whole implementation.

## Decision 2: `cmd/server` owns the flag, set inside `runScan`

`runScan` is called from two places — the startup sweep and
`periodicScan`'s loop — and both are sweeps a person asking "is it
scanning?" means. Setting the flag inside `runScan` covers both with one
`defer`, and cannot drift the way two call sites would:

```go
func runScan(ctx context.Context, db *storage.DB, libraryDir, coversDir string,
	missingGrace time.Duration, running *atomic.Bool) {
	running.Store(true)
	defer running.Store(false)
	...
}
```

The trigger channel needs no change at all: `scanTrigger` already exists,
already has capacity 1, and `periodicScan` already selects on it. The
watcher pokes it; now the handler does too, through the same non-blocking
send. Two sweeps still cannot overlap, because the scan goroutine is still
the only caller of `scanner.Scan` — which is the property the backlog item
warned about and which this preserves by not touching it.

The rejected alternative was a `scanner.Trigger` type owning the channel
and the flag together. It is tidier in the abstract and it moves
`cmd/server`'s wiring into a package that would then need a test of its
own for behaviour that is four lines of `select`. The existing shape is
already the codebase's answer to "poke a background loop"; matching it is
worth more than tidying it.

## Decision 3: the button re-renders the grid, and the pending state polls it

The empty block lives inside the `book-grid` fragment, so the grid is
already the right swap target — no new fragment, no out-of-band swap.

- `POST /scan` (wrapped in `sameSiteOnly`, like every other
  state-changing route) pokes and answers: an `HX-Request` **without**
  `HX-History-Restore-Request` gets the `book-grid` fragment, everyone
  else a `303` back to `/`. The same progressive-enhancement split the
  send and enrichment POSTs make, for the same reason: the button is a
  plain `<form method="post" action="/scan">` and works with JavaScript
  off.
- While `ScanRunning()` is true and the library is still empty, the block
  renders "Scanning …" with `hx-get="/"` and
  `hx-trigger="load delay:2s"` targeting `#book-grid` — the send
  control's poll shape.
- **Polling stops by construction**, the property the enrichment control
  is built on: a non-empty grid renders cards and no polling attributes,
  and an empty grid with no sweep running renders the idle block and no
  polling attributes. There is no counter and no limit to get wrong.

One consequence worth stating: a sweep that finds nothing returns the
idle empty block, which reads as "it ran and there was nothing to find".
That is correct and is the common case for a misconfigured `LIBRARY_DIR`
— which is exactly why the path line beside it earns its place.

## Decision 4: the path is displayed as configured, not resolved

`LIBRARY_DIR` defaults to `./library`, and in the container it is whatever
was mounted. Render the string `cmd/server` resolved, not
`filepath.Abs` of it: the person reading the page is looking at either a
compose file or a shell they launched it from, and an absolute path
computed against the container's `WORKDIR` names a location that does not
exist on the machine they would act on.

It is HTML-escaped by `html/template` like every other value, and rendered
in mono per the plate.

## Changes

- `cmd/server/main.go`: an `atomic.Bool` beside `scanTrigger`; `runScan`
  takes and sets it; `svc.RequestScan` and `svc.ScanRunning` wired to a
  non-blocking send on `scanTrigger` and to the flag; `libraryDir` passed
  to `web.Routes`.
- `internal/service`: the two function fields, documented as nil-able.
- `internal/web/web.go`: `Routes` takes `libraryDir`; `POST /scan`
  registered under `sameSiteOnly`.
- `internal/web/library.go` (wherever `libraryPage` lives): `LibraryPath`
  and `Scanning` fields; the handler sets them; a `scanHandler`.
- `internal/web/templates/partials.html`: the empty block gains the mono
  path line, the button, and the scanning variant.
- `internal/web/static/css/app.css`: styling for the two new lines. If
  `docs/plans/2026090607-button-system-consolidation.md` has landed, the
  button is `.button .button--primary` and costs no new CSS; if not, this
  step uses `.button --primary` as-is and does not add a fourth system.

## Tests

`internal/web`:

- The empty-library page renders the configured path, and it is escaped
  (pass a `libraryDir` containing `<`).
- A non-empty library renders neither the path nor the button — the empty
  block is not reached.
- `POST /scan` with `HX-Request` returns the `book-grid` fragment; without
  it, a `303` to `/`.
- `POST /scan` with `HX-Request` **and** `HX-History-Restore-Request`
  returns the full page, not the fragment — the same rule search's split
  follows, and the one a copy-paste of the send handler gets wrong.
- `POST /scan` calls `RequestScan` exactly once.
- A cross-site `Sec-Fetch-Site` is rejected, like the other POSTs.
- With `RequestScan` nil, `POST /scan` answers `503` with the block rather
  than `404` — a stale open tab gets an explanation, matching the send and
  enrich routes.
- **Polling attributes appear only in the scanning state**, and a grid
  with books carries none. This is the "stops by construction" property
  and it is invisible until it spins forever.

`cmd/server`: `runScan` sets and clears the flag, including on the error
return — a sweep that fails must not leave the page claiming it is still
running.

## CLAUDE.md

`internal/web`'s paragraph on the empty-library state currently says the
"Scan library" button and library path are "the one part of that plate not
built, tracked in `docs/backlog/2026090203-…`". That sentence is replaced
by what was built. `cmd/server`'s paragraph gains the running flag and the
fact that `scanTrigger` now has two pokers. `internal/service`'s paragraph
gains the two fields beside `Notify`/`NotifyEnrichment`, with the
"function field, not an interface, because importing the workers would be
a cycle" reasoning noted as shared.

## DESIGN.md (on `init`)

The Web UI section's status notes do not enumerate empty states, so
nothing there is now wrong. One clause is worth adding to the Scanner
section: a sweep can be asked for from the UI, and it is the same sweep on
the same goroutine — the manual trigger changes *when* a sweep happens and
never *what* one does, which is already how that section describes the
watcher.

## Verification

- Start against an empty `LIBRARY_DIR`. The empty block names the
  configured path in mono and shows a primary "Scan library" button.
- Drop an EPUB in, press the button: the block becomes "Scanning …", then
  the grid appears without a reload, and the polling stops (confirm in the
  network tab that requests to `/` cease).
- Press the button with nothing in the directory: it returns to the idle
  empty block, not a spinner that never resolves.
- Disable JavaScript and press the button: the page navigates and lands on
  `/` with the same states.
- Confirm two sweeps never overlap by pressing the button repeatedly
  during a slow sweep (a large library, or a `LIBRARY_DIR` on a slow
  mount) and watching the `scan starting` / `scan complete` log pairs.
