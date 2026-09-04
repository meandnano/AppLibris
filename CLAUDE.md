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
- `field_sources` (`internal/storage/metadata.go`) records where a field's
  current value came from: `embedded` when the scanner read it out of the
  file, `manual` once a person has edited it, or (since `internal/enrich`)
  a provider's name once it has answered one. That full range applies to
  the seven fields a person may edit. The eighth, `cover`, is narrower and
  the difference is easy to misread: a `cover` row exists **only** when a
  provider supplied the image. `setEmbeddedFieldSourcesTx` doesn't list
  `FieldCover`, so a cover the scanner extracted records nothing at all,
  and `UpdateBookField` refuses `FieldCover`, so `manual` is unreachable
  by construction. The discriminator between a provider's cover and the
  scanner's is therefore "a row naming a provider" versus "no row" — not
  "a provider's name versus `embedded`", which never matches.
  It was deliberately write-only until now: DESIGN.md forbade shipping
  manual editing before provenance, because the provider-enrichment step
  that came later would otherwise overwrite hand-fixed values with no way
  to tell they were hand-fixed. Writing it from the start is what made
  every book edited in the meantime already carry its marker by the time
  that consumer — `FieldSourcesForBook`, below — arrived.
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
  `field_sources.field`'s CHECK constraint was rebuilt to accept an
  eighth value, `cover` (migrations `2026090304`–`2026090307`, the same
  create-copy-drop-rename shape `2026083010`–`2026083013` used for
  `book_files`, since SQLite cannot alter a CHECK constraint in place).
  `MetadataField` gained `FieldCover` (`"cover"`), but — unlike every
  other member — it is deliberately **absent from `metadataFields`**, so
  `ParseMetadataField("cover")` returns false. That map is the gate on
  `internal/web`'s per-field edit routes, on the detail page's `?edit=`
  parameter and on `service.UpdateBookMetadata`, and `cover_path` holds a
  path `internal/cover.Store` produced rather than text a person types.
  Accepting the name there is the mistake to avoid rather than an
  oversight to fix: it makes `POST /books/{id}/metadata/cover` a route
  that parses, reaches `UpdateBookField`, comes back with
  `ErrInvalidMetadataField` — which is *not* `service.ErrInvalidMetadata`,
  so the web layer logs and answers **500** — and makes the matching `GET`
  render an empty field fragment pointing at `/books/0/metadata/`, where
  a name nobody may edit should simply 404. `UpdateBookField` still
  refuses `FieldCover` outright alongside `authors` as a second guard, and
  `ApplyEnrichedFields` is the only writer, storing a fetched cover's
  on-disk path (never a remote URL) under the answering provider's name
  through the same `updateBookColumnTx`/`fieldIsStillMissingTx` path every
  other field uses — so a cover the scanner already found is never
  replaced by a provider's guess, the same protection `isMissing` gives
  every text field. Writing `cover_path` also clears `cover_retry`, the
  same pairing `UpdateBookCoverPath` makes: the marker means "a cover
  store failed, try again next sweep", and the scanner skips its stat
  check entirely while it is set, so leaving it would have the next sweep
  re-extract the embedded cover over the provider's one while
  `field_sources` went on naming the provider. See `internal/enrich` below
  for the fetch and storage side of this.
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
  never passes through `syncBookFTSTx` on its own.
  `SearchBooks(ctx, query, page)` joins against it and orders by
  `(sort_title, id)`, not relevance — see `internal/web` below for why the
  ordering ignores relevance, and `BookPage` above for why the `id`
  tie-break is load-bearing rather than cosmetic: the paging cursor
  depends on the ordering being total. `MatchedSearchFields(ctx, query)` reports
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
  any search filter, and `CountSearchBooks` is its filtered sibling —
  how many books a query matches, which the transport can no longer count
  in hand now that it holds one bounded page, and without which the
  results line would read "48 of 1,284" for a search that matched nine
  hundred.
  `BookPage` is the keyset cursor `ListBooks` and `SearchBooks` both page
  by — `AfterTitle`, `AfterID`, `Limit`, zero value meaning the first
  page. **Keyset, not `LIMIT`/`OFFSET`**, for a reason specific to this
  application: the library changes underneath the reader. The scanner
  sweeps every fifteen minutes and on every filesystem event, inserting
  wherever a book's `sort_title` falls, and under `OFFSET` a book inserted
  above the reader's position shifts every later row down by one — so the
  next page repeats a card, or on a delete silently skips one. A cursor
  naming the last row seen has no such window. **`AfterID` is part of the
  cursor** because `sort_title` is emphatically not unique: it is a
  normalised title with the article stripped and the case folded, so two
  editions of one book collide by construction, and a cursor on a
  non-unique column either loops on the collision or skips past it. The
  comparison carries an explicit `COLLATE NOCASE` — it would be inherited
  from the column's own declaration anyway, but the whole correctness of
  paging rests on it matching the `ORDER BY`, and that is worth reading
  off the query rather than off the schema. `Limit: 0` is unbounded,
  which is what keeps the scanner's and the tests' whole-library calls
  working untouched; the web transport never passes zero. Index
  `books_sort_title_id` makes the range scan an index scan. `CountFilesByBook` mirrors `ListBookAuthors`: one
  `GROUP BY book_id` over `book_files`, keyed by book id, for the grid's
  multi-location badge. It counts every location row, missing ones
  included — a row stays in `book_files` until it has been missing past
  `MISSING_GRACE`, and the detail page lists it (annotated) for that whole
  window, so filtering them out here would make the grid and the detail
  page disagree about the same book's location count while linking
  directly to each other. A book id absent from the map counted zero rows —
  the plain `GROUP BY` never emits one for a book_id with none — which a Go
  map read returns the same as an explicit zero; zero rows should be
  exceptional in practice (the last location's deletion prunes the book in
  the same transaction) but that invariant is enforced by the scanner's
  orphan-pruning, not by this query, which only reports `book_files` as it
  stands.
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
  LIMIT 1`). `ListSendsSince` is the send history view's data: sends queued
  at or after a given time, newest first (`queued_at DESC, id DESC`, the
  same tie-break `LatestSendForBook` uses), capped at a row limit — indexed
  by `send_log_queued_at`, since neither existing `send_log` index serves
  an unfiltered `ORDER BY queued_at DESC`. It reads `book_title` and
  `recipient_address` straight off `send_log` rather than joining `books`
  or `recipients`: that denormalisation exists precisely so a pruned
  book's or a removed recipient's send still appears in its own history,
  and a join would silently drop exactly those rows — read the columns,
  do not join. `DeleteRecipient` removes a saved address, matching
  `COLLATE NOCASE` the same way `CreateRecipient` is idempotent across
  case, and returns `false` for an unknown address rather than an error —
  a double-submitted remove is a slip, not a failure. It never touches
  `send_log`: `recipient_address` being a plain string rather than a
  foreign key is what makes that a schema guarantee rather than a rule
  this method has to remember to keep.
- `enrichment_jobs` (`internal/storage/enrichment.go`) backs the provider-
  enrichment queue, one row per job (a book can accumulate several over
  time — terminal history isn't pruned, and a `running` job can coexist
  with a fresh `queued` one; only `EnqueueEnrichment`'s own guard, below,
  keeps two `queued` rows from coexisting): `status` is CHECK-constrained
  to `queued`/`running`/`done`/`failed`, the same shape `send_log` uses.
  `book_id` **cascades** on delete, unlike `send_log.book_id` — the
  contrast is deliberate: a send log entry is the record that a thing
  happened and must outlive its book, while an enrichment job is a
  *pending intention* about a book, meaningless once the book is gone.
  `EnqueueEnrichment` is idempotent while a job is `queued`
  (`INSERT … WHERE NOT EXISTS`, one statement) but not against a `running`
  one — a book already being processed doesn't block a fresh promise once
  that job goes terminal. `ClaimNextEnrichment` mirrors `ClaimNextSend`
  (oldest `queued` row, flip to `running` with `started_at` set, one
  transaction), and `MarkEnrichmentDone`/`MarkEnrichmentFailed` mirror
  `MarkSend*`'s `WHERE status = 'running'` terminal-state guard.
  `RequeueInterruptedEnrichment` is startup recovery, but the *opposite*
  of `FailInterruptedSends`: a send's side effect (a message leaving the
  process) isn't repeatable, so an interrupted one is failed rather than
  guessed at, but an enrichment job's only effect is writing fields a pure
  function (`internal/enrich.Resolve`) computed from data already in the
  database — running it again on the same inputs lands the same values,
  so a `running` row is put back to `queued` (with `queued_at` reset to
  the recovery time) rather than failed — unless that book already has a
  fresh `queued` sibling (`EnqueueEnrichment`'s guard only blocks a second
  *queued* row, so one can coexist with a `running` one, per Decision 3),
  in which case the interrupted row is deleted outright instead of
  requeued: requeuing both would leave the book with two queued promises,
  breaking that dedup invariant and doubling the provider calls the next
  drain makes for it, when the surviving sibling's own run already
  recomputes the missing set from scratch and covers whatever the
  interrupted one would have — and marking it `done` would misreport a
  crash as a successful run, `MarkEnrichmentDone`'s actual contract. Two
  details are easy to get backwards for the same reason: `internal/enrich`'s worker checks
  `ctx.Err()` after any failed step and, if the ambient context is
  already cancelled, leaves the row `running` rather than calling
  `MarkEnrichmentFailed` — a definite failure record would deny the row
  the automatic retry `RequeueInterruptedEnrichment` exists to give it,
  the opposite of `internal/sender`'s "retry is a new row" rule. And a
  vanished book *is* a `failed` job, not a `done` one with nothing to
  enrich — `failed` is reserved for the job itself going wrong (the book
  gone, a write failed), the same way it would be a bug for `internal/sender`
  to call a send "delivered" because there was nothing left to send. The
  cascade normally removes a claimed job's row along with its book before
  this can be observed; it exists for the narrow claim-then-delete race.
  `updated_fields` (migration `2026090308`) is a comma-separated list of
  the fields a run actually wrote, so the UI can name them instead of
  saying "done" — comma-separated rather than a join table because it is
  display text for one fragment, never queried, and a table would imply
  it is data. `MarkEnrichmentDone` takes that list and stores it in the
  same statement as the terminal state; empty is the ordinary
  "nothing to add" success. It is what `ApplyEnrichedFields` *wrote*, not
  what `Resolve` proposed — the two differ whenever its own re-check
  skips a field a concurrent edit has since filled, and the result line
  exists to say what this run did. So `ApplyEnrichedFields` returns
  `(written []MetadataField, exists bool, err error)`, iterating
  `metadataFieldOrder` rather than ranging over the caller's map so the
  same input reads the same way twice; it still validates every key up
  front and fails on an unrecognised field, since iterating the known
  fields would otherwise pass silently over a typo'd constant, and a
  silent no-op is how that bug survives to production.
  `GetEnrichmentJob` and `LatestEnrichmentForBook` are the poll route's
  and the detail page's lookups, mirroring `GetSend`/`LatestSendForBook`
  including the `queued_at DESC, id DESC` tie-break.
  `FieldSourcesForBook` is `field_sources`' first reader since it was
  introduced with inline editing — a field absent from the returned map
  (never embedded, never edited) reads back as an empty source, which the
  resolver's missing-field rule already treats as not-`manual`.
  `ApplyEnrichedFields` is `UpdateBookField`/`UpdateBookAuthors`
  generalised rather than a parallel path: both now call shared
  unexported helpers (`updateBookColumnTx`, `updateBookAuthorsTx`) that
  take a `source` parameter, with the public methods passing `"manual"`
  and `ApplyEnrichedFields` taking a `sourceName map[MetadataField]string`
  instead of one shared `source` — `Resolve`'s own return value, passed
  straight through, so a job that pulled fields from more than one
  provider still records each field under whichever one actually answered
  it, all in the one transaction `ApplyEnrichedFields` runs as. Before
  writing each field, `fieldIsStillMissingTx` re-reads its current value
  and provenance fresh, inside that same transaction, and skips it if it's
  no longer missing: `Resolve`'s snapshot can be stale by the time a
  provider has answered minutes later, and `DB.Write`'s single write
  connection means a concurrent manual edit — filling the field, or
  deliberately clearing it — has already committed by the time this
  transaction's read runs, so a provider's now-stale answer can never
  clobber it. That recheck is why provenance stays right across every
  caller instead of drifting in one. Authors move through the same
  join-table path `UpdateBookAuthors` uses, their value newline-joined in
  the `map[MetadataField]string` both `Resolve` and `ApplyEnrichedFields`
  share — the same convention the web layer's author textarea already
  uses — so an author list looks the same shape whether a provider or a
  person supplied it.
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
  rename, so readers never observe a partial canonical cover. It reads the
  image header on its own first (`image.DecodeConfig`) and refuses anything
  over `maxPixels` (50 MP) before `image.Decode` allocates a pixel buffer:
  a byte cap on the input is not the same guarantee, since a small, highly
  compressible file decodes to width × height × 4 bytes of RGBA. That
  matters most for a cover a metadata provider supplied (`internal/enrich`),
  where the bytes come from a third party, but an embedded cover is no more
  trustworthy.
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
- `internal/enrich` is the metadata provider-enrichment queue: a single
  `Worker` over `enrichment_jobs`, the `Provider` interface
  `internal/openlibrary` and `internal/googlebooks` implement, and the `Resolve`
  function that decides which fields a book still needs and merges what
  providers answer. Modeled directly on `internal/sender` — same
  claim/process/terminal-write shape, `Notify` poke plus `pollInterval`
  ticker — with two deliberate divergences, both because an enrichment
  job's only effect (writing fields a pure function computed from data
  already in the database) is repeatable where a send's is not: startup
  recovery **requeues** a `running` job instead of failing it
  (`storage.RequeueInterruptedEnrichment`, the inverse of
  `FailInterruptedSends`), and the worker treats a step failing while
  `ctx` is already cancelled as an abandoned attempt — leaving the row
  `running` for that recovery to retry — rather than a verdict worth
  writing, which is what `internal/sender` does with the same situation
  (there, `"failed"` is a definite, useful answer; here, a job has no
  "retry is a new row" affordance a permanent `failed` could rely on, so
  losing the row to an honest retry costs nothing and a wrong `failed`
  costs a book silently never being reconsidered).
  `Resolve(ctx, book, authors, sources, providers)` takes no database and
  no clock — everything about the book's current state is passed in —
  which is the whole point: DESIGN.md requires the merge logic testable
  without a real provider, and every resolver test in
  `resolver_test.go` runs against fakes. Its rule, in one function
  (`isMissing`) with both halves in one place: a field is worth asking a
  provider for only when it is **both** empty **and** not `manual` —
  dropping the emptiness half means re-enrichment overwrites good
  embedded metadata with a guess, dropping the `manual` half means a
  field someone deliberately cleared gets silently refilled, exactly the
  failure `field_sources` exists to prevent. Providers are asked in
  order, each only for what's still missing at the time it's asked; once
  the missing set is empty the loop stops without calling the rest — an
  explicit test asserts the un-called provider really is never
  called, not just that its answer goes unused. A provider erroring is
  logged and skipped, indistinguishable (deliberately — see above) from
  one abandoned by a cancelled `ctx`; either way the chain continues, and
  neither is `Resolve`'s own failure to report. A book's authors are
  resolved as a field like any other — missing when the book has none and
  no source claims `manual` — with the answer carried through
  `Resolve`'s `map[storage.MetadataField]string` newline-joined, the same
  representation `ApplyEnrichedFields` (`internal/storage`) and the web
  layer's author textarea already use, so the join-table write on the way
  in is `storage.ApplyEnrichedFields`'s job, not this package's. The
  worker passes `Resolve`'s `values` and `sourceName` straight through to
  one `ApplyEnrichedFields` call — DESIGN.md's field-level merge means a
  single job can legitimately resolve fields from more than one provider,
  and `sourceName` already carries each field's own answerer, so there is
  no grouping to do here; keeping every resolved field in one call is also
  what lets `ApplyEnrichedFields` apply (or skip, per its own re-check)
  the whole set as one transaction.
  Every value a provider supplies is sanitised before it reaches that map
  (`sanitizeValue`): trimmed, capped, and — for every field but
  description — stripped of line breaks. `ApplyEnrichedFields` is a second
  writer to the same columns `internal/service`'s `normalizeField` guards
  for a person's edit and never passes through it, so this is the only
  thing bounding what a remote source can put there; the limits are
  restated here rather than imported, since `internal/service` sits above
  this package — but restated at the *same numbers* (1024 bytes for a
  title and for one author name, 4096 for the other scalars, 64 KiB for a
  description, 100 names), which is the part that is easy to get wrong: a
  value this package writes but `normalizeField`/`normalizeAuthors` would
  reject is a field the app can no longer edit, since opening the editor
  and pressing Save unchanged then fails on a value nobody typed. Author
  names are sanitised one at a time and re-joined, because `authorsJoin`
  is itself a newline and sanitising the joined string would collapse a
  list into one name; the list is cut at 100 for the same reason each name
  is capped.
  A cover is resolved the same way but kept out of that map: `Resolve`
  returns it separately, as `coverURL`/`coverSource`, since `values` only
  carries strings that go straight into a column and a cover's path does
  not exist until the image has been downloaded and passed through
  `internal/cover.Store` — I/O `Resolve` deliberately never performs
  itself, per its "no database, no clock" contract. It is otherwise
  subject to the same rules as every other field — missing-set
  membership, first-answer-wins, the early stop once nothing is left
  missing — so a book whose `cover_path` is already set is never handed a
  provider's cover answer at all, which is what makes "only fetch a cover
  the book actually needs" a property of the whole chain rather than of the
  write alone: no URL comes back, so nothing is downloaded. The worker
  fetches it through `enrich.FetchCover` on its own `*http.Client`
  (`coverFetchTimeout`) — the URL may name a host, Open Library's separate
  covers domain for one, that has nothing to do with whichever provider
  answered — capped at `MaxCoverBytes` (512 KiB) read *before* any decoding
  and refused outright if its scheme is not `http`/`https`, since the URL
  is a third party's string rather than one this process chose — a check
  the worker's client re-applies to every redirect hop
  (`CheckCoverRedirect`, which also bounds the hop count), because the URL
  a redirect names is chosen by whichever host answered rather than by the
  provider whose response named the cover, so checking only the first one
  guards nothing. The request carries the same descriptive `User-Agent`
  both provider clients set, for the same reason: the host answering it is
  most often `covers.openlibrary.org`, Open Library's own, and a throttle
  or block there would arrive as an ordinary fetch failure — every cover
  silently unstored, nothing naming the cause. It then
  converts it exactly like the scanner converts an embedded one
  (`cover.Store`, resized, JPEG, named by the book's content hash — never
  the remote URL) before folding the resulting path into `values` under
  `storage.FieldCover`; a fetch or `Store` failure only loses the cover —
  it is logged and the field is left out of
  `values`, the same tolerance the scanner gives a cover that fails to
  store, since it must not fail a job whose text fields already resolved.
  The job itself failing (the book vanished between
  enqueue and claim, a write failed) is a `failed` job; a provider having
  nothing to say — the ordinary case for most books against most
  providers — is not, and a job that reached at least one provider still
  finishes `done`. `enrichment_jobs.book_id` cascades on delete (unlike
  `send_log.book_id`, which must survive its book to keep the record a
  send happened): a queued or running enrichment job is a pending
  intention about a book, and once the book is gone the intention is
  meaningless, so the ordinary path never even reaches the `bookGoneReason`
  the worker records for it — that exists for the narrow race where a
  claim and the book's deletion interleave.
- `internal/openlibrary` and `internal/googlebooks` are the two
  `enrich.Provider` implementations DESIGN.md names, shaped identically on
  purpose so the registry below can treat them interchangeably: `New()`
  (`googlebooks.New(apiKey)` takes an optional key) builds a `Client`
  around its own `*http.Client` with an 8-second `Timeout` — the
  `internal/resend` precedent of never relying on `http.DefaultClient`,
  sized short because enrichment is a background nicety nobody is waiting
  on. `ByISBN` normalises its argument exactly as `internal/epub`
  normalises a stored ISBN (hyphens/spaces stripped, a trailing
  check-digit `X` upper-cased — duplicated rather than imported, the same
  choice `internal/storage`'s own copy makes) so a lookup key round-trips;
  `Search` is the title/first-author fallback the resolver uses when a
  book has no ISBN.
  Open Library models a **work** (the book as written) separately from an
  **edition** (one publication of it), and the two `internal/openlibrary`
  paths hit different endpoints because of it — the distinction that
  decides whether `books.language` and `books.published_date` are right.
  `ByISBN` uses the **Read API**
  (`/api/volumes/brief/isbn/{isbn}.json`): an ISBN names one edition, so
  an edition-scoped answer exists and is the only correct one. It reads
  *both* blocks of that response, neither redundant — `data` is the only
  place author **names** appear (an edition record lists author
  references, or for many editions none at all, since authorship belongs
  to the work), while `details.details` is the raw edition record and the
  only place the language and description appear. `Search` stays on
  `/search.json`, which answers about works, and therefore reports
  **neither language nor publication date**: that endpoint's `language` is
  every language any edition was ever published in (31 of them, starting
  `bul`, for an English printing of *The Hobbit*) and its date is the
  work's first publication — 1937 for a 2012 edition. Leaving both empty
  is what keeps them *missing*, so `Resolve` offers them to the next
  provider and a person can still fill them by hand; a wrong value reads
  as answered and is never reconsidered. It is the standard
  `internal/epub` already holds itself to in never substituting a
  `creation` date for a publication one, and `internal/fb2` in preferring
  `publish-info/year` over `title-info/date`.
  Two shapes on that path are easy to get wrong and both are covered by
  live-captured fixtures: an ISBN the Read API knows nothing about is
  answered with a bare **`[]`** — a JSON *array* where a match is an
  object — so the body is checked before unmarshalling, since decoding it
  into the response struct fails with a type error and reporting the
  ordinary no-match as a parse failure would make an obscure book look
  like a broken provider; and `description` is either a
  `{"type", "value"}` object *or* a bare string in the same position
  (`textValue` tries both), where a decode expecting only the object would
  silently drop every string-shaped one. `ByISBN` also follows redirects
  under `checkRedirect`, a bounded, every-hop-scheme-checked policy
  mirroring `enrich.CheckCoverRedirect` — setting `CheckRedirect` at all
  replaces net/http's own hop limit, so a policy that checked only the
  scheme would follow a chain forever.
  Both providers implement the same four-case contract: a 200
  with no results and a defensive 404 are both a zero `Metadata` with a
  nil error — the ordinary case for an obscure or mistitled book, not an
  error — while a 429, any 5xx, or a transport/timeout failure are errors
  for the retry decorator and the resolver's skip-and-continue to see as
  such. A matched result's cover — Open Library's separate
  `covers.openlibrary.org` host by numeric `cover_i` id, Google's
  `imageLinks` already in the same response, largest size first and
  upgraded to `https` — is **named, not downloaded**: it comes back as
  `Metadata.CoverURL`, and the fetch is `internal/enrich`'s Worker's. That
  split is the point. Fetching inside the provider spends a round trip and
  up to `MaxCoverBytes` on every lookup, including the common case of a
  book that already has an embedded cover and whose answer `Resolve` then
  discards — and it puts image bytes into `WithCache`'s bounded map, where
  512 entries times two providers is hundreds of megabytes held for the
  process's lifetime. A `Metadata` of nothing but strings is what keeps
  that cache kilobytes. Both clients classify their failures for the retry
  decorator: a 429, a 5xx and a transport failure wrap
  `enrich.ErrRetryable`, while a 400, a 403 (Google's over-quota and
  rejected-key answer) and a malformed body do not, since another attempt
  answers those identically. Both set a descriptive `User-Agent` — Open
  Library's terms ask for one and throttle the generic Go default, and a
  block there would be indistinguishable from any other transient failure,
  so the resolver would silently skip the provider for every book. Open
  Library's MARC three-letter language codes are mapped to the ISO 639-1
  form `internal/epub`, `internal/fb2` and Google Books all produce, so the
  column doesn't hold `eng` for one book and `en` for the next. Google's
  `intitle:`/`inauthor:` values are quoted, which is load-bearing: the
  Volumes API binds the qualifier to the single token after it, so an
  unquoted multi-word title constrains only its first word. Google's
  `description` is documented as *HTML-formatted* and is rendered to plain
  text (`plainText`) before it leaves the package — block tags become line
  breaks, inline ones are dropped, and entities are unescaped only
  afterwards, so text that was itself escaped markup (`&lt;b&gt;`)
  survives as the characters an author wrote rather than being stripped as
  a tag. Nothing downstream treats a description as markup: `html/template`
  escapes the detail page's, so a tag left in shows a reader a literal
  `<p>` and then offers them the same markup to hand-fix in the edit
  textarea. It lives here rather than in `internal/enrich`'s
  `sanitizeValue` because Open Library's description — the edition
  record's, which is what `ByISBN` reads — is plain to begin
  with, and stripping tags from every provider's answer would mangle one
  that legitimately contains a `<`. Google Books'
  optional `apiKey` is scrubbed from every returned error's text
  (`redactKey`/`redactKeyBytes`) since a transport error embeds the full
  request URL and the key must never reach a log line through one; the
  redacting error keeps an `Unwrap`, so whether `errors.Is(err,
  context.Canceled)` works doesn't depend on whether a key happens to be
  configured. Both packages are tested against
  `httptest.Server` with fixtures under `testdata`, and those fixtures
  have two provenances worth telling apart when reading a failure.
  `internal/openlibrary`'s two `edition_*.json` are **live captures** of
  the Read API — they are what turned up the work-versus-edition defects
  `ByISBN` moved endpoint to fix, and the bare-`[]` no-match, neither of
  which a hand-written fixture would have shown. The rest are shaped after
  each API's stable, publicly documented response format instead, from
  when those packages were written with no outbound network access
  available. Each `_test.go` names which of its own fixtures is which at
  the top. Nothing here is hand-edited to fit a change: a fixture adjusted
  until the code passes tests the parser against its author's
  expectations rather than against the API — which is exactly how an ISBN
  lookup went seventy-five years and one language wrong with every test
  green.
- `internal/enrich`'s three decorators (`decorator.go`) wrap a `Provider`
  and satisfy `Provider` themselves, so they compose in any order and the
  resolver cannot tell they are there: `WithRateLimit` gates `ByISBN`/
  `Search` on a shared ticker-fed token, honouring `ctx` cancellation
  while waiting rather than blocking a shutdown until the next slot opens
  (`DefaultRateLimitInterval`, one call a second, a conservative default
  since Open Library's own limit is a courtesy ask, not an enforced one).
  `WithCache` serves a repeat lookup — same method, same arguments — out
  of a bounded LRU (`DefaultCacheSize`) instead of calling the wrapped
  provider again, caching a "no match" answer too, since without that a
  shelf of obscure books would re-ask the same negative answer on every
  sweep; an error is never cached, since the four-case contract treats it
  as transient. `WithRetry` retries only the 429/5xx/transport case, up to
  `DefaultRetryAttempts` total tries with a doubling backoff
  (`retryBaseDelay`/`retryMaxDelay`) and a `ctx` check between attempts —
  a "no match" is never retried, since it is an answer, not a failure.
  `internal/providers` (`registry.go`) is the compile-time name →
  constructor map, kept outside `internal/enrich` rather than inside it as
  the plan first sketched: both provider packages import `internal/enrich`
  for `Provider`/`Metadata`, so a registry living there and importing them
  back would be a cycle. `Resolve(names, googleBooksAPIKey)` turns
  `METADATA_PROVIDERS`'s parsed, ordered names (`ParseNames`) into the
  decorated chain `enrich.New` expects, composing each provider once as
  `WithCache(WithRetry(WithRateLimit(client)))`. The order is easy to get
  backwards, and it reads outermost-first because the outermost wrapper is
  what a call reaches first: **cache outermost**, so a lookup already
  answered spends neither a rate-limit token nor a retry attempt; **rate
  limit innermost**, so every attempt `WithRetry` makes — not just the
  first — takes a token of its own. Rate limiting on the outside instead
  would make a cached hit wait a full interval for an answer already in
  memory, and would leave retries paced only by `retryBaseDelay`, sending
  a provider that just answered 429 three requests inside one token. An
  unknown name fails outright, naming it and listing the valid ones
  sorted, rather than silently running with fewer providers than
  configured; a name repeated in the list is kept once at its first
  position (two chains would mean two caches and two rate-limit budgets);
  an empty name list resolves to an empty, non-nil slice —
  `METADATA_PROVIDERS=` is the documented way to disable enrichment
  outright, not an error.
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
  watch set after every sweep, which covers three measured failure modes:
  inotify is not recursive so a new subdirectory needs its own watch; a
  watch is dropped *silently* when its directory is deleted; and a
  directory that *moves out* of the library leaves the watches on its
  descendants live but filed under the names they had inside it (the moved
  directory's own watch fsnotify drops, its children's it cannot), so they
  go on reporting files created outside the library as though they had
  arrived in it. That third one is why `Refresh` compares each directory's
  `(dev, ino)` against a `registered` map and not just its name — and why
  it *also* checks `WatchList()` membership, since neither test is
  sufficient alone: a filesystem may hand a recreated directory the inode
  number the deleted one had (ext4 does, reproducibly), which reads as
  unchanged for a watch the kernel has already dropped. Together they are
  exact, because an inode number only becomes reusable once its inode is
  freed and freeing it drops the watch filed under that name. A superseded
  watch is released explicitly, before the re-add: fsnotify's `updatePath`
  drops the old descriptor from its own map — enough to stop its events
  being delivered, which is why an event-delivery test passes either way —
  but never calls `inotify_rm_watch`, so the kernel keeps the watch and no
  later `Remove` or `WatchList` can reach it, spending one watch from a
  per-user budget per replacement. Removing first is safe because inotify
  allocates descriptors cyclically rather than handing back the one just
  freed, so the `IN_IGNORED` it queues cannot land on the fresh watch. It
  cannot
  cover the fourth — losing *every* watch, when the library directory
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
  an idle one, so a delivery probe creates a file in the library root and
  waits for its own event; silence is one Warn. It is created with
  `os.CreateTemp` rather than at a name derived from the pid: `os.Create`
  would truncate whatever already sits at that path and follow a symlink
  through to its target, and in a container the process is pid 1, so the
  name is guessable — a diagnostic must not be able to destroy a book, and
  the only writes this directory ever gets are new paths. The probe's own
  name is recorded before any event is read and excluded from `qualifies`
  (it must not trigger the work it tests); the file is removed
  unconditionally including on the timeout path, and only ever the one
  this probe created. A read-only library is an Info-level skip rather
  than a failure.
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
  `BookSummary.Locations` — how many `book_files` rows a book has — comes
  from `storage.CountFilesByBook` alongside the author map, in the same
  helper, so both `ListBooks` and `SearchBooks` get it for free; a book
  absent from that map counted zero rows at the storage layer, but since
  zero and one both mean "don't show the multi-location badge," this
  normalizes it to 1 rather than carrying the raw zero forward — one
  location being the common real-world case an absent entry actually
  represents, zero rows in `book_files` being the exceptional one orphan-
  pruning is meant to prevent. `SearchBooks` sanitizes via
  `storage.SanitizeFTSQuery` first and returns
  a `SearchResult` — one page of books, whether a search actually ran,
  which indexed fields matched, how many books matched in total
  (`MatchCount`, which `Books` no longer tells you now that it holds a
  bounded page) and where the next page starts (`Next`). A query that sanitizes to nothing is treated as
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
  produce it through the same unexported `sendStateFrom` shaping, which
  itself calls the package-private `sendAt` for that collapse — `SendRecord`
  (the history view's row) shares it via `sendRecordFrom`, so the detail
  page's status box and the history table can never report a different
  instant for the same send. `SendHistory` returns recent `SendRecord`s,
  newest first, over the trailing `sendHistoryWindow` (30 days) measured
  from `Service.now` — so the window is testable without waiting — capped
  at `SendHistoryLimit` (500, exported for the same reason
  `MaxMetadataValueBytes` is: `internal/web` spells the number out in the
  scope line when truncation happens, and a copy of the literal there
  would drift). `truncated` reports whether the cap actually cut rows out
  of the window, found by asking storage for one row past the limit rather
  than a second `COUNT` query — getting `SendHistoryLimit+1` rows back
  proves at least one more exists, at which point the extra row is trimmed
  off before returning. A fixed "last 30 days" line over a silently
  truncated list would be a claim the page cannot support, and the history
  view exists to be believed. `RemoveRecipient` is a thin pass-through to
  `storage.DeleteRecipient`.
  Enrichment has the matching trio: `EnrichBook` (enqueue, poke the
  worker via `NotifyEnrichment`, return the state — `nil, nil` for an
  unknown book, `GetBook`'s contract), `EnrichmentState(ctx, jobID)` and
  `LatestEnrichment(ctx, bookID)`, shaped through `enrichmentStateFrom`
  and its own `enrichmentAt` — the same "collapse when-did-this-happen to
  one `At` field" `sendAt` makes, so a template picks one field and never
  branches on which produced it. `EnrichBook` reads the state back rather
  than synthesising it, because `EnqueueEnrichment` is idempotent while a
  job is queued: pressing the button twice makes no second promise, and
  what the caller wants back either way is the job the book actually has,
  which `LatestEnrichmentForBook` returns in both cases.
  `NotifyEnrichment` is a second function field beside `Notify` rather
  than one multiplexed hook — two queues, two workers, and poking the
  wrong one would leave a job sitting until its own poll tick.
  The symmetry with sending is now three pairs deep (state, latest,
  shaping) and stays two parallel surfaces on purpose: an abstraction over
  exactly two cases has no third instance to test its shape against, and
  the two differ in precisely the part that would have to be generic — a
  send's terminal detail is an address and a failure reason, an
  enrichment's is the list of fields it wrote. If a third queued-job type
  ever appears, that is the moment, not now.
  `BookDetail.FieldSources` maps a field name to where its current value
  came from, for the detail page's provenance markers.
  `ListBooks` and `SearchBooks` take a `storage.BookPage` and return a
  `NextPage` beside the summaries — `HasMore` plus the cursor the next
  page starts at. The cursor is returned rather than left for the caller
  to derive, because it is a `sort_title` and `BookSummary` deliberately
  has no such field: that is a storage ordering detail, not something a
  card renders. "Are there more?" is answered by asking storage for one
  row past the limit and trimming it (`pageOf`), never by a second count
  query — `SendHistory` already decides its own truncation that way, and
  reusing a technique this codebase has beats introducing a second one.
  `SearchResult.MatchCount` is the whole match total, which `Books` no
  longer tells you now that it holds one page.
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
  small per-page view model so templates stay logic-free. `bookCard.PathsLabel`
  is the multi-location badge's text ("2 paths") composed in the handler
  from `BookSummary.Locations`, the same way `searchSummary` composes the
  results line — set only above 1, so the template branches on presence
  rather than formatting a count itself and risking "1 paths". It renders
  in `.card__meta` beside the format badge, on both the full library page
  and the `book-grid` fragment a search request gets, sharing the accent
  and dotted-underline treatment the detail page's own `.locations summary`
  already uses for the same idea — the grid says "look here", the detail
  page (already listing every `book_files` row) says "here is what and
  where". `render` executes
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
- The grid is **paged**, not the whole library: `pageSize` (48, the
  handoff's own "Loading next 48 of 1,284" figure — a number in a mockup
  is a decision about how much scrolling one reveal buys) bounds every
  render, and a reveal trigger appends the next batch. One route serves
  three response shapes, which is one more than the `HX-Request` split can
  tell apart, so the third says so in the query: the full page, the whole
  `book-grid` fragment a keystroke gets, and — with `append=1` — just the
  next batch of cards. `book-grid-cards` is that batch, and is the same
  template the full grid renders inside its own `<ul>`, so a page of cards
  looks identical however it arrived.
  The trigger is a single `<li class="grid__more">` **inside** the same
  `<ul>` as the cards, because it replaces *itself*
  (`hx-target="this"`, `hx-swap="outerHTML"`) with the next batch plus a
  fresh trigger — whatever it swaps in has to be a legal child of that
  list. It carries both an `href` and an `hx-get`, the same
  single-markup-path rule the read affordances follow, and here that
  matters more than usual: before paging, an unpaged grid was the *only*
  thing that worked with JavaScript off, so a paging implementation that
  forgot the fallback would have made the no-JS case strictly worse than
  before the change. The plain href is a whole page starting at the same
  cursor. `MoreLabel` empty is how the last page renders no trigger at
  all rather than a line offering zero more books, and the count in it is
  the one the reader is looking at — the library total on an unfiltered
  grid, `SearchResult.MatchCount` during a search, since "of 1,284"
  beside a filtered grid names a number nothing on screen refers to.
  A keystroke rebuilds the whole grid including its trigger, so **a new
  search resets paging by construction** — a stale trigger left behind
  would append page two of the previous query. That is invisible until it
  breaks, so a test pins it.
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
- Enrichment's surface is three affordances and no more, because this is
  the one step in the sequence with **no mockup** — the handoff's seven
  plates never cover it and DESIGN.md designs the mechanism without a
  surface. Rather than invent one, it answers the three questions
  enrichment actually raises: where did this value come from
  (provenance), can I fetch metadata now (a trigger), and did it do
  anything (a result). A library-wide "enrich everything", an enrichment
  history page and editing provenance are all deliberately absent — the
  first has no honest progress display short of building one, the second
  would be a page nobody opens since the result is visible in the fields
  themselves, and a source is a fact rather than a setting.
  **Provenance is shown only where it isn't obvious**: `providerSourceNote`
  (`book.go`) renders a marker for a provider's name and *nothing* for
  `embedded`, `manual` or an absent source, so `editableFieldView.SourceNote`
  is empty for six fields out of seven on a typical book. Every field has
  a source, and rendering all of them would double the metadata block's
  weight to say "embedded" seven times — the default, and therefore not
  information. The marker is a **caveat**: a value the scanner read out of
  a file is a fact about the file, a value someone typed is theirs, and a
  value a third-party API guessed at is the only one whose origin changes
  how much to trust it. `manual` renders nothing deliberately even though
  it is the source the resolver cares most about — the person who typed it
  does not need telling, and a label beside a field that already carries
  an edit affordance is noise. It is derived in `makeFieldViews`, the one
  place every field view is built, which is also what makes **editing
  clear the marker for free**: saving sets the source to `manual`, and the
  metadata POST handler reloads the book rather than echoing the submitted
  value, so the rebuilt fragment reads the new provenance. A test pins
  that, because it is exactly what a later "optimisation" removes.
- `POST /books/{id}/enrich` and `GET /books/{id}/enrichment/{jobID}`
  (`internal/web/enrich.go`) are the trigger and its poll target, reusing
  the send control's state machine rather than inventing a second one —
  not a coincidence to be tidied away but the same underlying thing, a
  queued background job against one book. Same `sameSiteOnly` wrapper on
  the POST, same book-id scoping on the poll route so a mismatched pairing
  404s instead of leaking one book's job state under another's page, same
  one-swap-region-with-a-stable-id (`#enrich`), and the same
  progressive-enhancement split: an `HX-Request` gets the fragment,
  everyone else a `303` back to the book page, whose initial render picks
  the job up through `LatestEnrichment`. Polling stops **by
  construction**: only the pending block carries `hx-get`/`hx-trigger`, so
  a terminal fragment has nothing left to re-arm — no counter, no limit.
  The form's own `hx-post` survives every state, keeping "Fetch
  again"/"Retry" htmx-enhanced, and a re-run is a new row the same way a
  retried send is. Where it differs from sending, and why: there is no
  recipient picker, so the control is a single button; and the terminal
  states are "Added publisher, description" or "Nothing to add" rather
  than delivered/failed. **"Nothing to add" is a success** — it is the
  ordinary outcome for a book whose embedded metadata is already complete
  and for any book no provider had an answer for, and rendering it as a
  failure would train people to distrust a working feature. That is what
  `EnrichResultOK` carries, and a mutation test asserts it. The result
  names the fields rather than saying "done" (`enrichmentResultLine`, the
  `searchSummary` convention) so nobody has to hunt the page for what
  moved. With no provider configured the control renders the same disabled
  treatment the send control shows without Resend, and the POST 503s with
  that fragment rather than 404ing.
- The masthead's `site-header` partial takes two fields shared by every
  full-page render — `Nav` (`[]navItem`, composed by the package-private
  `navFor`) and `HeaderNote` (a plain string) — replacing what used to be
  a hardcoded single nav item and a `Count`/`CountText` pair the template
  pluralized itself. `libraryPage` and `bookDetailPage` lost `Count` and
  `CountText` entirely once `HeaderNote` replaced both uses — a field
  nothing reads is how the next person learns to distrust the struct.
  `headerBookCount` composes the library-total note ("1,284 books") both
  pages share; the history page (below) composes its own scope line
  instead. `navFor(current)` builds both nav entries every time, marking
  one current — rendered as plain text, not a link, since there is
  nowhere more useful to send someone already on the page a link would
  point to — so Library and History can never drift into describing each
  other inconsistently. The book detail page passes `navFor("library")`:
  it has no nav entry of its own, so highlighting Library there matches
  what the single hardcoded item did for every page before History
  existed.
- `GET /history` (`internal/web/history.go`, `history.html`) is the send
  history view DESIGN.md's send-to-Kindle section was missing: every send
  across the library over `service.SendHistory`'s trailing window, newest
  first, answering "did I already put this on the Kindle?" for the whole
  library rather than one book at a time. It renders even when
  `sendEnabled` is false — a log, not an action, so a library that used to
  send but no longer has a key configured still has history worth
  reading. Each `historyRow` is composed entirely in the handler, the
  `searchSummary` convention: `Status`/`StatusKind` come from
  `historyStatus`, which collapses `queued` and `sending` into one
  "Sending" label and `pending` kind — the same collapse the send control
  already makes, since the UI has no separate treatment for the gap
  between enqueue and claim, deliberately departing from plate 07's own
  mock (which draws a distinct muted "Queued") because two screens naming
  the same state differently would be worse than either alone.
  `BookURL` is empty for a send whose book has since been pruned, which
  the template renders unlinked rather than pointing nowhere.
  `historyScopeLine` mirrors the masthead's `HeaderNote` contract: "last
  30 days" ordinarily, or naming `service.SendHistoryLimit` once
  `SendHistory` reports the cap actually truncated the window.
  `relativeTime(t, now)` is the plate's timestamp format ("today, 14:02",
  "yesterday, 22:41", "28 Aug, 09:15") as a pure function — `now` passed
  in rather than read from the clock, so every case is a table test with
  no sleeping. It converts both times to the server's local zone (the
  only zone the server knows; the browser's is not available to a
  server-rendered page without JavaScript) and compares calendar dates via
  `AddDate`, never a raw `time.Sub`: a send at 23:50 is "yesterday" twenty
  minutes later at 00:10, which a duration-based `< 24h` comparison gets
  wrong exactly at that boundary.
- Removing a saved recipient is `POST /recipients/remove`
  (`internal/web/send.go`), wrapped in `sameSiteOnly` like every other
  state-changing route, and reachable only from the send control's
  address list — DESIGN.md's "no separate management screen" decision
  stands; this is the delete affordance that decision's own follow-up
  identified as missing, added where the addresses already are rather
  than a new page. The markup problem it solves: an `<option>` cannot
  hold a button, so the saved-address list a `<select>` alone cannot
  express instead lives in the send form's `+ add address` `<details>`,
  one row per address with a "remove" button — and that button cannot be
  a child of `send__form`, since submitting it must never also submit a
  send and HTML forbids nesting a `<form>` inside another besides. The
  fix is a sibling `<form id="recipient-form">`, submitted via each
  button's `form="recipient-form"` attribute (valid anywhere in the
  document, not just inside the form it names) — plain HTML, no JS, no
  duplicated markup — carrying the book id as a hidden field so the
  response can re-render that book's whole send control rather than a
  bare confirmation, since removing an address changes the picker too.
  The handler mirrors `sendHandler`'s progressive-enhancement split: an
  `HX-Request` gets the swapped-in control, everyone else a `303` back to
  the book page, which is what the form's own `action` is for. Removing
  an address that doesn't exist (a double-submit, two open tabs) 200s
  like any other case — a slip, not an error.
- `cmd/server` — entrypoint. `main` sets up logging and a
  `signal.NotifyContext` (SIGINT/SIGTERM) and calls `run(ctx) error`, so
  every failure path has one exit point (`slog.Error` + `os.Exit(1)`).
  `run` opens the database (`DB_PATH` env var, default `./data/library.db`)
  and starts serving immediately — `/healthz` and `internal/web`'s routes
  at `/`, on `ADDR` (default `:8080`), with `ReadHeaderTimeout`,
  `ReadTimeout`, `WriteTimeout` and `IdleTimeout` all set — rather than
  blocking startup on a scan; the initial full sweep of `LIBRARY_DIR`
  (default `./library`) against `COVERS_DIR` (default `./data/covers`,
  passed to the enrichment worker as well as the scanner — a
  provider-fetched cover goes through the same `internal/cover.Store` path,
  so the directory has one shape regardless of which side produced a
  thumbnail) runs in the background alongside the `SCAN_INTERVAL`-timed (default
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
  fails startup. The enrichment worker (`internal/enrich.New`) runs
  unconditionally, unlike the sender: it takes no config to disable itself
  the way `RESEND_API_KEY`/`RESEND_FROM` disable sending, since
  `METADATA_PROVIDERS=` (empty) already resolves to zero providers, which
  makes every job a no-op the same way an unset Resend key does for the
  sender. `METADATA_PROVIDERS` (default `openlibrary,googlebooks`) is
  parsed by `providers.ParseNames` and resolved in order through
  `providers.Resolve`, which composes each name's client with the three
  decorators (`internal/enrich`, above) and — unlike a missing Resend
  key — fails startup outright, naming the bad value and listing the
  valid ones, if a name doesn't resolve: an unset key means "not set up
  yet", but a misspelled provider name means "asked for something
  specific and didn't get it", the kind of silent shortfall nobody
  notices for months. `GOOGLE_BOOKS_API_KEY` is optional and only `Warn`s
  when absent (Google's own anonymous quota still works); its value never
  reaches a log line, in `cmd/server` or inside `internal/googlebooks`
  itself.
  `enrichEnabled` — whether any provider resolved — is what the *UI* gets,
  rather than a config flag of its own: a control offering to fetch
  metadata from nowhere would be a button that cannot do what it says, so
  with `METADATA_PROVIDERS=` the enrichment control renders the same
  disabled treatment the send control shows without Resend, and
  `Service.NotifyEnrichment` is left nil. The worker still runs either
  way, per the paragraph above.
  `storage.RequeueInterruptedEnrichment` runs once before it starts
  claiming jobs, unconditionally too — the requeue-not-fail counterpart to
  `FailInterruptedSends` above, run every startup rather than only when
  sending happens to be configured. The scan loop and both workers run on
  the same `scanCtx`, a child of the signal-aware context but
  independently cancellable — a shared `waitForBackground` helper
  (generalised from the scan-only `waitForScan` once the sender worker
  needed the identical treatment, then reused again for the enrichment
  one) cancels it and waits out a bounded 10s window before the database
  closes, on *both* the SIGINT/SIGTERM path (`run`
  shuts the HTTP server down first, then waits, then closes the database
  — order matters, so no request or in-flight scan/send/enrichment write is
  torn down by the database closing under it) and a serving failure (e.g.
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

Nothing designed in DESIGN.md is unbuilt any more. What remains is its
deferred list — series, tags, format conversion, near-duplicate
detection, a programmatic API, authentication — every item of which was
consciously ruled out of scope rather than left undone.

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
