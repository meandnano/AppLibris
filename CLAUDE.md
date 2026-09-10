# library

@README.md

README.md, imported above, says what the app does and how it is configured.
This file maps the code and lists the rules that are easy to get backwards.
The reasoning behind each area lives in `docs/notes/`; read the relevant
note before changing that area, and put new rationale there rather than
here. Notes describe the current design only, never its history.

## Code map

- `cmd/server` — entrypoint. Reads configuration, resolves every configured
  path through `resolveDir` (create, then `EvalSymlinks`; a dangling link in
  any component fails startup naming link and target), opens the database,
  serves immediately, and runs the scan loop, the sender worker and the
  enrichment worker on one cancellable `scanCtx`. Shutdown order: HTTP
  server, then `waitForBackground` (10s), then the database. Notes:
  `docs/notes/scanner.md`, `docs/notes/design.md`.
- `internal/storage` — SQLite (`modernc.org/sqlite`, WAL, foreign keys,
  5s busy timeout). Bounded read pool, single-connection write pool
  (`DB.Write`). Embedded migrations under `migrations/`, one statement per
  file. Tables: `books`, `authors`, `book_authors`, `book_files`,
  `field_sources`, `books_fts`, `recipients`, `send_log`,
  `enrichment_jobs`. Search sanitisation (`SanitizeFTSQuery`,
  `NormalizeSearchQuery`) and the keyset cursor (`BookPage`) live here.
  Note: `docs/notes/storage.md`.
- `internal/epub`, `internal/fb2` — embedded metadata and cover bytes from
  each format, same `Metadata` shape so the scanner treats them alike.
  `internal/cover` — resize to ~400px, JPEG, atomic write into
  `COVERS_DIR` keyed by content hash. All three cap what they read from an
  untrusted file. Note: `docs/notes/formats.md`.
- `internal/scanner` — walks `LIBRARY_DIR`, syncs it into storage
  (`Scan`), reconciles missing files in two phases, regenerates covers, and
  hosts the fsnotify watcher (`watcher.go`) plus the startup mount and
  delivery checks. Note: `docs/notes/scanner.md`.
- `internal/resend` — one-attachment `Client.Send` against Resend's API.
  `internal/sender` — the single `Worker` over `send_log`. Note:
  `docs/notes/sending.md`.
- `internal/enrich` — the enrichment `Worker`, the `Provider` interface,
  the pure `Resolve` function and `plausibleMatch` (`match.go`), the
  three decorators (`decorator.go`) and `FetchCover`.
  `internal/openlibrary`, `internal/googlebooks` — the two providers.
  `internal/providers` — the name → constructor registry
  (`METADATA_PROVIDERS`). Note: `docs/notes/enrichment.md`.
- `internal/service` — the layer beneath the HTTP handlers, so a future
  `/api/v1` is a second thin transport. Owns validation and normalisation
  (`UpdateBookMetadata`, `QueueSend`), page assembly (`BookSummary`,
  `BookDetail`, `SearchResult`), and the `Notify`/`NotifyEnrichment`
  function fields `cmd/server` wires to the workers. Note:
  `docs/notes/web.md`.
- `internal/web` — `html/template` pages and htmx fragments, CSS and the
  vendored htmx under `static/`, all `go:embed`ded. Routes: `GET /{$}`
  (grid, search, paging), `GET /books/{id}`, `GET`/`POST
  /books/{id}/metadata/{field}`, `POST /books/{id}/send`, `GET
  /books/{id}/sends/{sendID}`, `POST /books/{id}/enrich`, `GET
  /books/{id}/enrichment/{jobID}`, `POST /recipients/remove`,
  `GET /history`, `/static/`, `/covers/`. The UI is translated from mockups
  kept as `UI.md` and `ui-handoff/` on the `init` branch. Note:
  `docs/notes/web.md`.
- `docs/notes/design.md` — purpose, constraints, the library-directory
  rules, the conversion model behind `derived_from`, and the deferred list.
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
- `SearchBooks` and `ListBooks` order by `(sort_title, id)`; the cursor
  comparison stays in row-value form with **no** explicit `COLLATE NOCASE`,
  or the index seek becomes a scan.
- `MaxSearchBytes` (256, cut on a rune boundary) and `maxSearchTerms` (16)
  are both needed; neither subsumes the other.
- `send_log.book_id` is `ON DELETE SET NULL` with `book_title`
  denormalised, and `recipient_address` is a plain string. Every other FK
  cascades. History reads those columns and never joins `books` or
  `recipients`.
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
  it.
- `cover.MaxCoverBytes` (8 MiB) and `enrich.MaxCoverBytes` (512 KiB) are
  separate constants, never aliases. Google Books' cover-size choice is
  calibrated against the second.
- `maxPixels` (16 MP) is sized off `image/jpeg`'s progressive-scan
  allocation, not pixels × 4. Do not raise it on the RGBA reasoning.
- `cover.Store` wraps `ErrUnsupportedCover` for anything the bytes decide
  and leaves filesystem errors unwrapped; the scanner's retry logic depends
  on that split.
- Publication date is the edition's: never `creation`/`modification` in
  EPUB, never `title-info/date` over `publish-info/year` in FB2.
- FB2's declared charset is passed through unchanged (`CharsetReader`).
- Cover files are named by the *book's* content hash, not the thumbnail's
  bytes, so `/covers/` is never served `immutable`.

### Scanner (`docs/notes/scanner.md`)

- Identity is the content hash; path is an attribute. Known content at a
  new path is a `book_files` row, not a new book. `file_path` is relative to
  `LIBRARY_DIR`, slash-separated.
- The watcher never reads, hashes or parses a file. It pokes the one scan
  goroutine, so two sweeps can never overlap and correctness never depends
  on an event arriving.
- **An unknown is not evidence.** Only `fs.ErrNotExist` means gone, for
  missing rows, cover files and symlink targets alike; any other error
  leaves the row or book untouched and warns.
- `readEmbeddedCover` runs first; `field_sources` is consulted only after
  re-extraction has come back empty. Both cover-forgetting writes are
  guarded on the `cover_path` this sweep observed.
- `cover_retry` marks a transient store failure only. A decode failure
  (`cover.ErrUnsupportedCover`) is recorded as no cover and never retried.
- `PruneMissingFiles` filters nothing; every prune guard lives in the
  scanner. A row under a top-level directory that yielded no book files
  this sweep is marked but never pruned; a sweep that saw zero files
  reconciles nothing; a row under a directory the walk could not read is
  left alone at both phases.
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
- Only `fs.ErrNotExist` means the file is gone; other stat or read errors
  record "could not read the file", and an index error records its own
  reason. Never fold the three together.
- `last_used_at` is bumped at enqueue, not delivery. `Service.Notify` fires
  only when `EnqueueSend` reports `inserted`.
- Only formats Amazon accepts are offered (`sendableFormat`, today `epub`);
  `QueueSend` does not refuse others, the button is simply not rendered.

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
  `Asked > 0 && Failed == Asked`, or a lost cover with nothing else written.
  "Nothing to add" is a success.
- `sanitizeValue`'s limits equal `internal/service`'s (1024 for a title and
  a name, 4096 for other scalars, 64 KiB for a description, 100 names), or
  a provider-written value becomes uneditable.
- Providers name a cover URL and never download it. The worker fetches
  under `enrich.MaxCoverBytes` with the scheme checked on every redirect hop.
- Compose `WithCache(WithRetry(WithRateLimit(p)))`: cache outermost, rate
  limit innermost.
- Open Library `ByISBN` uses the edition-scoped Read API; `Search` answers
  about works and returns neither language nor publication date.
- Google's `best()` prefers `medium` and omits `extraLarge`. Never rewrite
  a thumbnail URL's `zoom` parameter. `internal/googlebooks` refuses a
  redirect that leaves the host (`sameHost`), because the key travels in
  the query string and `Referer` would carry it.
- Fixtures are live captures except `internal/openlibrary`'s
  `search_*.json`; each test file says which. Never hand-edit a fixture to
  make a test pass.

### Web and service (`docs/notes/web.md`)

- A fragment is answered when `HX-Request` is present **and**
  `HX-History-Restore-Request` is absent. `Vary` names both.
- **A rejected edit fragment answers 200; the rejected full page answers
  422.** htmx 2.0.10 does not swap a 4xx. Do not opt 422 in from the client.
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
  `cmd/server` wraps the whole handler in `fetchMetadataGuard`. An htmx
  fragment refusal is a 200 with the `fetch-metadata-refused` partial and
  `HX-Reswap: afterbegin`; every other client gets 403. `next` is not
  called in either shape. `sameSiteOnly` itself passes an *empty*
  `Sec-Fetch-Site` through on purpose; the opt-out mode depends on that.
- Every route that renders the send control copies `SendableNote`, so a
  fragment can never offer a button the full page withholds.
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
- Tests for the provider clients run against `httptest.Server` with
  fixtures under `testdata`. A fixture is a live capture or is labelled
  otherwise at the top of its test file.
- `go test ./...` must pass; CI also runs `go vet` with `-race` and builds
  the image.

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
are right. The notes record the current design, not the path to it, so a
superseded plan decision is not carried into them.

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
