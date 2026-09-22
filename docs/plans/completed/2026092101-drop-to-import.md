# Drop a book on the library page to import it

## Context

Importing today means opening the Import page, choosing a file and pressing
Upload. The library grid is where a person already is, and dragging a file
there from a file manager is the gesture they reach for first. The goal: a
file dropped anywhere on the library page runs the first phase of import
(stage and verdict) and lands on the same preview page `/import/{id}` the
Import page's own upload lands on — Import button for `new`, the warning for
`title-match`, the link to the existing book for `exists`.

The route already does exactly this for a plain, JavaScript-free form post:
`importUploadHandler` (`internal/web/import.go`) answers a non-htmx upload
with a 303 to `/import/{id}`, and a refusal with a 422 carrying the whole
Import page, the error line and the file input. So the drop needs no server
route and no second preview UI: it fills a hidden, plain upload form with the
dropped file and submits it natively. No fetch, no htmx request, no modal.

Decisions settled with the user:

1. One file at a time. Several files, or a folder, are refused on the page
   without uploading anything.
2. The size cap is checked before the upload starts, with the refusal
   sentence composed server-side. The server's own count stays the real
   check; the pre-check exists because an over-cap body can lose its 422
   while megabytes are still in flight (see `renderImportRejection`).
3. `exists` keeps today's preview page, not a redirect to the book.
4. Library page only. Book, history and import pages are not drop targets.
5. A discoverability line in the empty-library state and in the Import
   form's hint.

## Design

**Markup** — a new `import-drop` partial in
`internal/web/templates/partials.html`, included only from `library.html`
(never from `book-grid`, which is the htmx search/paging fragment, so it
survives every grid swap the way `search-bar` does) and rendered only when
`libraryPage.ImportDrop` is non-nil:

```html
<div class="drop" data-import-drop data-max-bytes="{{.MaxBytes}}">
  <form class="drop__form" method="post" action="/import/file"
        enctype="multipart/form-data" hidden>
    <input type="file" name="file">
  </form>
  <div class="drop__overlay" data-drop-overlay hidden>
    <p class="drop__prompt" data-drop-state="ready">Drop to import</p>
    <p class="drop__prompt" data-drop-state="uploading" hidden>Uploading &hellip;</p>
  </div>
  <p class="drop__error" role="alert" data-drop-refusal="too-large" hidden>{{.TooLargeLine}}</p>
  <p class="drop__error" role="alert" data-drop-refusal="not-one" hidden>{{.NotOneLine}}</p>
</div>
```

The form carries `action`/`method`/`enctype` and **no** `hx-post`: htmx must
not intercept it, and `form.submit()` (not `requestSubmit`) fires no submit
event anyway. Every sentence the page can show lives in the template or is
composed in the handler; the script only toggles `hidden`.

**View data** — `internal/web/web.go`, `libraryPage` gains:

- `ImportDrop *importDropView` — nil when `svc.ImportEnabled()` is false, so
  a read-only library renders no target and the script finds nothing. The
  empty-state line branches on the same field; no separate flag.

`importDropView` (in `internal/web/import.go`, beside `importPage`):
`MaxBytes int64` (`svc.MaxImportBytes()`), `TooLargeLine string` from
`importFailureLine(importer.ErrTooLarge, max)` — the exact sentence the
server's refusal shows, reused rather than restated — and `NotOneLine`,
a new `const dropOneFileLine = "Drop one book file at a time."`. Built by a
small `importDropViewFor(svc)` called from the library handler where
`libraryPage` is assembled (`web.go:396`).

**Script** — new `internal/web/static/js/drop.js`, added to `site-scripts`
(it no-ops without `[data-import-drop]`, like `search.js` without its input):

- Acts only on drags whose `dataTransfer.types` includes `"Files"`, so
  dragging a cover image or a link within the page is left to the browser.
- `dragenter`/`dragleave` on `document` keep a depth counter so the overlay
  does not flicker over child elements; `dragover` calls `preventDefault`
  and sets `dropEffect = "copy"`; `drop` always calls `preventDefault` for a
  file drag, so a refused drop never navigates the tab to the file.
- On drop: hide any shown refusal. If `files.length !== 1`, or the one item
  is a directory (`items[0].webkitGetAsEntry()?.isDirectory`, when that API
  exists), show `not-one`. If `file.size > maxBytes`, show `too-large`.
  Otherwise `input.files = dataTransfer.files`, switch the overlay to
  `uploading`, and `form.submit()`.
- No extension check: format is decided by content in `detectSuffix`, and
  the server's 422 page already says "not an EPUB or FB2 file".
- `pageshow` resets the overlay, the refusals and `input.value`, so Back
  into a bfcache'd library page after an upload is not stuck on
  "Uploading…".

**Styling** — `internal/web/static/css/app.css`: `.drop__overlay` fixed,
full-viewport, translucent `var(--bg)` with a dashed `var(--rule)` inset
border and a centred serif prompt (echoing `.empty`); `.drop__error` styled
like `.import__error`, placed at the top of `main`. No new colours; tokens
only, so both themes follow.

> **Correction found while implementing.** A dashed `var(--rule)` outline
> all but vanished against the translucent overlay in the dark theme, where
> `--rule` is `#322e2a` on a `#1a1613` ground. The outline uses
> `var(--fg-muted)` instead, still a token, so both themes follow.

**Discoverability**

- Empty state (`book-grid`, `partials.html:151`): when `.ImportDrop` is set,
  add a second `empty__body` line — "Or drag a book file onto this page to
  import it."
- `import-form` hint (`partials.html:499`): append "You can also drop a file
  on the <a href="/">library page</a>." — `importPage` already knows import
  is enabled whenever the form renders.

**Unchanged by construction**: `sameSiteOnly` and `fetchMetadataGuard` pass
(a native same-origin submit sends `Sec-Fetch-Site: same-origin`);
`extendDeadlines`, `MaxBytesReader`, draining and the 422 page all apply as
for any non-htmx upload; nothing in `internal/importer` or `internal/service`
changes.

## Steps

1. `internal/web/import.go`: `importDropView`, `dropOneFileLine`,
   `importDropViewFor`.
2. `internal/web/web.go`: `libraryPage.ImportDrop`, set in the library
   handler.
3. `internal/web/templates/partials.html`: `import-drop` partial; empty-state
   line; import-form hint line. `library.html`: include `import-drop` inside
   `<main>`, outside the grid.
4. `internal/web/static/js/drop.js`; `site-scripts` loads it.
5. `internal/web/static/css/app.css`: `.drop*` rules.
6. Tests in `internal/web/import_test.go` (reuse `newImportHandler`):
   - `GET /` with import enabled renders `data-import-drop` whose
     `data-max-bytes` equals the configured cap, a form whose opening tag
     (via the existing `openTag` helper) has `action="/import/file"`,
     `method="post"`, `enctype="multipart/form-data"` and no `hx-post`, and
     the too-large line equal to `importFailureLine(importer.ErrTooLarge, …)`.
   - With import disabled (the read-only setup of
     `TestImportIsDisabledWhenTheLibraryIsReadOnly`), `GET /` renders no
     `data-import-drop` and no empty-state drop line.
   - The htmx grid fragment (`GET /?q=…` with `HX-Request`) carries no
     `data-import-drop` — the target must not be duplicated or dropped by a
     swap.
   - Empty library with import enabled shows the drop line; the Import
     page's hint links to `/`.
7. Docs, present tense only:
   - `docs/notes/import.md`: new section "Dropping a file on the library
     page" (why a native submit of a hidden plain form: it is the no-JS
     upload's own path, so redirect, 422 page and every bound are shared;
     why the size pre-check; why one file); rewrite the "What is
     deliberately absent" bullet to name only importing from a URL.
   - `docs/notes/web.md`, "htmx contract and progressive enhancement": the
     drop target is JavaScript-only by nature, is an enhancement over the
     Import page, and deliberately bypasses htmx.
   - `CLAUDE.md`, Import invariants: the drop fills and natively submits the
     hidden upload form — never fetch or an htmx request, and the form
     never gains `hx-post`; `import-drop` is included only from
     `library.html`, never from `book-grid`; every sentence it shows is
     composed server-side, the too-large one through `importFailureLine`.
     Code map: `internal/web` mentions the drop target.
   - `README.md`, "Importing a book": one sentence that a file can be
     dropped onto the library page.
8. Move the plan to `docs/plans/completed/` once implemented.

## Verification

- `go test ./...` and `go vet ./...`.
- `preview_start` `{name: "server"}` (`.claude/launch.json`). Drops are
  simulated with `javascript_tool`: build a `DataTransfer`, add `File`s, and
  dispatch `dragenter`/`dragover`/`drop` `DragEvent`s on `document.body`. A
  real book's bytes come from a small EPUB built in the scratchpad, passed
  in as base64 and decoded in the page. Check:
  - the overlay appears for a Files drag and not for a text drag;
  - two files → the `not-one` line, no navigation;
  - a `File` of `max + 1` bytes → the `too-large` line, and
    `read_network_requests` shows no POST;
  - a non-book file → the 422 Import page saying it is not an EPUB or FB2;
  - the EPUB → `/import/{id}` with the `new` verdict; confirm, go back to the
    library, drop it again → the preview with the `exists` link;
  - Back from the preview → the overlay is not stuck on "Uploading…".
- `resize_window` dark mode for the overlay; a read-only `./library`
  (`chmod -w`, restart) renders no drop target and no empty-state line.
- Screenshots of the overlay and of the landed preview as proof.
