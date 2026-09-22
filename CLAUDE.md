# library

@README.md

README.md, imported above, says what the app does and how it is configured.
This file maps the code and lists the rules that are easy to get backwards.
The reasoning behind each area lives in `docs/notes/`; read the relevant
note before changing that area, and put new rationale there rather than
here. See Documentation below for what these files may and may not say.

## Code map

- `cmd/server` — entrypoint. Reads configuration, resolves the library
  through `requireExistingDir` (stat, then `EvalSymlinks`) and the covers
  and database directories through `resolveDir` (create, then
  `EvalSymlinks`); a dangling link in any component fails startup naming
  link and target, and a refused `MkdirAll` names both uids. Opens the
  database, serves immediately, and runs the scan loop, the sender worker,
  the enrichment worker and the import janitor on one cancellable
  `scanCtx`; the janitor exists only when the importer does. Shutdown
  order: HTTP server, then `waitForBackground` (10s) over each of them,
  then the database. Builds an `importer.Stager` through `newStager` only
  if both the writability probe on `LIBRARY_DIR` (`probeWritable`) and the
  staging directory under `os.TempDir()` succeed, which is the whole of
  what "importing is offered" means; either failing disables import at
  Warn and never fails startup. Parses `MAX_IMPORT_SIZE` through
  `parseByteSize`. Notes: `docs/notes/scanner.md`,
  `docs/notes/design.md`, `docs/notes/import.md`.
- `internal/storage` — SQLite (`modernc.org/sqlite`, WAL, foreign keys,
  5s busy timeout). Bounded read pool, single-connection write pool
  (`DB.Write`). Embedded migrations under `migrations/`, one statement per
  file. Tables: `books`, `authors`, `book_authors`, `book_files`,
  `field_sources`, `books_fts`, `recipients`, `send_log`,
  `enrichment_jobs`. Search sanitisation (`SanitizeFTSQuery`,
  `NormalizeSearchQuery`), the keyset cursor (`BookPage`), and the
  derivations and limits every writer of a metadata column shares
  (`SortTitle`, `NormalizeISBN`, `PlainDescription`, `CapBlankLines`, the
  `Max*` constants) live here.
  `internal/storage/storagetest` — the database a test opens (`Open`), a
  copy of a template migrated once per test binary, plus `SeedSends`. Used
  by every package above this one but `cmd/server`.
  Notes: `docs/notes/storage.md`, `docs/notes/testing.md`.
- `internal/epub`, `internal/fb2` — embedded metadata and cover bytes from
  each format, same `Metadata` shape so the scanner treats them alike.
  Neither decides that what a file calls a cover is an image.
  `internal/cover` — resize to ~400px, JPEG, atomic write into
  `COVERS_DIR` keyed by content hash; `ContentType` is the header read
  `Store` makes, for a caller that serves raw cover bytes. All three cap
  what they read from an untrusted file. Note: `docs/notes/formats.md`.
- `internal/scanner` — walks `LIBRARY_DIR`, syncs it into storage
  (`Scan`), indexes one path on demand (`IndexFile`, the importer's way
  in), reconciles missing files in two phases, regenerates covers, and
  hosts the fsnotify watcher (`watcher.go`, reached through `watchSet` so
  tests can drive its debounce) plus the startup mount and delivery
  checks. Note: `docs/notes/scanner.md`.
- `internal/resend` — one-attachment `Client.Send` against Resend's API.
  `internal/sender` — the single `Worker` over `send_log`. Note:
  `docs/notes/sending.md`.
- `internal/enrich` — the enrichment `Worker`, the `Provider` interface,
  the pure `Resolve` function and `plausibleMatch` (`match.go`), the
  three decorators (`decorator.go`), `FetchCover` and the guards both
  providers share: `RefusePrivateAddress` (`cover.go`) and
  `CheckLookupRedirect`/`SameHost` (`redirect.go`).
  `internal/openlibrary`, `internal/googlebooks` — the two providers.
  `internal/providers` — the name → constructor registry
  (`METADATA_PROVIDERS`). Note: `docs/notes/enrichment.md`.
- `internal/importer` — staging, previewing and landing a book uploaded
  through the web UI: `Stager`, the three `Verdict`s, `detectSuffix`
  (`sniff.go`) and the library-name derivation and collision loop
  (`name.go`). Calls `scanner.IndexFile` to index what it wrote. Note:
  `docs/notes/import.md`.
- `internal/service` — the layer beneath the HTTP handlers, so a future
  `/api/v1` is a second thin transport. Owns validation and normalisation
  (`UpdateBookMetadata`, `QueueSend`), page assembly (`BookSummary`,
  `BookDetail`, `SearchResult`; `ImportPreview` is an alias for
  `importer.Staged`), and the
  `Notify`/`NotifyEnrichment` function fields `cmd/server` wires to the
  workers. `New` takes functional options; `WithImporter` is the only one.
  Note: `docs/notes/web.md`.
- `internal/web` — `html/template` pages and htmx fragments, CSS and the
  vendored htmx under `static/`, all `go:embed`ded. Routes: `GET /{$}`
  (grid, search, paging), `GET /books/{id}`, `GET`/`POST
  /books/{id}/metadata/{field}`, `POST /books/{id}/locations/forget`,
  `POST /books/{id}/send`, `GET
  /books/{id}/sends/{sendID}`, `POST /books/{id}/enrich`, `GET
  /books/{id}/enrichment/{jobID}`, `POST /recipients/remove`,
  `GET /history`, `GET /import`, `POST /import/file`, `GET /import/{id}`,
  `GET /import/{id}/cover`, `POST /import/{id}/confirm`, `POST
  /import/{id}/discard`, `/static/`, `/covers/`. The library page is also
  a drop target for import (`static/js/drop.js`). The UI is translated from mockups
  kept as `UI.md` and `ui-handoff/` on the `init` branch. Note:
  `docs/notes/web.md`.
- `docs/notes/design.md` — purpose, constraints, the library-directory
  rules, the conversion model behind `derived_from`, and the deferred list.
- `docs/notes/import.md` — where an import lands, the staging model, the
  three verdicts and the order confirm writes in.
- `docs/notes/testing.md` — the fake clock, the in-memory test server, and
  what still runs on real time.
- `docs/plans/`, `docs/backlog/` — see Planning and Backlog below.

Logging is `log/slog` on stderr through the package-level functions,
levelled once in `cmd/server` from `LOG_LEVEL`.

## Invariants

Each of these is a rule the code will not tell you about and a plausible
tidy-up would break. The note named in the heading carries the reasoning.

### Storage (`docs/notes/storage.md`)

- A `DB.Write` callback never calls an exported `*DB` method: the pool has
  one connection and the outer call holds it. Compose the `…Tx` helpers
  inside one `Write` instead. A directly nested `Write` returns
  `ErrNestedWrite`.
- Every exported write method owns exactly one transaction; a multi-step
  atomic write is one `Write` call, never two exported methods.
- No backfill migrations. A development database older than a table is
  reset by deleting the file.
- **A cleared field stays `manual`.** Never infer provenance from emptiness.
- A `cover` row in `field_sources` exists only for a provider-supplied
  cover; a scanner-extracted cover has no row. The scanner-versus-provider
  test is "row exists", never a comparison against `embedded`.
- `FieldCover` stays out of `metadataFields`, so `ParseMetadataField
  ("cover")` returns false and no edit route accepts it. `UpdateBookField`
  refuses `FieldCover` and `authors` as a second guard.
- Writing `cover_path` clears `cover_retry`, from every writer.
- `ClearProviderCover` and `RecordUnusableCover` take the `cover_path` the
  caller observed and refuse to blank anything else; their bool means
  *cleared*, not *exists*. Their `DELETE` is scoped to `(book_id, field)`.
- `ApplyEnrichedFields` re-checks `fieldIsStillMissingTx` per field inside
  its own transaction and records only what it actually wrote.
- `FieldLimit` is the one lookup from a metadata field to its byte limit,
  and `CapField` the one truncating derivation over it — shared by
  `internal/scanner`, `internal/enrich` and `internal/importer`.
  `internal/service` shares `FieldLimit` only: a person's edit is refused,
  never rewritten, and a line break they typed is an error rather than
  something to collapse behind them.
- `SearchBooks` and `ListBooks` order by `(sort_title, id)`; the cursor
  comparison stays in row-value form with **no** explicit `COLLATE NOCASE`,
  or the index seek becomes a scan.
- `MaxSearchBytes` (256, cut on a rune boundary) and `maxSearchTerms` (16)
  are both needed; neither subsumes the other.
- `send_log.book_id` is `ON DELETE SET NULL` with `book_title`
  denormalised, and `recipient_address` is a plain string. Every other FK
  cascades. History reads those columns and never joins `books` or
  `recipients`. A same-path replacement re-points those rows rather than
  letting them go `NULL`.
- `EnqueueSend` dedups against `queued` *and* `sending`;
  `EnqueueEnrichment` dedups against `queued` only.
- `Mark*` terminal writes are scoped to the in-progress status and are a
  silent no-op otherwise.
- `FailInterruptedSends` fails; `RequeueInterruptedEnrichment` requeues
  (or deletes when a fresh `queued` sibling exists).

### Formats and covers (`docs/notes/formats.md`)

- Every zip entry and FB2 binary is read through a cap before its bytes are
  held; never add an uncapped read in `internal/epub` or `internal/fb2`.
  `maxZipDocumentBytes` (128 MiB) bounds the whole `.fb2` inside an
  archive because `encoding/xml` buffers a full text node before returning
  it, and is applied on **both sides** of the charset decoder — a decoder
  only grows a byte count, so capping the read alone leaves the figure two
  to three times looser than it says.
- `cover.MaxCoverBytes` (8 MiB) and `enrich.MaxCoverBytes` (512 KiB) are
  separate constants, never aliases. Google Books' cover-size choice is
  calibrated against the second.
- `maxPixels` (16 MP) is sized off `image/jpeg`'s progressive-scan
  allocation, not pixels × 4. Do not raise it on the RGBA reasoning.
- `cover.Store` wraps `ErrUnsupportedCover` for anything the bytes decide
  and leaves filesystem errors unwrapped; the scanner's retry logic depends
  on that split.
- `cover.ContentType` and `Store` share one header read (`inspect`), so
  what this app calls an image is decided once. Neither format reader
  decides it, so any caller serving raw cover bytes must ask.
- Publication date is the edition's: never `creation`/`modification` in
  EPUB, never `title-info/date` over `publish-info/year` in FB2.
- Every reader of an ISBN calls `storage.NormalizeISBN` — `internal/epub`'s
  three branches, `internal/fb2`'s `<isbn>`, both providers' `ByISBN` and
  `bestISBN`. Never a private copy. It ignores text around the run, which is
  safe only because every caller reads a slot already claiming to hold an
  ISBN; `internal/epub`'s bare branch is the exception and carries its own
  guard (`bareISBN`, the whole identifier must be the run) rather than
  making the shared function pay for it. A scheme-marked EPUB identifier
  holding no run falls through to the next.
- A format package hands the scanner a plain-text description. EPUB's
  `dc:description` legally holds escaped HTML, so `internal/epub` runs it
  through `storage.PlainDescription`; `internal/fb2` reaches the same shape
  structurally and calls nothing. `PlainDescription` is also
  `internal/googlebooks`' flattening — one derivation, never a private copy.
- FB2's declared charset is decoded through `htmlindex`; only a label
  `htmlindex` does not know passes through unchanged.
- Cover files are named by the *book's* content hash, not the thumbnail's
  bytes, so `/covers/` is never served `immutable`.

### Scanner (`docs/notes/scanner.md`)

- Identity is the content hash; path is an attribute. Known content at a
  new path is a `book_files` row, not a new book. `file_path` is relative to
  `LIBRARY_DIR`, slash-separated.
- `capMetadata` bounds embedded metadata in `createBook` to what the editor
  accepts: every field but description is collapsed onto one line, then
  truncated through `storage.Max*` on a rune boundary and logged at Info.
  The collapse runs **before** the cut. Never reject the file; the field
  stays `embedded`.
- `LIBRARY_DIR` is stat'd, never created, so a read-only mount works and an
  absent one fails startup. `COVERS_DIR` and `DB_PATH`'s directory are
  created.
- `IndexFile` is `scanFile` with a fresh `Result`, and returns the book id
  the path now belongs to along with whether it created one. There is no
  second way into the index; every guard a new book needs lives in
  `createBook`.
- `scanFile` reads by content hash on the read pool and inserts later, so
  two callers can both find nothing and both insert against the UNIQUE
  `books.content_hash`. The loser re-reads and attaches its path to the
  winner's book rather than failing — that is what makes the promise that a
  sweep and an import "converge on one book" true. Every other unique column
  is either `ON CONFLICT` or read and written inside the single-connection
  write pool.
- `MatchedSuffix`, `BookFormat` and `ExtractMetadata` are exported because
  `internal/importer` must agree with a sweep about what a file is, what
  its format is called and what it holds. `ExtractMetadata` takes the
  fallback title and returns the parse error, since a sweep names a path
  and a preview names an upload.
- A `.part` file and `.applibris-write-probe` are invisible to a sweep by
  suffix. Nothing else must acquire a supported suffix before it is whole.
- The watcher never reads, hashes or parses a file. It pokes the one scan
  goroutine, so two sweeps can never overlap and correctness never depends
  on an event arriving.
- **An unknown is not evidence.** Only `fs.ErrNotExist` means gone, for
  missing rows, cover files and symlink targets alike; any other error
  leaves the row or book untouched and warns. A *successful* `Lstat` on an
  unseen row is not an unknown: the walk did not see that spelling, so the
  row is marked.
- A same-path replacement inherits `manual` fields, their provenance and
  `send_log` rows; reassignment across paths never does.
- `readEmbeddedCover` runs first; `field_sources` is consulted only after
  re-extraction has come back empty. Both cover-forgetting writes are
  guarded on the `cover_path` this sweep observed.
- `cover_retry` marks a transient store failure only. A decode failure
  (`cover.ErrUnsupportedCover`) is recorded as no cover and never retried.
- `PruneMissingFiles` filters nothing; every prune guard lives in the
  scanner. A row under a top-level directory that yielded no book files
  this sweep, or under a symlinked directory the walk declined to follow,
  is marked but never pruned, until a person forgets the row; a sweep that
  saw zero files reconciles nothing; a row under a directory the walk could
  not read is left alone at both phases.
- `ForgetMissingFile` is the person's delete, so its guards are clauses on
  the `DELETE`, never a read before it.
- Symlinked directories are not followed; they are named at Warn and
  counted in `Result.Errors`. Symlinked files are indexed like any other.
- `Refresh` checks both `(dev, ino)` and `WatchList()` membership, and
  removes a superseded watch before re-adding it.
- Do not add inode or device-id tracking. The mover's invisibility depends
  on the index storing neither.

### Sending (`docs/notes/sending.md`)

- Interrupted sends fail and never requeue. Retry is a new `send_log` row,
  never a mutation of a failed one.
- Definite verdicts (delivered, or a failure decided locally) are written
  under `context.WithoutCancel` plus `markTimeout`. Only a transport call
  abandoned with `ctx` already cancelled leaves the row `sending`. A send
  whose own deadline expired is written `failed` with `timedOutReason`,
  detected by `errors.Is(err, context.DeadlineExceeded)` *after* the
  `ctx.Err()` check.
- `resend.Client` sets no `http.Client.Timeout`; the worker's size-scaled
  context deadline is the only bound and `SendTimeout` is its floor.
- Only `fs.ErrNotExist` under a populated top-level directory
  (`scanner.TopLevelDirHasBooks`) means the file is gone; other stat or
  read errors record "could not read the file", and an index error records
  its own reason. Never fold the three together.
- `last_used_at` is bumped at enqueue, not delivery. `Service.Notify` fires
  only when `EnqueueSend` reports `inserted`.
- Only formats Amazon accepts are offered (`sendableFormat`, today `epub`);
  `QueueSend` does not refuse others, the button is simply not rendered.
- `process` recovers a panic and writes the row `failed` with
  `crashedReason`, the same shape as enrichment's.

### Enrichment (`docs/notes/enrichment.md`)

- `isMissing` is empty **and** not `manual`. Both halves, one function.
- A `Search` answer must pass `plausibleMatch`; a `ByISBN` answer never
  does. Title match required; author overlap is a veto and never a pass.
  Titles match on delimited segments with at most `maxSegments` (2) parts,
  never on substrings or undelimited runs.
- A `Search` answer never fills `isbn`, so enrichment cannot write `isbn` by
  any route. The line dropping `isbn` from the missing set is also what
  lets the early stop fire for a book with no ISBN.
- An ISBN no-match falls back to `Search` in the same iteration; an ISBN
  *error* does not.
- A step failing with `ctx` already cancelled leaves the row `running` for
  `RequeueInterruptedEnrichment`; never `MarkEnrichmentFailed` there. Every
  terminal branch, including the all-providers-failed one, guards itself
  with its own `ctx.Err()` check.
- `failed` means the job went wrong: book gone, write failed,
  `Asked > 0 && Failed == Asked`, a lost cover with nothing else written,
  or a recovered panic. "Nothing to add" is a success.
- Both workers `recover` inside `process` and write the row terminal with
  `crashedReason`. A panicking enrichment job left `running` is requeued
  into a crash loop; the panic value goes to the log, never the status box.
- Every writer of the metadata columns caps through `storage.CapField` or,
  where it refuses rather than truncates, `storage.FieldLimit`; never
  restate a number, or a value one writes becomes uneditable. A description
  is capped at two consecutive newlines by `CapField` itself, and again on
  the way in: `internal/epub` through `PlainDescription`, `internal/fb2` at
  the end of `annotationText`. A person's edit is deliberately not capped —
  `normalizeField` trims and bounds a description and shapes it no further,
  since the blank lines someone typed are their own.
- `sanitizeValue` does not flatten markup. Google's description is HTML and
  `internal/googlebooks` flattens it through `storage.PlainDescription`
  before it leaves that package; Open Library's is plain, and a blanket
  strip here would answer for a source that never sends markup.
- Providers name a cover URL and never download it. The worker fetches
  under `enrich.MaxCoverBytes` with the scheme checked on every redirect
  hop, and refuses loopback, private, link-local, multicast and
  unspecified addresses at dial time (`RefusePrivateAddress`) so every hop
  and every DNS answer is covered. Tests opt out through a `_test.go`-only
  helper; production has no switch.
- `Metadata.Partial` describes the answer, not the book. Only `WithCache`
  reads it, and only to decline storing; `Resolve` and `IsEmpty` ignore it.
- A refused redirect is **not** retryable: every return in
  `enrich.CheckLookupRedirect` wraps `enrich.ErrRedirectRefused`, and each
  client tests for it before its retryable wrap.
- Compose `WithCache(WithRetry(WithRateLimit(p)))`: cache outermost, rate
  limit innermost.
- Open Library `ByISBN` uses the edition-scoped Read API; `Search` answers
  about works and returns neither language nor publication date. Its cover
  URL carries `?default=false`, or a stale id is a 200 placeholder stored
  as a real cover.
- Google's `best()` prefers `medium` and omits `extraLarge`. Never rewrite
  a thumbnail URL's `zoom` parameter.
- Both clients share one redirect policy, `enrich.CheckLookupRedirect` over
  `enrich.SameHost` — never a copy per package. It refuses a hop leaving
  the starting host: on Google because the key travels in the query string
  and `Referer` would carry it, on Open Library because the client would
  otherwise adopt the answering host's response.
- A Google detail-request failure marks the answer `Partial` and leaves
  the list answer standing. It never fails the lookup.
- Fixtures are live captures except `internal/openlibrary`'s
  `search_*.json`; each test file says which. Never hand-edit a fixture to
  make a test pass.

### Import (`docs/notes/import.md`)

- Format is decided by content in `detectSuffix`; the client's filename and
  `Content-Type` never choose the parser or the suffix written. An XML
  opening alone is not enough: `<FictionBook` must appear in the sniff
  window, or any XML document is written into the library as a `.fb2`.
- The preview is capped through `storage.CapField` before it is built, so it
  shows what would be stored — and so the title-match verdict compares a
  capped title against the capped `sort_title` the scanner derived.
- Staging is bounded in total bytes, not in stages. The reservation is taken
  at the cap before the copy and corrected after to everything the stage
  retains — the file, the cover held for the preview, the metadata — never
  the file alone, or a small upload holds megabytes of cover against a small
  charge. It is given back by every path that drops a staged file: discard,
  expiry, confirm, and the duplicate verdict, which releases the file's
  share and keeps charging the cover its page still renders.
- Confirm does not re-check a `new` or `title-match` verdict: it copies,
  and `IndexFile`'s answer is the truth, so landing on a book the index
  already had is a second location and not an error. `exists` is the one
  verdict that decides anything at confirm, because reaching it deleted the
  staged file — there is nothing left to copy and the existing book id is
  the answer.
- A failed `IndexFile` leaves the library file in place. Never delete a
  file the library already holds over an index error.
- The index write runs on `context.WithoutCancel`, because the library owns
  the bytes before it starts — the same rule `internal/sender` applies once
  Resend has accepted a message. A cancelled write would report a failure
  that did not happen to every later confirm.
- A swept staged file reads as `ErrExpired`, never as a raw `fs.ErrNotExist`:
  expiry is checked under the lock and the file opened after it is dropped.
- The library is written only as `<name>.part` then published with
  `os.Link`; nothing creates a supported suffix in the library directly.
  The link is what refuses to replace an existing name, which `os.Rename`
  would do silently — never swap it back. A filesystem with no hard links
  falls back to `Lstat` then `Rename`, logged once, and the window stays
  open there.
- A staged cover is served with the media type `cover.ContentType` decided
  from its header, never `http.DetectContentType` over the bytes: no format
  reader checks that what a file calls a cover is an image, so sniffing lets
  an upload choose the type. `HasCover` therefore means "a cover this app
  would keep", not "the file named one".
- Every route serving bytes rather than a rendered template sets
  `X-Content-Type-Options: nosniff` — `/import/{id}/cover`, `/covers/` and
  `/static/`.
- The write probe is removed before it is created. `O_EXCL` refuses a name
  already taken, so a probe left by a crash would otherwise disable
  importing for good; `O_EXCL` stays, so the create never follows a symlink
  left at that name.
- Staged state is in memory and on `os.TempDir()`. Nothing about a stage is
  written to the database, and `importer.New` wipes the staging directory.
- A `Stager` exists exactly when importing is available; there is no
  disabled `Stager`. `cmd/server` builds one only when the probe succeeds
  and the staging directory can be created; either failing is a Warn, never
  a startup failure, since a library that can be read is still worth
  serving. `internal/service` answers a nil one with `ErrImportDisabled`,
  the convention `Notify` follows. Never add a second disabled state inside
  the importer.
- Whether the nav offers Import comes from `svc.ImportEnabled()`, not from
  a flag threaded down beside `sendEnabled` and `enrichEnabled`: those two
  are configuration the service never sees, where the importer is its own.
- A file dropped on the library page is the upload form's own post:
  `drop.js` fills the hidden `drop__form` and calls `form.submit()`. Never a
  fetch or an htmx request, and the form never gains `hx-post` — the
  redirect to the preview and the 422 page are what the drop relies on.
  `import-drop` is included only from `library.html`, never from
  `book-grid`, whose fragment a search swaps in. Every sentence it shows is
  composed server-side, the too-large one through `importFailureLine`.
- A drop refusal is pinned to the viewport, where the overlay it replaces
  was, and the live region is the `drop__errors` wrapper, which is never
  hidden. In the flow at the top of `<main>` the sentence renders above
  whatever a scrolled grid is showing, and the two client-side refusals
  have no other feedback; a live region toggled from `hidden` is not
  reliably announced, where a change inside a standing one is.
- `uploading` is cleared by a bfcache `pageshow` **and** by Escape. A
  submit the person cancels aborts the navigation without unloading the
  document, so no `pageshow` follows it, and nothing else takes the veil
  down or unlocks the drops that `uploading` locks out.
- The write probe runs once at startup. A per-request check would answer
  differently only when confirm is about to report its own error.
- A repeated confirm answers the first call's book id rather than copying
  the file in again, which is why the record outlives the confirm.
- `internal/importer` imports `internal/scanner`, so `internal/scanner`'s
  in-package tests cannot import `internal/service`. `capmetadata_test.go`
  is `package scanner_test` over `export_test.go` for that reason; it may
  not live in `package scanner`.
- `cmd/server`'s `TestMain` points `TMPDIR` at a directory of its own,
  because `run()` derives the staging directory from `os.TempDir()` and
  `importer.New` wipes it — without that, the package's tests delete a
  development server's staged uploads.

### Web and service (`docs/notes/web.md`)

- A fragment is answered when `HX-Request` is present **and**
  `HX-History-Restore-Request` is absent. `Vary` names both.
- **A rejection answers 422 wherever it has a body to show** — an edit, an
  import upload and confirm, and a send address, on both paths. The
  `htmx-config` meta tag in `document-head` keeps `4xx` and `5xx` in
  `noSwap`, so an error swaps only when the element that asked names it:
  every form whose route answers 422 carries
  `hx-status:422="swap:outerHTML"`, `send__form` and
  `enrich__form` also carry `hx-status:503="swap:outerHTML"` for the
  disabled control their route answers with, and every page's `<body>`
  carries the 403 one. A route that gains an error body, 4xx or 5xx, gains
  the matching `hx-status` on the element that asked, or the rejection
  never shows. The opt-in names the exact status, never a wildcard:
  `noSwap` is consulted before the element at each of the three steps, so
  an opt-in on a wildcard `noSwap` itself lists is unreachable.
- That tag sets `includeIndicatorCSS` false: nothing carries
  `htmx-indicator`, and every indicator is a rule of this app's own keyed on
  `htmx-request`.
- The request timeout is htmx's own 60s everywhere but the two import forms,
  which carry `hx-config="timeout:0"` because an upload's window scales with
  `MAX_IMPORT_SIZE` and a confirm's is derived from `importer.IndexTimeout`.
  Never move that back to `defaultTimeout` on the meta tag: a hung search or
  status poll would then have no bound and leave its indicator up.
- Both import forms and the discard form carry `hx-disable="find button"`.
  The dimmed button is appearance only — a focused one still answers Enter,
  and htmx queues the second submit rather than dropping it.
- The search input carries `hx-sync="this:replace"`. htmx queues one request
  per element and drops the rest, and a queued one carries the query it was
  built with, so without this the grid settles on an older string than the
  box holds — and the box is never re-rendered to say so.
- Every import route that can answer a fragment names both htmx headers in
  `Vary`, `GET /import` included. Every refused upload drains what is left of
  the body first.
- `maxMetadataFormBody` is `3 × service.MaxMetadataValueBytes + 1024`,
  sized off the author list, not the description.
- `libraryPage.SearchMaxLength` must carry `storage.MaxSearchBytes`; the
  input's `maxlength` is what bounds the request, the handler's
  `NormalizeSearchQuery` only makes the rendered query the searched one.
- A new search rebuilds the grid including its paging trigger, so paging
  resets by construction. `MoreLabel` empty means no trigger.
- Every read affordance carries both `href` and `hx-get`, every editor both
  `action` and `hx-post`. One markup path; no separate no-JS path.
- Every state-changing route is wrapped in `sameSiteOnly`, and
  `cmd/server` wraps the whole handler in `fetchMetadataGuard`. A refusal is
  a 403 for every client; an htmx fragment caller's carries the
  `fetch-metadata-refused` partial, which every page template's `<body>`
  swaps in through `hx-status:403:inherited="swap:afterbegin"`. `next` is
  not called in either shape. `sameSiteOnly` itself passes an *empty*
  `Sec-Fetch-Site` through on purpose; the opt-out mode depends on that.
- Every route that renders the send control copies `SendableNote`, so a
  fragment can never offer a button the full page withholds.
- The upload and confirm routes extend their own deadlines through
  `http.NewResponseController`, **both halves**; `cmd/server`'s timeouts are
  never loosened for the other routes. Go installs the write deadline once,
  when the request headers are read, so a read window widened for a large
  body sits inside a write deadline that expired while it was arriving — the
  import lands and its answer never reaches the browser. `confirmWindow` is
  derived from `importer.IndexTimeout` rather than restating it.
  `http.MaxBytesReader` bounds the body, `importer`'s own count bounds the
  file, and only the second is the number a refusal names.
- `providerSourceNote` renders a marker for a provider's name and nothing
  for `embedded`, `manual` or absent. Editing clears the marker because the
  POST handler reloads the book rather than echoing the input.
- Polling stops by construction: only the pending block carries
  `hx-get`/`hx-trigger`.
- `.button--primary`'s foreground stays `var(--bg-raised)`, never `#fff`
  (2.9:1 in dark theme). `--md`/`--lg` reset `min-height`;
  `.button--tertiary:disabled` beats `.button:disabled` on source order.
- `Service.now` is the clock for every timestamp the service writes;
  `relativeTime(t, now)` takes `now` as a parameter.
- Sending and enrichment stay two parallel surfaces (state, latest,
  shaping). Do not abstract over exactly two cases.

## Conventions

- Handlers parse the request, call one service method and render. Text the
  page shows is composed in the handler (`searchSummary`,
  `enrichmentResultLine`, `historyStatus`), never formatted in a template.
- Absent is not an error: finders return `nil, nil`, updates return
  `(false, nil)`, for an unknown id. The transport turns that into a 404.
- A test that waits on time runs in `synctest.Test`, and a test server is
  `httptest.NewTestServer` on its in-memory network
  (`docs/notes/testing.md`). A database is `storagetest.Open(t)`, never
  `storage.Open` on a fresh path. Three exceptions, each with its own
  reason: a test about opening a database; `cmd/server`, which stays on
  `storage.Open` throughout; and `storage`'s own tests, which cannot import
  `storagetest` and so carry the same template in `books_test.go`. Inside
  a bubble the database is opened with
  the bubble's `t`, and code a bubble runs owns no goroutine that outlives
  its caller. The server's `Client()` sends every host to it, so tests
  never read `URL` before `Client()`, name fixed hosts under `.test`, keep
  one server per client dispatching on `r.Host`, and hand a production
  client `server.Client().Transport` itself, never a clone. `Start` is
  for a test that needs a real dial — the cover address guard, the import
  refusals; the `Worker`'s remaining cover tests share that one loopback
  server rather than standing up a second.
- Provider-client fixtures live under `testdata`. A fixture is a live
  capture or is labelled otherwise at the top of its test file.
- `go test ./...` must pass; CI also runs `go vet` with `-race` and builds
  the image.
- A `v*` tag publishes: `.github/workflows/publish.yaml` runs the tests, builds
  `linux/amd64` and `linux/arm64` on a runner each, pushes both to
  `ghcr.io/meandnano/applibris` by digest, joins them into one manifest list
  tagged with the version, and creates the GitHub release from the commits
  since the previous tag. A version is `vMAJOR.MINOR` with an optional
  `-suffix` for a prerelease, and the image tag is that with the `v`
  dropped — `v0.1` publishes `applibris:0.1`. The tag is matched by regex
  (`type=match`) and not parsed as semver, which emits no tags at all for
  a version this short; the first job refuses a tag of any other shape so
  a typo cannot reach the registry.

## Documentation

CLAUDE.md, README.md and `docs/notes/` describe the **current state of the
code and why it is that way**. They are not a changelog. Git history,
`docs/plans/completed/` and pull requests already record how the code got
here, and a second copy of that record in prose goes stale, grows without
bound and buries the rules a reader actually needs.

Concretely, when writing or editing any of these files:

- Describe what the code does now and the reason it must stay that way.
  Never describe what it did before, what a plan proposed, what a review
  found, or what was tried and removed. Phrases like "used to", "no longer",
  "first built as", "the plan said", "was corrected", "before this change"
  are the signal to delete the sentence or rewrite it in the present tense.
- A rejected alternative may be mentioned only as a present-tense reason
  the current design is right, and only when the code cannot show it:
  "the comparison carries no explicit `COLLATE NOCASE`, since an explicitly
  collated expression is no longer the indexed one" is a rule; "the
  comparison used to carry `COLLATE NOCASE` until review found it scanned"
  is history.
- Do not cite plan files, PR numbers or commits from these documents. A
  backlog file may be cited for a current known limit, since it describes
  the code as it stands.
- When a change makes a sentence untrue, replace the sentence. Do not
  append a correction beneath it; that is the exception below, and it
  belongs to plans only.

The one place history is kept deliberately is `docs/plans/completed/`,
where a correction found while implementing a plan is appended rather than
rewritten (see Planning). A completed plan is a record of a decision at a
point in time, which is exactly what these three documents are not.

## Planning

Each implementation step is planned in its own file under
`docs/plans/<YYYYMMDDNN-description>.md` (e.g.
`docs/plans/2026083001-covers.md`) — `NN` a same-day sequence number, same
scheme as the migration filenames. Once a plan's step has been implemented,
move its file into `docs/plans/completed/`.

Plans in `docs/plans/completed/` are immutable: never edit one after it's
moved there, even to fix a mistake found later. If a problem is discovered
in a completed plan, write a new plan for the fix instead of rewriting the
old one.

The one edit a plan may take on its way *into* `completed/` is a
**correction found while implementing it**, in the same commit as the
move — because a plan whose instruction the implementation had to
contradict is misleading to anyone who later reads the two side by side.
Such an edit must **append**, never rewrite: leave the wrong instruction
standing, and add a block below it saying what was tried and what refuted
it. Rewriting it silently produces a plan that appears to have been right
all along, which git cannot distinguish from the honest version — the
rename shows as one similarity score either way, so the discipline is the
only thing separating them.
`docs/plans/completed/2026090608-googlebooks-live-fidelity.md`'s cover-size
block is the worked example.

Where a completed plan and the code disagree, the code and `docs/notes/`
are right, and the disagreement is not recorded in the notes.

## Backlog

`docs/backlog/` holds known work that is worth doing but isn't top
priority — things that don't corrupt data, don't block another step, and
aren't visibly wrong in the shipped app today. Anything that *does* meet
one of those bars belongs in `docs/plans/` instead, not here.

Backlog files use the same `<YYYYMMDDNN-description>.md` naming as plans,
sharing one same-day `NN` sequence with them so a number identifies exactly
one file across both directories.

A backlog item is a **problem statement, not an approved plan**: it records
what's wrong and why it was judged non-urgent, with only a sketch of a fix.
It is never implemented directly. To act on one, first re-validate it
against the current code — the finding may have been fixed in passing,
changed shape, or become urgent since it was written — then write a real
plan under `docs/plans/` and delete the backlog file in the same change.
