# Web UI and service layer

Rules for `internal/service` and `internal/web`.

## Routes

`GET /{$}` (grid, search, paging), `GET /books/{id}`, `GET`/`POST
/books/{id}/metadata/{field}`, `POST /books/{id}/locations/forget`, `POST
/books/{id}/send`, `GET /books/{id}/sends/{sendID}`, `POST
/books/{id}/enrich`, `GET /books/{id}/enrichment/{jobID}`, `POST
/recipients/remove`, `GET /history`, `GET /import`, `POST /import/file`,
`POST /import/url`, `GET /import/{id}`, `GET /import/{id}/cover`, `POST
/import/{id}/confirm`, `POST /import/{id}/discard`, `/static/`,
`/covers/`. The import routes are governed by `docs/notes/import.md`.

## Service layer

- **`internal/service` sits beneath the handlers; a handler parses the
  request, calls one service method and renders.** A future `/api/v1` is
  a second thin transport, so validation and normalisation live where both
  need them and presentation stays a pure function of the data.
- **`Service.now` is the clock for every timestamp the service writes,
  and `relativeTime(t, now)` takes `now` as a parameter.** Timing is
  testable without sleeping.
- **`New` takes functional options, `WithImporter` and `WithFetcher`; a
  nil importer or fetcher answers `ErrImportDisabled`, the nav asks
  `svc.ImportEnabled()`, and `ImportPreview` is an alias for
  `importer.Staged`.** The importer is the service's own, where the
  send and enrich flags are configuration the service never sees.
- **`ListBooks` and `SearchBooks` build a `BookSummary` through one shared
  helper.** The two grids cannot diverge.
- **`BookSummary.Locations` normalises a book absent from
  `CountFilesByBook` to 1.** One location is what an absent entry means.
- **`SearchBooks` sanitises through `storage.SanitizeFTSQuery`; a query
  that sanitises to nothing is `ListBooks`.** An empty box and a fresh
  page are one state.
- **`SearchResult.Searched` is decided by the service, never derived by
  the transport from the raw query.** `?q=%00` looks non-blank and is no
  search; a transport-side flag would render a count over the whole
  library.
- **`Fields` is fetched only when something matched.** The no-matches
  state names the searched fields itself.
- **`GetBook` returns `nil, nil` for an unknown id and the handler answers
  404.** Absent is not an error.
- **`BookDetail.FileSize` is book-level, from the first location, and
  `HasFileSize` distinguishes no location from a zero-byte file.** Every
  location is byte-identical, and the page must not claim `0 B` for a size
  it does not know.
- **`UpdateBookMetadata` returns the reloaded `BookDetail`, never the
  input.** Normalisation is visible.
- **Title is required; every scalar but description rejects a line
  break.** A stored break breaks every single-line rendering, and an API
  could submit one where a browser input cannot.
- **Authors are split on newlines, trimmed, blanks dropped, a repeat kept
  once.** A repeat in free text is a slip.
- **A rejected value is a `metadataValidationError` wrapping
  `ErrInvalidMetadata` and carrying the sentence the field shows.** A bad
  value is a field error, never a 500.
- **`QueueSend` validates through `net/mail.ParseAddress` and stores only
  the mailbox; `ErrInvalidAddress` queues nothing.** A pasted display name
  is not part of the address.
- **`sendableFormat` decides `Sendable` and `SendableNote`, today `epub`
  only; `QueueSend` does not refuse an unsendable book, the button is not
  rendered.** Amazon drops an FB2 silently, so Delivered would lie; a 4xx
  on a hand-crafted POST is a second rule to keep in step for no gain.
- **`Notify` and `NotifyEnrichment` are function fields set by
  `cmd/server`, nil in tests and when a queue is unconfigured.**
  Importing `internal/sender` is a cycle, and one multiplexed hook would
  poke the wrong worker.
- **`Notify` fires only when `EnqueueSend` reports it inserted.** A double
  submit never wakes the worker twice.
- **Sending and enrichment stay two parallel surfaces (`sendStateFrom`,
  `enrichmentStateFrom`, one `At` field each). Never abstract over
  exactly two cases.** They differ in precisely the part that would have
  to be generic.
- **`SendHistory` covers a trailing 30 days capped at `SendHistoryLimit`;
  truncation is detected by asking for one row past the limit, as
  `HasMore` is.** No second `COUNT`. The cap is exported because
  `internal/web` spells it out in the scope line.

## Rendering

- **`render` executes into a buffer before writing; only the pre-write
  error reaches the handler, and a write failure after the commit is
  logged inside `render`.** A template error is a clean 500, and
  `http.Error` on a committed response would double-write.
  `renderStatus` is the same with an explicit status.
- **Every sentence the page shows is composed in the handler; the
  template only chooses a block.** The paths badge is set only above one,
  so the template cannot render "1 paths".
- **Static assets carry a content-derived `ETag` computed once at startup
  and a five-minute `max-age`.** `embed.FS` reports a zero `ModTime`, so
  `http.FileServer` would emit no validator.
- **Covers carry a day-long `max-age` and never `immutable`.** A cover is
  named by the book's hash, not the bytes served.
- **Both file mounts wrap their filesystem in `noDirFS`, and every route
  serving raw bytes sets `X-Content-Type-Options: nosniff`.** A directory
  must 404 rather than list every hash in the library.
- **The UI is translated from mockups kept as `UI.md` and `ui-handoff/`
  on the `init` branch.**

## htmx contract

- **htmx is vendored at `static/js/htmx.min.js`, 4.0.0, pinned in a
  comment at the top, and used only where dynamism is needed.**
- **Every read affordance carries both `href` and `hx-get`, every editor
  both `action` and `hx-post`; the plain answer is a page or a 303, the
  htmx answer a fragment.** One markup path, so no no-JS path drifts.
- **Dropping a file on the library page is the upload form's own post:
  `drop.js` fills a hidden plain form and calls `form.submit()`, never a
  fetch or an htmx request.** The drop relies on the redirect and the 422
  page; see `docs/notes/import.md`.
- **A fragment is answered when `HX-Request` is present and
  `HX-History-Restore-Request` absent (`isHTMXFragment`).** Back issues a
  GET marked with the second header and swaps the answer into the whole
  body; a fragment there leaves a bare grid that cannot search.
- **Every route serving two bodies names both headers in `Vary`, import
  routes included; a one-body route sets no `Vary`.**
- **The `htmx-config` tag in `document-head` keeps `4xx` and `5xx` in
  `noSwap`; every form whose route answers 422 carries
  `hx-status:422="swap:outerHTML"`, `send__form` and `enrich__form` also
  carry `hx-status:503="swap:outerHTML"`, and every `<body>` carries the
  403 one.** A plain-text `internal error` must never replace a control.
  A route gaining an error body gains the attribute on the element that
  asked, or Save looks like it did nothing; the 503 opt-in sits on the
  enabled form, the one a stale tab holds.
- **The opt-in names the exact status, never a wildcard.** htmx consults
  `noSwap` before the element at each of the `422`, `42x`, `4xx` steps, so
  `hx-status:4xx` is never reached.
- **A rejection answers 422 wherever it has a body, on both paths.** A
  redirect lands on a page that has forgotten the message and the input.
- **`includeIndicatorCSS` is false; nothing carries `htmx-indicator`, and
  every indicator is a rule of this app's own keyed on `htmx-request`.**
- **The timeout is htmx's own 60s everywhere but the upload and confirm
  forms, which carry `hx-config="timeout:0"`. Never move that to
  `defaultTimeout` on the meta tag.** An upload's window scales with
  `MAX_IMPORT_SIZE` and a confirm's with `importer.IndexTimeout`; a hung
  search with no bound leaves its indicator up forever.
- **The upload, confirm and discard forms carry `hx-disable="find
  button"`.** Dimming is appearance only: a focused button still answers
  Enter, and htmx queues the second submit. The attribute is applied after
  the body is read, so it cannot strip the file part.

## Search

- **Search is `GET /{$}` with `q`; each keystroke is a `delay:300ms`
  request swapping `#book-grid` with `outerHTML`, and `hx-push-url` keeps
  the URL shareable.** The box lives only in the full page and is never
  re-rendered, so a keystroke mid-request is never lost.
- **The search input carries `hx-sync="this:replace"`.** htmx queues one
  request per element and a queued one carries the query it was built
  with, so the grid settles on an older string than the box holds and
  nothing says so. `replace` narrows that window without closing it, which
  is why the answer is the attribute and not a patched htmx.
- **The clear link, the `/` shortcut hint and the filtering status line
  resolve in the browser.** The input is never re-rendered. The hint is
  unhidden only after `search.js` binds the key, and the status line shares
  the results count's container so the grid does not jump.
- **The handler passes `q` through `storage.NormalizeSearchQuery`, and
  `SearchMaxLength` carries `storage.MaxSearchBytes` into `maxlength`.**
  The first only makes every rendered copy the searched string; the input
  is outside `#book-grid`, so only `maxlength` bounds the request. A
  forgotten field renders `maxlength="0"`, so the wiring has a test. UTF-16
  units approximate the byte cap from above.
- **A query that sanitises to nothing renders the plain grid with no
  count; a search renders the results line or the distinct `search__empty`
  block; the empty-library state disables the control.** Counts group
  thousands as the masthead does, and the two empty states call for
  different next actions.
- **Search never orders by relevance.** A grid must not reorder under
  someone typing, and the same property lets one cursor page both grids.

## Paging

- **`pageSize` is 48, the mockup's figure.** A number in a mockup is a
  decision about how much scrolling one reveal buys.
- **One route serves three shapes: the page, the `book-grid` fragment, and
  with `append=1` the `book-grid-cards` batch.** `HX-Request` tells two
  apart, so the third is named in the query; one template renders the
  cards wherever they land.
- **The trigger is one `<li class="grid__more">` inside the cards' `<ul>`,
  replacing itself, and carries both `href` and `hx-get`.** It must swap
  in a legal child of that list, and the href is a whole page at the same
  cursor, since a paged grid with no fallback is worse than no paging.
- **`MoreLabel` empty means no trigger; its count is the library total, or
  `MatchCount` during a search.**
- **A new search rebuilds the whole grid including its trigger, so paging
  resets by construction.** A stale trigger would append page two of the
  previous query; a test pins it.
- **The clear link persists on a deep unfiltered page, reading "first
  page".** With JavaScript off nothing else on such a page leads home.
- **The cursor is keyset and is `internal/storage`'s decision.** See
  `docs/notes/storage.md`.

## Book detail and editing

- **A non-numeric and an unknown id both plain 404.** Neither is worth its
  own page.
- **Empty optional fields render as visible em-dash rows.** A hidden field
  cannot be filled in.
- **`PublishedDate` renders exactly as stored.** It is free text, and
  parsing it would lie confidently.
- **Locations reveal through a native `<details>`, with a location in its
  grace period annotated.** No JavaScript is guaranteed to have loaded.
- **A marked location carries a forget form posting to `POST
  /books/{id}/locations/forget` with the `book_files` id in `file`.** The
  scanner never prunes under a top-level directory that yielded no files,
  so a renamed folder otherwise leaves a dead path for good
  (`docs/notes/scanner.md`).
- **The `book-locations` partial is the whole `<dd>` with
  `id="locations"`, rendered by page and route alike.** An `outerHTML`
  swap must replace the element carrying the id.
- **Forgetting a book's last location answers `303` to `/` without htmx
  and `HX-Redirect: /` with it.** A fragment cannot be swapped into a page
  whose subject is gone.
- **Editing is one route per field, and `makeFieldViews` builds all seven
  from one place.** Each field is its own swap target, and page and
  fragment cannot drift.
- **Each view carries `Value` and `Display` separately, and every read
  affordance carries an `aria-label` naming its field.** Stored and
  readable forms differ, and an empty value's visible text is only a dash.
- **Without htmx the GET redirects to `?edit={field}` and the POST 303s
  back; an unrecognised `?edit=` opens nothing; both paths load the book
  before choosing a shape.** An unknown book is a 404 on each, never a 303
  to nowhere.
- **Every metadata `<dd>` takes the row's free space, as a property of the
  row and not of editable rows alone.** `added` has no editor and would be
  the one pushed right.
- **`storage.ParseMetadataField` gates these routes and `?edit=`, and
  `cover` is absent from it.** `cover_path` is never typed text; admitting
  the name would answer 500 where it should 404.
- **`maxMetadataFormBody` is `3 × service.MaxMetadataValueBytes + 1024`,
  sized off the author list; over the cap is a field error, not a 413.**
  Form-urlencoding triples non-ASCII text, and 100 names of 1 KiB outweigh
  64 KiB of prose, so sizing off the description would reject a valid
  author list before `normalizeAuthors` could.
- **`providerSourceNote` renders a marker for a provider's name and nothing
  for `embedded`, `manual` or absent.** A third-party guess is the only
  origin that changes how much to trust a value. Editing clears the marker
  because the POST reloads the book rather than echoing the input; a test
  pins that, since it is what a later optimisation removes.

## Send and enrichment controls

- **The send control is `{{template "send-control" .}}` over
  `bookDetailPage`, and the send routes build a mostly-zero
  `bookDetailPage` rather than a parallel type.** `applySendState`
  computes the states once, so the template branches on a block and never
  on phrasing; `queued` and `sending` are one visual state.
- **The whole control is one swap target, `#send`; the pending block's
  poll targets `#send`, and only the pending block carries
  `hx-get`/`hx-trigger`.** Polling stops by construction. The form's
  `hx-post` survives every state, so Retry stays enhanced and is a new row.
- **Every route that renders the control copies `SendableNote`, and the
  page carries only the note, no `Sendable` bool.** A fragment can never
  offer a button the page withholds, and a route copying neither cannot
  render a refusal with no reason.
- **With zero recipients the add-address `<details>` renders open and the
  `<select>` is omitted; it also renders open after a rejected address with
  the typed values carried back, and that path re-reads `LatestSend`.**
  Nothing was queued, so retracting a shown result would contradict the
  page.
- **`POST /books/{id}/send` answers a fragment or a 303; a rejected
  address answers 422 on both paths through `makeBookDetailPage` and
  `setPageSendError`; sending unconfigured answers 503 with the disabled
  fragment; `GET /books/{id}/sends/{sendID}` is scoped under the book
  id.** One setter cannot word the error two ways, a 503 explains itself
  to a stale tab, and the scoping keeps one book's send off another's page.
- **`POST /recipients/remove` is reachable only from the send control; the
  remove button submits the sibling `recipient-form` through `form=`, the
  form carries the book id, and an unknown address 200s.** HTML forbids
  nested forms and the button must never also send; the picker changed, so
  the whole control re-renders; a double submit is a slip.
- **Enrichment reuses the send state machine: `#enrich`, the same scoping,
  the same fragment-or-303 split, polling that stops because only the
  pending block carries a trigger; with no provider the control renders
  disabled and the POST answers 503.**
- **"Nothing to add" is a success, carried by `EnrichResultOK`, and the
  result names the fields that moved.** It is the ordinary outcome for a
  complete book, and a failure there teaches distrust of a working feature.
- **Enrichment is exactly three affordances: marker, trigger, result. A
  library-wide enrich, an enrichment history page and editable provenance
  are absent by decision.** The first has no honest progress display, the
  second shows nothing the fields do not, and a source is a fact, not a
  setting.

## Cross-site protection and the HTTPS requirement

- **Every state-changing route is wrapped in `sameSiteOnly`, which rejects
  a `Sec-Fetch-Site` other than `same-origin` or `none`.** There is no
  login, and a form POST from any page needs no preflight to reach a LAN
  server.
- **Deployment is an HTTPS gateway in front and the plain listener
  reachable only by it.** Browsers send `Sec-Fetch-Site` only to HTTPS or
  localhost, so over plain HTTP `sameSiteOnly` admits everything.
  Redirects are relative paths, so the app is scheme-agnostic.
- **`cmd/server` wraps the whole handler in `fetchMetadataGuard`, picking
  `RequireFetchMetadata` (refuse a non-GET/HEAD/OPTIONS request with no
  header, Warn per refusal) or `WarnMissingFetchMetadata` (admit all, one
  Warn per process) through a pure function with a table test on both
  directions.** Swapped branches would invert the security default with
  every handler test green.
- **The refusal is a 403 for every client; an htmx fragment caller's
  carries the `fetch-metadata-refused` partial, which every `<body>` swaps
  in through `hx-status:403:inherited="swap:afterbegin"`. `next` is not
  called in either shape.** The line lands as the first child of the
  form's target, so the control survives and the wrapper never learns
  which control posted.
- **`sameSiteOnly` passes an empty header through.** It answers only "the
  browser said cross-site", and the opt-out mode depends on that.
- **There is no `Host` allowlist.** A rebound hostname fails certificate
  validation against an HTTPS origin, and the plain listener is not
  reachable from a browser.

## Upload bodies

- **The upload route extends both halves of its deadline through
  `http.NewResponseController`, the link and confirm routes the write half
  alone; `cmd/server`'s timeouts are never loosened for other routes.** Go
  installs the write deadline once, when the headers are read, so a
  widened read window alone sits inside a write deadline that expired
  while the body arrived, and the import lands with its answer never
  reaching the browser. Extended rather than removed, so a stalled upload
  still ends. `confirmWindow` is derived from `importer.IndexTimeout`,
  never restated.
- **`http.MaxBytesReader` bounds the body at the cap plus multipart
  overhead and is installed before any refusal; the number a refusal names
  is the importer's own count of the file part.** The read-only refusal
  answers a body still arriving, and only the importer sees the part.
- **The body streams through `r.MultipartReader`, never
  `ParseMultipartForm`, and every refusal drains what is left before
  rendering.** `ParseMultipartForm` spools a second copy of the file, and
  no response is written over a body nobody consumed.

## History page

- **`GET /history` renders with sending unconfigured.** It is a log, not
  an action.
- **`historyStatus` collapses `queued` and `sending` into "Sending", as
  the send control does.** Two screens must not name one state
  differently.
- **`BookURL` is empty for a pruned book and rendered unlinked; the row's
  title and address come from `send_log`'s own columns.** That is what
  lets a pruned book's send still appear.
- **The scope line reads "last 30 days" and names `SendHistoryLimit` once
  the cap has truncated the window.** A false "no" causes a duplicate
  delivery, the failure the send queue exists to avoid.
- **`relativeTime` converts both times to the server's local zone and
  compares calendar dates through `AddDate`, never `< 24h`.** A send at
  23:50 is "yesterday" at 00:10.
- **`site-header` takes `Nav` and `HeaderNote` composed by the handler;
  `navFor` marks the current entry as plain text, the book page passes
  `navFor("library")`, `headerBookCount` composes the count both book
  pages share, and the history page puts its scope line in that slot.**
  There is nowhere more useful to send someone already on the page.

## Styling

- **There is one button system in `app.css`: `.button` with `--md`/`--lg`
  and `--primary`/`--secondary`/`--tertiary`, plus `.spinner` and
  `.spinner--sm`.** `.locations__forget` is in it at a size it states
  itself and clears the base `min-height` as `--md` and `--lg` do, since
  the base minimum is shaped for the editors. `.search__spinner` stays
  outside because it is coloured against the input and shares only the
  keyframes; `.paste-button` because it is a toolbar control drawn to the
  search row's metrics; `.send__remove` because it is a borderless text
  affordance, not a button. Known limit:
  `docs/backlog/2026090702-button-base-carries-the-editors-size.md`.
- **The Paste button's separator hides with the button through
  `.search__paste:has(> [data-paste-button][hidden])`.** The script
  reveals one element, and a rule standing beside nothing reads as a
  broken toolbar.
- **`.button--primary`'s foreground stays `var(--bg-raised)`, never
  `#fff`.** `--accent` is a light tan in dark theme, where white measures
  2.9:1; the token holds 5.8:1 in both.
- **`--md`/`--lg` reset the base `min-height`, and
  `.button--tertiary:disabled` beats `.button:disabled` on source order
  alone.** Without the first the enrichment button gains a pixel; grouping
  the `--tertiary` rules together silently reverts the second.
- **Two tests guard button class names in each direction: no retired name
  survives, and every class the markup names has a rule.** A mistyped
  modifier renders as a bare `.button` with every handler test green.
- **`white-space: pre-line` on the description's read view is contract,
  with one test for the rule and one that the break reaches the markup.**
  A description is stored with its paragraph breaks; `pre-wrap` would also
  reproduce a provider's indentation and stray double spaces.
- **The selector is `.editable__read.detail__description`, never the bare
  class.** The `<textarea>` carries `.detail__description` too, and an
  author `white-space` overrides its `pre-wrap` default, collapsing spaces
  a submitted value keeps.
