# Import a book from a pasted link

## Overview

- A book is often a link before it is a file. Today it must be downloaded to
  the browser's machine and uploaded again. This step lets the library page
  take the link itself, via Cmd/Ctrl+V or a Paste button. The server downloads
  it, stages it through the existing `Stager.Stage`, and lands on the existing
  preview. From the preview on, nothing differs from an upload.
- Cmd/Ctrl+V of a copied **file** on the library page goes through the drop
  path, so pasting a file and pasting a link are one gesture.
- It integrates as one more `io.Reader` source for `Stager.Stage`. Staging,
  the cap, the staging budget, `detectSuffix`, the verdicts, confirm and
  cleanup are unchanged. `docs/notes/import.md` lists "Importing from a
  URL" under Deliberately absent; this step removes that line.
- The main risk is SSRF. The app has no login, so anything that can reach it
  could make it fetch internal addresses: Docker neighbours, a router,
  `169.254.169.254`, or tailnet peers in `100.64/10`. A pasted public link
  can also redirect to any of them. A non-book response is never kept,
  because `detectSuffix` refuses it. Error text and timing would still make
  the app a port scanner. Everything else (parser caps, the cover's media
  type, `nosniff`, the name sanitiser, the staging budget) already holds for
  uploaded bytes and holds here unchanged.

## Context (from discovery)

- **Files and components involved:**
  - `internal/importer/importer.go`: `Stager.Stage(ctx, name, io.Reader)`
    reserves the budget before reading, and removes the temp file when the
    reader fails.
  - `internal/service/import.go`: `StageImport`, `ErrImportDisabled`.
  - `internal/web/import.go`: the upload handler, `uploadRate`, deadline
    sizing, `importFailureLine`.
  - `internal/web/templates/library.html` and `import.html`.
  - `internal/web/static/drop.js` and `internal/web/static/css/app.css`.
- **Related patterns:**
  - SSRF guard: `internal/enrich/cover.go` has `RefusePrivateAddress`,
    `refusedPrefixes`, `translatedPrefixes`, and `coverDialContext` checking
    every connection in `net.Dialer.Control`. It is used by
    `internal/enrich/worker.go`, with the test opt-out in `export_test.go`.
  - Client script: `drop.js` fills the hidden `drop__form` and calls
    `form.submit()`, never fetch or htmx. `import-drop` is included only from
    `library.html`, and only when a `Stager` exists.
  - Test rules (`docs/notes/testing.md`): in-memory
    `httptest.NewTestServer`; a production client handed
    `server.Client().Transport`; `.test` hosts; `synctest` for time; `Start`
    only for a test about the socket itself.
- **Dependencies:** none new. Standard library only (`net/http`, `mime`,
  `net/netip`).

## Development Approach

- **testing approach**: Regular (code first, then tests in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in
  that task
  - write unit tests for new and modified functions
  - add test cases for new code paths, success and error
  - update existing tests if behaviour changes
- **CRITICAL: all tests must pass before starting the next task.** No
  exceptions.
- **CRITICAL: update this plan file when scope changes during
  implementation**
- run `go test ./...` after each change
- keep backward compatibility: no existing route, form or behaviour changes

## Testing Strategy

- **Unit tests:** required for every task, following `docs/notes/testing.md`.
- **Template and handler tests** in `internal/web` cover the markup the
  client relies on:
  - the Paste button and dialog are rendered only when a `Stager` exists;
  - the Import page carries the URL form.
- **No e2e harness:** the project has none, and `drop.js` has no JS tests
  either. The behaviour of `paste-link.js` is checked by hand in the browser
  pane; see Post-Completion.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

- **Public addresses only.** The fetch reuses the cover guard unchanged, so
  DNS rebinding and a redirect to a bare private IP are refused at connect
  time. A book on the LAN is imported by dropping or uploading the file. An
  allowlist env var was rejected: it would be configuration for a case the
  drop already covers.
- **One guard.** The guard moves from `internal/enrich` into a new
  `internal/netguard`, which `internal/enrich` imports back. A check that
  decides what the server may connect to must exist once.
- **Synchronous in the request.** `POST /import/url` downloads inside the
  request, then answers 303 to the preview or renders the upload's 422 page.
  It needs no new stage state and works with JavaScript off. Closing the tab
  cancels the download. A background job with progress was rejected: it
  would need a "downloading" state, polling and its own cancellation, for a
  wait of seconds.
- **One download at a time.** The staging budget bounds bytes at rest. A
  process-wide limit also bounds outbound connections and requests held
  open, which the budget does not.
- **The client:**
  - Cmd/Ctrl+V of a link opens a confirmation `<dialog>` holding a real
    `<form method=post action=/import/url>`.
  - Cmd/Ctrl+V of a file goes through the drop path.
  - The Paste button reads links only, since `navigator.clipboard.readText()`
    cannot read files.
  - The Import page carries the same URL form statically, as the path with
    JavaScript off.
- **Out of scope:**
  - The designer's "Add file" button, and moving the `/` badge into the
    search field. Both came in the same handoff as the Paste button and are
    deferred; the `/` badge stays where it is.
  - Cookies, credentials in the URL, custom headers. A link that needs a
    session is not a direct link to a file.

## Technical Details

### `internal/netguard`

- It contains `RefusePrivateAddress`, `refusedPrefixes`,
  `translatedPrefixes`, and `DialContext(guard func(net.IP) error, timeout
  time.Duration)`, which replaces `coverDialContext`.
- Error strings lose the "cover" wording, since the guard now serves two
  callers.

### `importer.Fetcher` (`internal/importer/fetch.go`)

```go
func NewFetcher(maxSize int64) *Fetcher
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Download, error)

type Download struct {
	Name string        // Content-Disposition, else last URL path segment, else ""
	HTML bool          // Content-Type was text/html
	Body io.ReadCloser // unread; Stage's copyIn counts it
}

type StatusError struct{ Code int }
```

- **Transport:**
  - the dialer is `netguard.DialContext(netguard.RefusePrivateAddress, 10s)`;
  - `TLSHandshakeTimeout` is 10s and `ResponseHeaderTimeout` 15s;
  - `Proxy: nil`, since a proxy set in the environment would move the real
    connection off the address the guard checked.
- **`CheckRedirect`:**
  - at most 5 hops, and each hop must be `http` or `https`;
  - hops to another host are allowed, since download links routinely bounce
    to a CDN and the guard checks every hop's address anyway;
  - it deletes `Referer` from each hop's request, since the previous URL can
    carry a signed token.
- **Headers:** no cookie jar. The `User-Agent` is the string `enrich` uses.
- **Refusals before any body byte is read:**
  - a status other than 200 returns `*StatusError`;
  - `ContentLength > maxSize` returns the importer's existing too-large
    error.
- **`Name`** comes from `mime.ParseMediaType` over `Content-Disposition`,
  which decodes `filename*`. Otherwise it is the last unescaped segment of
  the final URL's path, otherwise `""`. Nothing is sanitised here: the
  existing name sanitiser and the stage-id fallback handle it.
- **Counting the body:** it is counted once, by `Stage`'s `copyIn`, which
  stops at the cap plus one byte. Gzip is decompressed by the transport
  before counting, so a compressed bomb hits the same cap.
- **Tests:** the transport is replaceable only through `export_test.go`,
  the pattern `enrich.Worker`'s guard follows.

### `service.StageURL` (`internal/service/import.go`)

- A nil `Stager` returns `ErrImportDisabled`.
- **Validation** (`url.Parse`): scheme `http` or `https`, a host present, no
  userinfo, at most 2 KiB. A failure returns `ErrUnsupportedLink`.
- **Busy limit:** `downloads chan struct{}` of capacity 1, acquired without
  waiting. If it is taken, the call returns `ErrDownloadBusy`.
- **Flow:** `Fetch`, then `Stager.Stage(ctx, d.Name, d.Body)`, then the
  preview exactly as `StageImport` builds it. `Stage` already cleans up
  after a reader error.
- **Not a book:** when `Stage` refuses the content and `d.HTML` is set, the
  refusal is wrapped so the handler can say "web page".

### `POST /import/url` (`internal/web/import.go`)

- Same-site only, like every state-changing route. It reads one `url` field
  from the urlencoded form.
- It extends its read and write deadlines through `ResponseController` to
  the header time plus `maxSize/uploadRate` plus slack, reusing the upload's
  sizing function. The request context carries the same deadline.
- On success it answers 303 to `/import/{id}`. On failure it renders the
  upload's 422 page with the line below.
- It is registered whether or not importing is enabled, like the other
  import routes.

| Cause | Line |
|---|---|
| `ErrUnsupportedLink` | That isn't a supported link. |
| Too large (length header or count) | existing too-large line |
| `*StatusError` | The server answered 404. (the code) |
| Not a book, `HTML` set | That link opens a web page, not a book file. |
| Not a book otherwise | existing not-EPUB-or-FB2 line |
| Deadline exceeded | The download took too long. |
| `ErrDownloadBusy` | Another link is still downloading. |
| Refused address, DNS, connect, TLS, refused redirect | Couldn't download that link. |

The status code is safe to show because only public hosts are ever reached.
The private-address refusal stays inside the generic line on purpose: naming
it would confirm that an internal hostname resolves.

Each failure is logged at Info with the underlying error and the scheme,
host and path of the requested URL, never its query or fragment.

### Client (`internal/web/static/paste-link.js`)

- **The paste listener** sits on `document` and does nothing when the
  target is an input, textarea or contenteditable element, so the search
  box pastes normally.
  - A pasted file (`clipboardData.files`) goes through the drop path.
    `drop.js` exposes its submit rather than being copied, so both keep one
    set of refusals (several files, over the cap) and sentences. There is no
    dialog, because the preview is the confirmation.
  - Pasted text opens the dialog only when, trimmed, it is a single token
    that `new URL()` parses with `http:` or `https:`. Anything else is
    ignored silently.
- **The dialog** is a `<dialog>` with a real
  `<form method=post action=/import/url>`:
  - it leads with the host ("Download from example.com?");
  - the full URL sits in an editable `url` input;
  - Import is focused, with Cancel beside it, and Escape closes the dialog;
  - it submits with plain `form.submit()`, never fetch or htmx, and sets the
    handing-off state.
- **The Paste button** reads links only, through
  `navigator.clipboard.readText()`. If access is denied or the clipboard
  holds no link, the dialog opens with an empty, focused input. The button
  renders `hidden` and the script reveals it.
- **The hint** reads `⌘V` on Apple platforms and `Ctrl+V` elsewhere, set by
  the script.

### Paste button look

From the designer's handoff (`Bookshelf Mockups.dc.html`, sections 01 and
08), Paste only:

- **Placement:** a control group at the right end of the search toolbar row
  in `library.html`, separated from the search field by a 1px vertical rule
  (`--rule2`, 26px tall) and a 14px gap.
- **Rest:**
  - transparent background, `border: 1px solid var(--rule)`,
    `color: var(--fg2)`, padding `11px 16px`, and a 9px gap between label
    and hint;
  - the label `Paste` is IBM Plex Mono 11px, `letter-spacing: 0.1em`,
    uppercase;
  - the hint is `--fg3`, `letter-spacing: 0.04em`, not uppercased.
- **Title:** `title="Paste a link from the clipboard"`. The handoff's "a file
  or a link" is wrong for the button, which can only read text.
- **Hover:** `border-color: var(--fg)`, `color: var(--fg)`.
- **Handing off:**
  - the hint is replaced by an 11px spinner: `1.5px` border `--rule`,
    `border-top-color: var(--accent)`, `border-radius: 50%`, and the
    existing `@keyframes spin` at `0.7s linear infinite`;
  - the label turns `--fg3`;
  - it is the same state `drop.js` sets as `uploading`, cleared by a bfcache
    `pageshow` and by Escape.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests and documentation
  in this repository.
- **Post-Completion** (no checkboxes): checking the client by hand in the
  browser, which cannot run in `go test`.

## Implementation Steps

### Task 1: Extract the address guard into `internal/netguard`

**Files:**
- Create: `internal/netguard/netguard.go`
- Create: `internal/netguard/netguard_test.go`
- Modify: `internal/enrich/cover.go`
- Modify: `internal/enrich/cover_test.go`
- Modify: `internal/enrich/worker.go`
- Modify: `internal/enrich/export_test.go`

- [x] move `RefusePrivateAddress`, `refusedPrefixes`, `translatedPrefixes` into `internal/netguard`, with error text that names no caller
- [x] replace `coverDialContext` with `netguard.DialContext(guard, timeout)` and use it from `internal/enrich`
- [x] move the guard's tests into `netguard_test.go` unchanged
- [x] confirm the `enrich.Worker` guard opt-out and its real-socket guard tests still pass
- [x] run tests - must pass before task 2

### Task 2: Add `importer.Fetcher`

**Files:**
- Create: `internal/importer/fetch.go`
- Create: `internal/importer/fetch_test.go`
- Create: `internal/importer/export_test.go`

- [ ] implement `NewFetcher`, `Fetch`, `Download`, `StatusError` per Technical Details (transport, redirect policy, no `Referer`, no jar, `Proxy: nil`)
- [ ] implement the name derivation: `Content-Disposition` (`filename*`, `filename`), URL path segment, empty
- [ ] write tests on `httptest.NewTestServer` with the production client handed `server.Client().Transport`: 200 body returned unread, name cases, `HTML` flag
- [ ] write error-case tests: redirect cap, redirect to a non-http scheme, `Content-Length` over cap refused with no body read, non-200 → `*StatusError`, no `Referer` or cookie on a redirected hop
- [ ] write a real-socket (`Start`) test pinning that the production `Fetcher` refuses a loopback server
- [ ] run tests - must pass before task 3

### Task 3: Add `service.StageURL`

**Files:**
- Modify: `internal/service/import.go`
- Modify: `internal/service/service.go`
- Modify: `internal/service/import_test.go`
- Modify: `cmd/server/main.go` (wiring the `Fetcher`, if the service does not build it itself)

- [ ] add `ErrUnsupportedLink`, `ErrDownloadBusy`, the URL validation and the one-download limit
- [ ] implement `StageURL`: `Fetch`, `Stage`, preview; wrap a not-a-book refusal when `HTML` is set
- [ ] write tests for success: a book lands as a preview carrying the fetched name
- [ ] write error-case tests: each validation refusal; nil `Stager`; a second concurrent call refused; an over-cap body without a length ends in the too-large error and leaves the staging directory empty; a slow drip hits the deadline under `synctest`
- [ ] run tests - must pass before task 4

### Task 4: Add `POST /import/url`

**Files:**
- Modify: `internal/web/import.go`
- Modify: `internal/web/web.go`
- Modify: `internal/web/import_test.go`

- [ ] add the handler with deadline extension, 303 and 422 per Technical Details, and register the route
- [ ] extend `importFailureLine` with the new lines from the table
- [ ] log failures with scheme, host and path only
- [ ] write tests: 303 on success; the route refuses a request with no `Sec-Fetch-Site`, like the other import routes
- [ ] write error-case tests: the 422 page with each line of the table; a URL's query string never reaches the log
- [ ] run tests - must pass before task 5

### Task 5: Add the URL form to the Import page

**Files:**
- Modify: `internal/web/templates/import.html`
- Modify: `internal/web/static/css/app.css`
- Modify: `internal/web/import_test.go`

- [ ] add a `<form method=post action=/import/url>` with one `type=url` input named `url` beside the file form
- [ ] style it with the existing import form rules
- [ ] write tests: the Import page renders the URL form when importing is enabled and the explanation, without the form, when it is not
- [ ] run tests - must pass before task 6

### Task 6: Paste a link or a file on the library page

**Files:**
- Create: `internal/web/static/paste-link.js`
- Modify: `internal/web/static/drop.js`
- Modify: `internal/web/templates/partials.html` (the dialog partial)
- Modify: `internal/web/templates/library.html`
- Modify: `internal/web/static/css/app.css`
- Modify: `internal/web/web_test.go`

- [ ] expose the drop's file submit from `drop.js` so a pasted file reuses its refusals and sentences
- [ ] add the confirmation dialog partial: host line, editable `url` input inside a real form, Import and Cancel
- [ ] implement `paste-link.js` per Technical Details: paste listener, file routed to the drop, link to the dialog, the handing-off state
- [ ] include the partial and script from `library.html` only when a `Stager` exists
- [ ] write tests: the library page carries the dialog and script when importing is enabled and neither when it is not; a search fragment (`book-grid`) carries neither
- [ ] run tests - must pass before task 7

### Task 7: Add the Paste button to the library toolbar

**Files:**
- Modify: `internal/web/templates/library.html`
- Modify: `internal/web/static/paste-link.js`
- Modify: `internal/web/static/css/app.css`
- Modify: `internal/web/web_test.go`

- [ ] add the control group (rule plus button) at the right end of the search toolbar row; the button renders `hidden`, with its title and a hint element
- [ ] add the rest, hover and handing-off styles per the Paste button look
- [ ] in `paste-link.js`: reveal the button, set the platform hint, and read the clipboard on click, opening the dialog prefilled or empty
- [ ] write tests: the button is rendered `hidden` with its title only when importing is enabled
- [ ] run tests - must pass before task 8

### Task 8: Verify acceptance criteria

- [ ] verify every requirement in the Overview is implemented
- [ ] verify the edge cases: a private-address link, a link redirecting to a private address, an HTML page, an oversize body, a timeout, a second concurrent download, a pasted file, text pasted into the search box
- [ ] run the full test suite: `go test ./...`
- [ ] run `go vet ./...` and `go test -race ./...` as CI does
- [ ] verify every new function has tests covering success and error paths

### Task 9: [Final] Update documentation

- [ ] `docs/notes/import.md`: rules for the URL route, the fetcher, the busy limit, the refusal lines, the logging, and the paste gestures; remove the Deliberately-absent line
- [ ] `docs/notes/enrichment.md`: the guard lives in `internal/netguard`
- [ ] `docs/notes/web.md`: `POST /import/url` in the route list; the `paste-link.js` inclusion rule
- [ ] README.md: the Importing section covers pasting a link or a file, and public addresses only
- [ ] CLAUDE.md code map: `internal/netguard`
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification** in the browser pane, against `make run`:
- Cmd+V of a link opens the dialog with the host shown. Import lands on the
  preview; Cancel and Escape close the dialog.
- Cmd+V of a file copied in Finder goes straight to the preview, and of two
  files shows the drop's refusal.
- Cmd+V of plain text does nothing. Cmd+V inside the search box pastes
  normally.
- The Paste button with clipboard permission allowed opens the dialog
  prefilled. With permission denied, it opens the dialog empty.
- The handing-off spinner shows during a slow download and clears after a
  bfcache return.
- The button's hint reads `⌘V` on macOS and `Ctrl+V` otherwise.
- Light and dark themes, and phone width with no horizontal scroll.
- With JavaScript off, the Import page's URL form works.

**Security review considerations:**
- Try a link to `http://127.0.0.1/`, `http://169.254.169.254/`, a tailnet
  address, and a public URL that 302s to each. Each should give "Couldn't
  download that link." at the same speed.
