# library

A self-hosted ebook library server: browse a book collection and send titles
to a Kindle by email. See [DESIGN.md](DESIGN.md) on `init` branch for the
full design.

## Current implementation

- `internal/storage` — SQLite storage layer (`modernc.org/sqlite`). WAL mode,
  foreign keys on. A read pool (`DB.Read()`), bounded to `readPoolSize` (8)
  open and idle connections so concurrency above `database/sql`'s default
  idle-2 ceiling reuses pooled connections instead of opening and
  discarding a fresh one per request, and a single-connection write pool
  (`DB.Write(ctx, fn)`) serialize writes without a hand-rolled
  goroutine/channel. Each exported write method owns one transaction; a
  multi-step atomic write composes the package-internal `…Tx` helpers (e.g.
  `createBookTx`) inside a single `DB.Write` call instead of calling two
  exported methods — a `Write` callback must never call an exported `*DB`
  method, since the pool's one connection is already held by the outer
  call, and nesting deadlocks until the context expires. A directly nested
  `Write` call is instead caught and returns `ErrNestedWrite`. Migrations
  are embedded SQL files under
  `internal/storage/migrations/`, one statement per file, named
  `YYYYMMDDNN_description.sql`, applied in filename order inside individual
  transactions and tracked in a `schema_migrations` table. `storage.Open` is
  idempotent — safe to call on every process start. `migrate` iterates the
  embedded *files* and skips those already recorded, so a
  `schema_migrations` row naming a file that no longer exists is inert
  rather than an error — which is what makes deleting a migration a safe
  change while the project is pre-deployment.
- Schema so far: `books` (identity and metadata; no location fields),
  `authors`, `book_authors` (join table), and `book_files` — one row per
  physical file location, keyed by `book_id`, so byte-identical content at
  more than one path is one `books` row with multiple `book_files` rows
  rather than a single mutable `file_path`. `books.sort_title` is derived
  from the title rather than a copy of it — one leading English article
  stripped, case folded — and the column is declared `COLLATE NOCASE`, so
  any `ORDER BY sort_title` is case-insensitive without a per-query clause.
  `book_files.book_id` and both `book_authors` foreign keys are `ON DELETE
  CASCADE`, so deleting a book takes its file locations and author links
  with it (the author row itself survives). `book_authors` carries a
  `position` so a book's authors keep the order its source file listed
  them in — `author_id` order is first-sight-in-the-library order, which is
  not the same thing once an author is shared between books. A name
  credited twice in one file links once, at its first position, rather
  than failing the insert. `authors.name` has a unique
  index. Every timestamp column is written as fixed-width UTC RFC 3339 text
  (`sqliteTimeLayout`/`formatTime` in `internal/storage`), so SQLite's own
  date functions and a plain `ORDER BY` both work on it.
- `field_sources` (`internal/storage/metadata.go`) records where each of the
  seven editable fields' current value came from — `embedded` when the
  scanner read it out of the file, `manual` once a person has edited it.
  Nothing reads it yet, and that is the point: DESIGN.md forbids shipping
  manual editing before provenance, because the provider-enrichment step
  that comes later would otherwise overwrite hand-fixed values with no way
  to tell they were hand-fixed. Writing it now is what makes every book
  edited in the meantime carry its marker when that consumer arrives.
  Provenance is set at creation (`setEmbeddedFieldSourcesTx`, called from
  `createBookTx` inside the same transaction as the book, its author links
  and its FTS row — an invariant, not a backfill-only repair) and on every
  edit. There is deliberately no backfill migration: the service has never
  been deployed, so no database exists that predates the table, and a
  migration whose `SELECT` can only ever match zero rows is a fixture
  pretending to be a guarantee. A local development database made before
  this table needs the same one-time reset `books_fts` already documents —
  delete the file and let the next sweep rescan.
  The rule that is easy to get backwards: **a cleared field stays
  `manual`.** An empty value with a `manual` source is a decision someone
  made, not metadata that is missing, and a resolver that infers
  provenance from emptiness would undo it. `UpdateBookField` writes one
  scalar, `UpdateBookAuthors` replaces the whole author list — dropping a
  repeated name at its first occurrence and keeping positions contiguous,
  the same rule `createBookTx` applies, because `book_authors` is keyed on
  `(book_id, author_id)` and a duplicate would otherwise violate the
  primary key and roll the whole update back — and both
  update the value, its provenance and the FTS row in one transaction —
  `UpdateBookField` refuses `authors` outright (`ErrInvalidMetadataField`)
  since that lives in a join table. Both return `(false, nil)` for an
  unknown book, the same absent-isn't-an-error contract as the finders.
  `SortTitle` also lives here rather than in `internal/scanner`, because
  two callers now derive that column — the scanner on first sight of a
  file, and a title edit — and a second copy of the rule is a library that
  sorts differently depending on how a title arrived.
- `books_fts` is an FTS5 virtual table (`title`, `authors`, `description`,
  `isbn`, `tokenize='unicode61 remove_diacritics 2'`) — a plain table, not
  `content='books'`, since `authors` isn't a books column to begin with
  (it's assembled from a join) and a contentless table can't support the
  delete-by-rowid the sync below needs. Sync is asymmetric on purpose:
  deletion is a trigger (`books_fts_after_delete`, since books die on
  several independent Go paths and every future one dies for free too),
  while insert/update is the package-internal `syncBookFTSTx(ctx, tx,
  bookID)` — recomputes the row from scratch via a `group_concat` join
  over `book_authors`/`authors` rather than tracking deltas, and runs
  inside the same `DB.Write` transaction as `createBookTx`, right after it
  (`CreateBook`, `CreateBookWithFile`) so the FTS row can see the authors.
  No other write method touches an indexed column, so nothing else calls
  it. No backfill migration exists or ever will — a row created after
  this migration is synced by construction, but a database that predates
  it needs a one-time manual reset (delete the file, let the next sweep
  rescan) before that guarantee holds, since a pre-existing `books` row
  never passes through `syncBookFTSTx` on its own. `SearchBooks(ctx, query)`
  joins against it and orders by `sort_title`, not relevance — see
  `internal/web` below for why. `MatchedSearchFields(ctx, query)` reports
  which of the four columns produced hits, for the results line that names
  them; it is one round trip of four `EXISTS`, each scoped with FTS5's
  `{col} : (expr)` filter — the parentheses are load-bearing, since
  without them the filter binds to the first term only. `query` must already be a valid FTS5
  `MATCH` expression; `SanitizeFTSQuery` (also `internal/storage`, no DB
  access) is the one place raw user input becomes one, by quoting and
  prefix-terming every whitespace-separated token so no input, however
  adversarial, can reach `MATCH` unescaped.
- Single-book lookups, for the detail page: `FindBookByID` (nil, nil on an
  unknown id, same contract as `FindBookByContentHash`); `ListBookFiles`
  (a book's own locations, ordered by `file_path`, `missing_since`
  surfaced — a targeted query, not a filter over `ListFilesUnder("")`,
  which loads every location in the library to serve one book's row);
  `ListAuthorsForBook` (one book's authors in source order, as an empty
  non-nil slice rather than an error when there are none — `ListBookAuthors`
  loads the whole library's author map, right for the grid page and wrong
  for a single book). `CountBooks` is a plain `count(*)`, independent of
  any search filter.
- `recipients` and `send_log` (`internal/storage/sends.go`) back
  send-to-Kindle. `recipients.address` is `COLLATE NOCASE` with a unique
  index on it — the same arrangement `books.sort_title` uses — so
  `CreateRecipient` (`INSERT … ON CONFLICT DO NOTHING` then a select) is
  idempotent across case: re-adding a known address returns the existing
  row rather than erroring, since that's a user slip, not a failure.
  `send_log.recipient_address` is a plain string, not a foreign key
  (deleting a recipient must never orphan or rewrite history), and
  `send_log.book_id` is `ON DELETE SET NULL` — the one non-cascading FK in
  the schema — with `book_title` denormalised beside it: every other FK
  here cascades because those rows are meaningless without their book, but
  a send log entry is *the record that a thing happened*, and the scanner
  deletes books routinely (orphan pruning, `PruneMissingFiles`). Cascading
  would erase the evidence a book was ever sent, defeating the log's
  purpose of answering "did I already put this on the Kindle?". `status`
  carries a `CHECK` closing it to the four states
  (`queued`/`sending`/`delivered`/`failed`) DESIGN.md defines — a typo in a
  Go constant fails at the write rather than producing a job no worker
  ever claims. `EnqueueSend` inserts the `queued` row and bumps
  `recipients.last_used_at` in one transaction (via package-internal
  `…Tx` helpers, per the `DB.Write` composition rule above) — the bump
  belongs at enqueue, not delivery, because "most recently used" means
  "the one I last chose," and a failed send must not silently reset the
  picker's default to a different address. `ListRecipients` orders
  `last_used_at DESC, address`; SQLite sorts `NULL` smaller than any
  value, so that ordering alone puts never-used addresses last with no
  `NULLS LAST` clause, and the picker's default becomes simply "the first
  option," with no ordering logic in the transport. `ClaimNextSend` is one
  transaction that selects the oldest `queued` row, flips it to `sending`
  with `started_at` set, and returns it — atomic even though today's
  single worker makes contention impossible, because the claim is the one
  place a second worker would corrupt. `MarkSendDelivered` and
  `MarkSendFailed` both scope their `UPDATE` to `WHERE status = 'sending'`,
  so a terminal row can never be rewritten by a late or duplicate call —
  the guard is a silent no-op `UPDATE`, not an error, by design.
  `FailInterruptedSends` fails every row still `sending` (startup recovery
  after a crash — see `internal/sender`) and never requeues: which side of
  the in-flight request a dead process failed on is unknowable, so
  requeueing risks a silent duplicate delivery, while failing surfaces the
  ambiguity and leaves retry a click away. `LatestSendForBook` is the
  detail page's initial-render lookup (`ORDER BY queued_at DESC, id DESC
  LIMIT 1`).
- `internal/epub` — reads embedded EPUB metadata (title, authors, language,
  ISBN, description, publisher, publication date) from the OPF package
  inside the zip, and extracts the declared cover image (EPUB3
  `properties="cover-image"`, falling back to EPUB2's `<meta name="cover">`)
  as raw bytes. Publication date prefers a `dc:date` tagged
  `opf:event="publication"` among repeated elements, falling back to the
  first event-less one (EPUB3's form); `creation`/`modification` dates are
  never used. ISBN is recognised via `opf:scheme="ISBN"`, a `urn:isbn:`
  identifier, or a bare ISBN-shaped one, in that order, and returned
  normalised (hyphens/spaces stripped, prefix stripped, trailing check
  digit upper-cased) rather than as found — it's the lookup key a future
  provider chain needs. Cover hrefs are percent-decoded (and any fragment
  stripped) before the zip lookup, since a manifest href is a URI
  reference.
- `internal/fb2` — reads embedded metadata from an FB2 document, mirroring
  `internal/epub`'s `Metadata` field set and surface exactly so the scanner
  can fill it from either source with the same code shape. `PublishedDate`
  prefers `publish-info/year` (when this *edition* was published, what
  `books.published_date` means) over `title-info/date` (when the work was
  *written*, per the FB2 spec), falling back to `date`'s `value` attribute
  then its text. Authors are given as structured `first-name`/`middle-
  name`/`last-name` elements, joined with a single space into the one
  display name the `authors` table stores — the one place FB2 offers more
  structure than the schema keeps. The cover is whichever `<binary>` the
  coverpage's namespaced `l:href="#id"` points at, base64-decoded. A
  document's declared XML encoding is never trusted: `ReadMetadata` sets an
  `xml.Decoder.CharsetReader` that passes every charset through unchanged,
  since the library's FB2 files are UTF-8 regardless of what they declare
  and a bare decoder fails outright (not gracefully) on any declared
  encoding it doesn't otherwise recognise. A `.fb2.zip` archive is parsed
  the same way after opening it and locating its one `.fb2` entry; zero or
  more than one is an error rather than a guess at which book it contains.
- `internal/cover` — turns a raw cover image into the stored thumbnail:
  resized to ~400px on the long edge (never upscaling), JPEG, written to a
  derived directory keyed by content hash. `Store` creates that directory
  on demand and writes through a same-directory temporary file plus atomic
  rename, so readers never observe a partial canonical cover.
- `internal/resend` — `Client.Send` POSTs one attachment to Resend's API
  (DESIGN.md's send-to-Kindle transport), enforcing the ~28MB size limit
  DESIGN.md derives before attempting a send, and setting a non-empty
  `text` body ("Sent from the library.") since Resend requires one of
  `text`/`html`/`react`. Its caller is `internal/sender`'s queue worker.
  `NewClient` builds its own `*http.Client` with `SendTimeout` (5 minutes)
  set, rather than `http.DefaultClient`, which has none — the exported
  constant covers the *whole* request including the attachment upload, not
  just a connect deadline, and `internal/sender` reuses it for the
  worker's own per-job context deadline, the mechanism that actually makes
  a send abandonable at shutdown. The whole attachment is still held in
  memory as raw bytes, a base64 string, the marshaled JSON body and a
  reader over it all at once (~3.3x `MaxAttachmentSize` at the ceiling) —
  deliberately unstreamed: `internal/sender` runs a single worker, so
  there's only ever one send in flight, making this a bound on the whole
  process rather than a per-request multiplier, and the pre-read stat
  check in `internal/sender` means the worst case only occurs for a file
  that will actually be sent. Revisit on measurement, not as a reflex.
- `internal/sender` — the send-to-Kindle queue worker: a single `Worker`
  claims `send_log` rows one at a time, in queue order, and hands each to
  a `Transport` (`*resend.Client` satisfies it without changes; the
  interface lives here, on the consumer side, since `internal/resend`
  deliberately has no `Sender` interface of its own). `Run` wakes on
  either a `Notify` poke (non-blocking, capacity-1 channel — a burst of
  enqueues coalesces into one wake-up) or a once-a-minute `pollInterval`
  tick, the same poke-plus-tick shape as the scanner's watcher-versus-
  periodic-rescan split: the poke is the optimisation, the tick is the
  mechanism that catches anything a poke missed (a row left `queued` by a
  crash between insert and notify, most obviously). A book's file is
  resolved at *send* time, not enqueue time — `ListBookFiles`, first
  location whose `missing_since` is `NULL` — since a queue is a promise to
  act later and the library moves underneath it; no such location (the
  book was pruned, or every copy is currently missing) fails the job with
  a fixed "the file is no longer in the library" reason. A failure to read
  the index *itself* is reported separately ("could not read the library
  index — try again"), never folded into that one: a storage error says
  nothing about whether the book is still there, and claiming otherwise is
  a confident lie about a file that is probably fine. Before reading
  the file, its size is stat'd against `resend.MaxAttachmentSize`, failing
  with both sizes named ("14.2 MB exceeds the 28 MB limit") so an
  oversized file is never loaded into memory and the failure reason always
  has the numbers; the transport call itself runs under a per-job
  `context.WithTimeout(ctx, resend.SendTimeout)`. A transport error's text
  is recorded verbatim (truncated to `maxFailureReason`, 500 bytes, on a
  UTF-8 boundary) as the
  failure reason — Resend's API errors already read as sentences. Two
  rules are easy to get backwards and are deliberate: **interrupted jobs
  fail, they never requeue** — a job in flight when `Run`'s `ctx` is
  cancelled is left `sending` in the database (the process may or may not
  have reached the transport; guessing risks a silent duplicate delivery),
  recovered at next startup by `cmd/server` calling
  `storage.FailInterruptedSends` before the worker starts; and **retry is
  a new row** — the retry button re-posts the same form, calling
  `EnqueueSend` again rather than mutating the failed row back to
  `queued`, so the log keeps the fact that the first attempt failed, and
  `MarkSend*`'s `status = 'sending'` guard never has to reason about an
  in-place transition out of a terminal state.
  The line those two rules are drawn along is *whether the outcome is
  known*, and the terminal writes follow it: a send Resend accepted, and a
  failure decided locally (file gone, oversized, unreadable, index
  unreadable, or a transport answer that is a rejection), are both
  definite, so `MarkSendDelivered`/`fail` write under
  `context.WithoutCancel` plus a short `markTimeout` — a shutdown landing
  in that gap must not cost a verdict already reached, or recovery
  rewrites a delivered book as failed and invites a duplicate send. Only
  the genuinely unknown case, a transport call abandoned mid-flight (its
  error arrives with `ctx` already cancelled), skips the write and leaves
  the row `sending` for startup recovery to surface.
- `internal/scanner` — walks the library directory and syncs it into
  `internal/storage`. Cheap path+size+mtime check (against `book_files`)
  skips unchanged files; content hash (SHA-256) is a book's identity, so
  known content found at a not-yet-seen path gets an additional
  `book_files` row rather than a new book — this covers a moved/renamed
  file and a genuine duplicate location alike, since the two are
  indistinguishable from a single path's perspective. New EPUB files get
  embedded metadata and a stored cover via `internal/epub`/`internal/cover`;
  FB2 files (plain `.fb2` and `.fb2.zip` archives alike) get the same
  treatment via `internal/fb2`, both recording `format` as `fb2` regardless
  of which — how a book is packaged on disk isn't something the format
  badge in the UI should surface. Supported files are matched on filename
  *suffix* rather than `filepath.Ext`, since a `.fb2.zip` archive is two
  extensions and `Ext` would only ever see the last one. For known content,
  a sweep re-extracts a recorded cover whose file is missing or zero bytes
  and refreshes its stored path, making `COVERS_DIR` disposable. An empty
  stored cover path records that no embedded cover was found and is not
  retried on every sweep; a separate `cover_retry` marker records a
  transient initial store failure and retries it later. Cover inspection
  regenerates only on a missing or zero-byte file; other stat failures warn
  without re-parsing the source. A new book is created together with its
  first file location in one transaction (`storage.CreateBookWithFile`).
  Content replacing a known path's previous content reassigns that
  `book_files` row and, in the same transaction, deletes whatever book it
  left with zero locations (`storage.ReassignFileAndPruneOrphan` /
  `CreateBookWithFile` — either can orphan a path's previous owner, since
  both reassign it unconditionally), logging the deletion at Info.
  `book_files.file_path` is stored relative to `LIBRARY_DIR`
  (slash-separated), so the index survives the library being mounted at a
  different absolute path (dev's `./library` versus a container's
  `/library`); a directory `WalkDir` can't read skips its subtree and
  counts an error rather than aborting the rest of the sweep. A
  `book_files` row whose path goes missing is reconciled in two phases:
  first marked (`missing_since`, via `storage.SetFilesMissing`) rather than
  deleted outright, then actually deleted (`storage.PruneMissingFiles`,
  taking its book with it via the same orphan-pruning path as a reassigned
  file if it was the book's last location) once it's stayed missing past
  the `MISSING_GRACE` duration — a row that reappears before then has its
  mark cleared (`storage.ClearFilesMissing`) instead. `storage.
  PruneMissingFiles` does no filtering of its own (no age check, no path
  exclusion) — it deletes exactly the file IDs it's given; every guard
  below lives in the scanner, the only place with access to live
  filesystem state, and decides that ID list. Guards that keep this from
  misreading a transient failure as deletion: a row is only eligible if
  `os.Lstat` on it fails with `fs.ErrNotExist` specifically (any other
  error, e.g. a path component that's no longer a directory, only logs a
  warning), and this check runs fresh *every* sweep — including for a row
  already marked from an earlier sweep — so a row whose failure mode
  later changes (`ErrNotExist` to `EACCES`, say, or to a directory now
  sitting at that path) can never be deleted on the strength of a
  confirmation that's since gone stale; a row under a directory the sweep
  couldn't read *this* sweep is left untouched, at both mark and prune
  time (`Scan` tracks these as `skippedDirs`, a negative list — `WalkDir`'s
  callback only ever reports directory-read failure as a second,
  error-bearing invocation, so a positive "cleanly read" list isn't
  obtainable from the API); and reconciliation is skipped entirely if the
  sweep visited zero files (an unmounted volume can present as an empty
  directory, so seeing nothing is not evidence that everything is gone),
  logged at Warn.
- The filesystem watcher (`internal/scanner/watcher.go`) is a *trigger*,
  not a second index path: it never reads, hashes or parses the file an
  event names, it only pokes a capacity-1 channel that `cmd/server`'s one
  scan goroutine selects on beside its ticker. So a sweep is a sweep
  however it was woken, two can never overlap, and nothing about the
  index's correctness depends on an event arriving — DESIGN.md makes the
  rescan the mechanism and the watcher an optimisation, and a watcher
  that never fires costs only latency. Events are debounced (`WATCH_SETTLE`,
  default `5s`) because an event says something changed, not that it
  finished changing: a copy fires `CREATE` long before its last byte
  lands. One timer covers both bounds — the settle window handles a burst
  that ends, and `watchMaxDelay` (60s) handles one that doesn't, poking on
  the first event past the cap so a bulk import can't hold the debounce
  open forever. The debounce is a quality measure, not a correctness one:
  a sweep that does catch a partial file indexes it, and the completing
  write's changed size/mtime makes the next sweep re-hash and
  orphan-prune it in one transaction. Only events worth a sweep qualify —
  a supported suffix, any remove/rename (the name may already be gone,
  and a book leaving is what missing-file reconciliation wants), or a new
  directory — so a `.part` file growing during a download provokes
  nothing until it is renamed into place. `Refresh` re-registers the
  watch set after every sweep, which covers two measured failure modes:
  inotify is not recursive so a new subdirectory needs its own watch, and
  a watch is dropped *silently* when its directory is deleted. It cannot
  cover the third — losing *every* watch, when the library directory
  itself is replaced by an unmount and remount — because a sweep only
  calls `Refresh` after something pokes it, and a watcher with no watches
  can never poke again; that leaves it deaf until the periodic rescan
  happens along, which was reproduced by deleting and recreating the
  library directory under a running server. So `Run` also carries a
  `watchRecheckInterval` (30s) ticker that rebuilds the set once
  `WatchList()` is empty — a length check while healthy, a walk only when
  there is nothing left to lose — and pokes a sweep when it succeeds,
  since whatever changed while it was deaf is still unindexed.
- Two startup checks report what the watcher can actually see, because
  neither is observable later: `mountFor` names the filesystem backing
  `LIBRARY_DIR` from `/proc/self/mountinfo` (absent on a macOS dev box —
  a Debug line, not a warning), and warns for the types where changes
  routinely happen *behind* the mount rather than through it (`fuse.*`,
  `nfs`, `cifs`, `9p`). That distinction is the whole Unraid story and was
  measured against a FUSE passthrough: a file written **through** the
  mount produces `CREATE`+`WRITE`, one written **directly into the
  backing store** produces nothing at all, though a later `readdir` sees
  it. So an SMB copy to a `/mnt/user` share is seen and the mover
  shuffling between `/mnt/cache` and `/mnt/diskN` is not — and bind-mounting
  the disk path instead makes every change local. Since `fsnotify.Add` on
  such a mount returns *no error*, a dead watch is indistinguishable from
  an idle one, so a delivery probe creates `.watch-probe-<pid>` in the
  library root and waits for its own event; silence is one Warn. The probe
  file is excluded from `qualifies` (it must not trigger the work it
  tests), is removed unconditionally including on the timeout path, and a
  read-only library is an Info-level skip rather than a failure.
- The mover needs no handling at all, which is worth keeping true: it
  preserves path, size and mtime, so the cheap check skips those files,
  and the inode and physical disk it does change are not things this index
  stores. That is the reason not to add inode tracking or a device-id
  column later.
- `internal/service` — the layer beneath HTTP handlers DESIGN.md's
  "Layering for a future API" calls for, so a future `/api/v1` can reuse it
  as a second thin transport alongside `internal/web`. `ListBooks` and
  `SearchBooks` both assemble `internal/storage`'s books and authors into a
  `BookSummary` per book via a shared unexported `summarize` helper.
  `SearchBooks` sanitizes via `storage.SanitizeFTSQuery` first and returns
  a `SearchResult` — the books, whether a search actually ran, and which
  indexed fields matched. A query that sanitizes to nothing is treated as
  `ListBooks`, so the empty search box and a freshly-loaded page are the
  same state and callers don't special-case it; `Searched` reports which
  of the two happened, because "sanitizes to nothing" is wider than "looks
  blank" (control characters are stripped, so `?q=%00` is a non-blank
  query that is nonetheless no search) and a transport deriving its own
  flag from the raw query would render a result count over the whole
  unfiltered library. `Fields` is fetched only when something matched —
  with no results there are no fields to name, and the no-matches state
  names the searched fields itself. `CountBooks` returns the library's total size,
  independent of any search filter — what the masthead shows, kept
  separate from however many a search matched. `GetBook` assembles one
  `BookDetail` for the detail page (nil, nil on an unknown id, same
  absent-isn't-an-error contract as the storage finders — the web handler
  is what turns that into a 404). `BookDetail.FileSize` is a book-level
  field, not per-location: every location of one book is byte-identical by
  construction (content hash is identity), so it's taken from the first
  `ListBookFiles` row rather than carried per location, where showing a
  size per path would imply a difference that cannot exist. A book with
  zero locations can't actually be observed — the last location's deletion
  prunes the book in the same transaction — but `GetBook` treats that race
  as survivable anyway, rendering no size rather than erroring.
  `HasFileSize` is what carries that: without it a book with no location
  is indistinguishable from one whose file is genuinely zero bytes, and
  the page claims `0 B` for a size it doesn't know.
  `Service.now` is the package's clock (a private `func() time.Time`,
  defaulted by `New`, overridden in tests), and every write that stamps a
  timestamp goes through it rather than reaching for `time.Now` — which is
  what lets `modified_at` propagation be asserted here without a timing
  assumption.
  `UpdateBookMetadata` is the editing entry point: it maps a field name
  onto `storage`'s enum, normalizes the submitted value (trimmed; title
  required; per-field byte limits, with `MaxDescriptionBytes` exported
  because `internal/web` sizes its request-body cap from it), writes it,
  and returns the reloaded `BookDetail` rather than an echo of the input —
  so the caller renders canonical data and normalization is visible. The
  `authors` field takes a different path from the six scalars:
  `normalizeAuthors` splits on newlines, trims, drops blanks and links a
  name credited twice only once, since the textarea is free text and a
  repeat is a slip. Description is the only multiline field; every other
  scalar rejects an embedded CR or LF rather than storing one. A browser
  text input cannot produce one, but the check lives here because this is
  also what a future API calls, and a stored line break breaks every
  single-line rendering downstream. A rejected value is a
  `metadataValidationError`
  wrapping `ErrInvalidMetadata`, carrying the sentence the field shows —
  `MetadataValidationMessage` unwraps it — so a bad value is a field
  error, never a 500. An unknown book id returns `nil, nil`, `GetBook`'s
  contract.
  The send-to-Kindle surface follows the same layering: `Recipients`
  shapes `storage.ListRecipients` into picker options; `QueueSend` is
  where the business rules live, per DESIGN.md's "parse request, call
  service method, render" note for handlers — it validates the address
  with `net/mail.ParseAddress` and stores only `addr.Address` (so a pasted
  `"Mike <mike@kindle.com>"` saves the mailbox, not the display name),
  returning `ErrInvalidAddress` and queueing nothing on a parse failure;
  reads the book directly via storage rather than through `GetBook` (whose
  authors/file-location joins a title snapshot has no use for), returning
  `nil, nil` on an unknown id, `GetBook`'s own contract; then saves the
  recipient (idempotently) and calls `EnqueueSend`. `Service.Notify
  func()`, set by `cmd/server` to the worker's `Notify` method (nil in
  tests and whenever sending is unconfigured), is called once a send is
  successfully queued — a function field rather than an interface, since
  `internal/service` depending on `internal/sender` (which depends on
  `storage`) would be a cycle. `SendState` collapses "when did this
  happen" to one `At` field (`finished_at` once terminal, else
  `queued_at`) so the template branches on one shape regardless of which
  produced it, mirroring `BookDetail.FileSize`'s single-source-of-truth
  approach; `SendState`, `LatestSend` and `QueueSend`'s return value all
  produce it through the same unexported `sendStateFrom` shaping.
- `internal/web` — the browser UI's HTTP transport: thin handlers over
  `internal/service`, `html/template` templates and CSS/JS embedded via
  `go:embed` (`internal/web/templates/`, `internal/web/static/`), no build
  step. `GET /{$}` renders the library grid — the first real page,
  translated from Claude Design's mockups (see `UI.md`, kept on the `init`
  branch/worktree, not on `master`) — `{$}` matches only the exact path, so
  the mux's own 404 handles every other unmatched path (`GET /books/{id}`
  is now a real route below, but the mux still 404s a non-numeric or
  unknown id under it); `GET /static/` serves the embedded stylesheet and
  scripts; `GET /covers/` serves the scanner's stored cover thumbnails out of
  `COVERS_DIR` (runtime data, so it's passed into `Routes` rather than
  embedded). Both mounts wrap their filesystem in `noDirFS` so a directory
  with no `index.html` 404s instead of `http.FileServer` generating a
  browsable listing. Neither gets `Cache-Control: immutable`: `cover.Store`
  names each file by the *book's* content hash, not a hash of the
  resized/JPEG-encoded thumbnail bytes actually served at that path, so a
  future change to the resize/encode pipeline (or a regeneration under a
  changed pipeline) can overwrite different bytes at an unchanged URL —
  `immutable` would promise the opposite. Covers instead get a day-long
  `max-age`, bounding how stale a legitimately changed thumbnail can get
  while still eliminating the redundant per-visit revalidation round trip
  the header exists to avoid; `http.FileServer`'s own `Last-Modified`
  (from the file's mtime) is what lets a client revalidate once that
  window elapses. The embedded static assets get a content-derived `ETag`
  (computed once at startup, since `embed.FS` reports a zero `ModTime` and
  `http.FileServer` would otherwise emit no validator at all) paired with
  a short, five-minute `max-age` — that bounds how long a client can serve
  a stale file after a deploy without consulting the `ETag`, rather than
  eliminating the risk outright. Handlers map `service.BookSummary` onto a
  small per-page view model so templates stay logic-free. `render` executes
  into a buffer before writing anything to the response, so a template
  error is a clean 500 rather than a truncated page, and sets `Content-Type`
  explicitly rather than relying on sniffing. Only the pre-write `ExecuteTemplate`
  error is ever returned to the handler: once `Content-Type` is set and the
  buffer starts writing to the response, the response is committed, so a
  write failure past that point (almost always the client disconnecting)
  is logged inside `render` rather than returned — a handler reacting to
  it with `http.Error` would double-write onto an already-committed
  response. `renderStatus` is the same thing with an explicit status code,
  for the one case that needs a rendered body and a non-200 together: a
  rejected edit answering 422 with the editor and its message, where an
  `http.Error` would swap the editor away and lose what was typed.
- Search-as-you-type is `GET /{$}` extended, not a separate route: a `q`
  parameter narrows the grid, and htmx (vendored at
  `internal/web/static/js/htmx.min.js`, version pinned in a comment at the
  top of the file) turns each keystroke into a debounced (`delay:300ms`)
  partial request. Whether a request gets the full page or just the
  `book-grid` fragment (a named template in `templates/partials.html`,
  alongside the new `search-bar`) depends on the `HX-Request` header htmx
  sets — but on that header *without* `HX-History-Restore-Request`, not on
  `HX-Request` alone. htmx sets both on the request it issues when the user
  goes Back to a URL that has dropped out of its history cache (ten
  entries, and `hx-push-url` pushes one per keystroke, so this is ordinary
  Back-button use), and it swaps that response into the whole document
  body — so answering it with the fragment replaces the masthead, search
  bar and scripts with a bare grid that can no longer search. Both headers
  are therefore named in `Vary: HX-Request, HX-History-Restore-Request`:
  this one URL serves different bodies depending on both, and without the
  header a cache or the browser's back-forward cache could serve one where
  another was wanted.
  The search box itself lives only in the full-page render, never in the
  fragment: `hx-target="#book-grid"` with `hx-swap="outerHTML"` means only
  the grid is ever replaced, so a keystroke mid-request is never lost to a
  render. `hx-push-url="true"` keeps the URL shareable.
  The row is translated from plate 01 of the handoff (`ui-handoff/` on the
  `init` branch), which draws it as part of the top chrome — the masthead's
  ground and edges, closed by a `--rule-faint` hairline — rather than as a
  block inside the page body, so `library.html` renders `search-bar`
  between the masthead and `<main>`. Three affordances there resolve in the
  browser rather than per keystroke, since the input is never re-rendered:
  a `clear ×` link (plain `href="/"`, hidden by CSS while the box shows its
  placeholder), the `/` shortcut hint (`search.js` binds the key and only
  then unhides the hint, so it never advertises a shortcut that isn't
  bound — with JS off, or with the control dimmed, it stays hidden), and
  the `filtering …` status line. That status line is rendered *inside*
  `<main>` above the grid, not in the form: it swaps places with the
  results count, so it has to share the count's container and margins or
  the grid jumps on every keystroke. Plate 02e's empty-library state dims
  and disables the whole control — with nothing indexed there is nothing to
  search. Its "Scan library" button and library path are the one part of
  that plate not built, tracked in
  `docs/backlog/2026090203-empty-library-scan-action.md`. With JavaScript
  off, the same `<form method="get">` degrades to a normal navigation
  hitting the identical handler, so there is no separate no-JS path to
  drift out of sync. A `q` that sanitizes to nothing is "not
  searching" — the plain grid, no result count, same as before this query
  parameter existed; that covers blank and whitespace-only input and also
  input stripped to nothing, such as a lone control character. A `q` that
  does search renders either the results state — a mono line reading
  `4 of 1,284 · matched title, author`, the match count against the
  library total (grouped by thousands, as the masthead's count is, since
  one screen must not show the same number two ways) followed by the
  fields that actually matched, composed in the handler so the template
  holds no formatting logic — and the filtered grid,
  or a distinct `search__empty` block (`Nothing matches "<query>"`, the
  query HTML-escaped by `html/template`) if nothing matched — kept visually
  and structurally separate from the "No books yet" empty-library block,
  since the two call for different next actions.
- `GET /books/{id}` is the detail page (`book.html`, alongside
  `library.html`). `r.PathValue("id")` is parsed with `strconv.ParseInt`;
  a non-numeric id and an unknown one both plain 404 (`http.NotFound`),
  indistinguishable on purpose — neither is a client error worth its own
  page. Every grid card in `book-grid` is wrapped in `<a
  href="/books/{{.ID}}">`. Its masthead is the shared `site-header`
  partial, so `bookDetailPage` carries the same `Count`/`CountText` pair
  `libraryPage` does — the partial renders one and pluralizes on the
  other. Its `Locations` is `service.FileLocation` as it comes: the
  template reads `Path` and `Missing` under those names, so a per-page
  copy of the same two fields would be a rename of nothing. Metadata renders
  field-granular — one element per field, not one blob — which is what let
  the inline editors connect to existing markup rather than redesign it:
  empty optional fields (publisher, published date, language, ISBN — and
  file size, when the book has no location to take one from) render as
  visible em-dash rows rather than being dropped — a hidden field can't be
  filled in, and sparse metadata is the common FB2 case — while an empty
  author or description gets its own italic `--fg-faint` line, now phrased
  as the invitation it became ("Author unknown — add one") instead of an
  em dash, since those aren't table rows. `PublishedDate` renders
  exactly as stored, never parsed — it's free-text from embedded metadata
  (sometimes a year, sometimes a full date) and parsing it would lie
  confidently. A book's locations show as a count with a dotted-underline
  accent affordance; since no JS is guaranteed to have loaded yet, the
  reveal is a native `<details>`/`<summary>` rather than anything
  htmx-driven, and a location still within its missing-file grace period
  carries a subdued annotation.
- The send-to-Kindle control mounts at `book.html`'s designed position
  (above the description, "the reason the page gets opened") via
  `{{template "send-control" .}}`, reusing `bookDetailPage` itself as the
  fragment's data — the same struct-reuse `book-grid`'s fragment already
  makes of `libraryPage` — so `POST /books/{id}/send` and
  `GET /books/{id}/sends/{sendID}` build one mostly-zero-valued
  `bookDetailPage` rather than a parallel type. Plate 06's four states
  (idle, sending, delivered, failed) plus a fifth for
  `RESEND_API_KEY`/`RESEND_FROM` being unset are driven by fields
  `book.go`'s `applySendState` computes once — `SendPending`,
  `SendButtonLabel`, `SendButtonPrimary`, `SendAt`, `SendPollURL` — the
  same discipline `searchSummary` applies to the results line, so the
  template only branches on *which* block to show, never how to phrase
  it. `send.Status` of `queued` or `sending` are one visual state
  ("Sending"): the UI has no separate treatment for the gap between
  enqueue and claim, which the worker's `Notify` poke keeps short anyway.
  The whole control is one swap target (`id="send"`, the class
  `detail__send` kept alongside it for positioning) — form and status
  share a region because the states replace each other rather than
  coexisting; the status box's own `hx-get`/`hx-trigger="load delay:2s"`
  targets `#send` (not itself) so the outer swap replaces the whole
  control, and a terminal state's status block carries no such attributes
  at all, so polling stops by construction — the `<form>`'s own
  `hx-post` is unrelated and survives every state, keeping "Send
  again"/"Retry" htmx-enhanced. With zero saved recipients the "+ add
  address" `<details>` renders open and the `<select>` is omitted — the
  first-run state; it also renders open after a rejected address, with
  `SendNewAddress`/`SendNewLabel` carrying the typed values back so the
  fix is an edit rather than a retype. That rejection path re-reads
  `LatestSend` rather than rendering a nil state: nothing was queued, so
  retracting a Delivered or Failed result the user is looking at would
  make the page contradict itself over a typo that changed nothing.
  `POST /books/{id}/send` accepts `recipient` (an
  address from the picker) or `new_address`/`new_label`
  (whichever's non-blank wins), answers an `HX-Request` (without
  `HX-History-Restore-Request`, the same rule search's fragment split
  uses) with the fragment and everyone else with a `303` back to the book
  page — so a plain form POST with JS disabled still works, landing on a
  page whose initial render (`LatestSend`) picks the job up — and 503s
  with the disabled fragment when sending isn't configured, rather than
  404ing, so a stale open tab gets an explanation. `GET
  /books/{id}/sends/{sendID}` is scoped under the book id specifically so
  a mismatched pairing 404s instead of leaking one book's send status
  under another's page; it needs no `Vary` at all, since it serves one
  body to every caller.
- Inline metadata editing is `GET`/`POST /books/{id}/metadata/{field}`
  (`internal/web/metadata.go`), one route per field rather than one form
  per page: each field is its own swap target, so a keystroke in one never
  re-renders another. `makeFieldViews` builds all seven from one place, so
  a whole-page render and a single-field fragment cannot drift; each view
  carries `Value` (what the control edits — authors newline-separated) and
  `Display` (what the read view shows — "A, B & C") separately, because
  the stored form and the readable one differ. Every read affordance
  carries an `aria-label` naming its field: with an optional value empty
  its visible text is only an em dash, so the accessible name is the sole
  thing distinguishing seven otherwise identical "edit" links. The read
  view is an `<a>`
  with both an `href` and an `hx-get`, and the editor a `<form>` with both
  an `action` and an `hx-post`, so there is one markup path rather than a
  no-JS path that drifts: without htmx the `GET` redirects to
  `/books/{id}?edit={field}`, which renders the whole page with that
  editor open, and the `POST` 303s back to the book. An unrecognised
  `?edit=` value opens nothing rather than 400ing — it names no resource.
  Two details are load-bearing and easy to get backwards. **A rejected
  fragment answers 200, the rejected full page answers 422.** The vendored
  htmx (2.0.10) does not swap a 4xx, so an honest status on the fragment
  would leave the editor untouched and make Save look like it did nothing.
  Opting 422 in from the client (`htmx:beforeSwap`) was tried and removed:
  it buys the status code at the price of the whole interaction depending
  on one listener still being loaded and still matching, and a silent
  no-op Save is the worst failure this page has. The navigation path keeps
  the 422, where nothing swallows it. **The body cap is derived, not
  chosen**: `maxMetadataFormBody` is `3 × service.MaxMetadataValueBytes +
  1024`, because the service limits *decoded* bytes while `MaxBytesReader`
  bounds the *encoded* body and form-urlencoding triples non-ASCII text.
  It is sized off the author list rather than the description, since 100
  names of 1 KiB outweighs 64 KiB of prose — sizing off the description
  rejects a valid author list before `normalizeAuthors` can apply its own
  limits. Over the cap is still a field error, not a bare 413. Both the
  fragment and the navigation path load the book before choosing which
  shape to answer with, so an unknown book is the same plain 404 on
  each — redirecting first would answer 303 for a book that does not
  exist.
  The send POST and every metadata POST — the app's only state-changing
  routes — are wrapped in `sameSiteOnly`, which rejects a request whose
  `Sec-Fetch-Site` the browser reports as anything but `same-origin` or
  `none`. There is no
  login here, so a request's network position is the only thing between
  the collection and everyone else: any page in the user's browser can
  reach a LAN or localhost server its author cannot, and a form-encoded
  POST needs no CORS preflight to do it — with the attachment's
  destination address sitting in the request body. A request carrying no
  fetch metadata at all is allowed through, since a client that sends
  none (curl, a script, a browser predating the header) isn't the
  ambient-authority vector this guards, and failing closed there would
  cost the UI for no security gain.
- `cmd/server` — entrypoint. `main` sets up logging and a
  `signal.NotifyContext` (SIGINT/SIGTERM) and calls `run(ctx) error`, so
  every failure path has one exit point (`slog.Error` + `os.Exit(1)`).
  `run` opens the database (`DB_PATH` env var, default `./data/library.db`)
  and starts serving immediately — `/healthz` and `internal/web`'s routes
  at `/`, on `ADDR` (default `:8080`), with `ReadHeaderTimeout`,
  `ReadTimeout`, `WriteTimeout` and `IdleTimeout` all set — rather than
  blocking startup on a scan; the initial full sweep of `LIBRARY_DIR`
  (default `./library`) against `COVERS_DIR` (default `./data/covers`)
  runs in the background alongside the `SCAN_INTERVAL`-timed (default
  `15m`) periodic rescan, with missing-file grace period `MISSING_GRACE`
  (default `24h`). `WATCH_ENABLED` (default `true`) and `WATCH_SETTLE`
  (default `5s`, rejected as negative like `MISSING_GRACE`) configure the
  watcher; disabling it leaves exactly the pre-watcher behaviour, which is
  what a mount whose delivery probe reports silence wants. `periodicScan`
  sweeps on its ticker *or* a watcher poke and calls `Refresh` after each
  sweep, so a directory the sweep just discovered is watched before the
  next change lands in it. A watcher that fails to start is a Warn, not a
  failed startup — the rescan still runs. Sending is configured by `RESEND_API_KEY` and
  `RESEND_FROM`: both set builds an `internal/resend.Client` and an
  `internal/sender.Worker`, wires `Service.Notify` to the worker's
  `Notify`, and runs `storage.FailInterruptedSends` once before the
  worker starts claiming jobs; either missing only logs a `Warn` naming
  which one and passes `sendEnabled: false` into `web.Routes` — browsing
  must work on a dev machine with neither set, so a missing key never
  fails startup. The scan loop and the (if enabled) worker both run on
  the same `scanCtx`, a child of the signal-aware context but
  independently cancellable — a shared `waitForBackground` helper
  (generalised from the scan-only `waitForScan` once the worker needed
  the identical treatment) cancels it and waits out a bounded 10s window
  before the database closes, on *both* the SIGINT/SIGTERM path (`run`
  shuts the HTTP server down first, then waits, then closes the database
  — order matters, so no request or in-flight scan/send write is torn
  down by the database closing under it) and a serving failure (e.g.
  `ADDR` already in use), which used to close the database immediately
  and race the still-running scan. `internal/scanner.Scan` itself checks
  `ctx.Err()` before the walk starts and on every entry it visits, so
  cancellation stops the walk outright (and skips reconciliation) rather
  than just failing each remaining file's own DB calls one at a time. The
  image is built on `distroless/static-debian12:nonroot` (CA
  certificates, tzdata, `/tmp`, and a non-root uid, none of which
  `scratch` provides) with an explicit `WORKDIR /` — the base image's own
  default, `/home/nonroot`, would otherwise silently resolve `LIBRARY_DIR`
  et al.'s relative defaults to the wrong place — so mounted volumes must
  be writable by that uid.
- Logging goes through `log/slog` (a text handler on stderr), leveled via
  the `LOG_LEVEL` env var (default `INFO`) set once in `cmd/server` and
  used everywhere else via `slog`'s package-level functions against that
  default logger.

Still missing from DESIGN.md: metadata provider enrichment (Open Library
/ Google Books) — the consumer `field_sources` is waiting for — the send
history view, recipient management beyond the inline add-address control,
near-duplicate detection, and format conversion.

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
