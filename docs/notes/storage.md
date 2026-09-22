# Storage

Rules for `internal/storage` and `internal/storage/storagetest`.

## Engine, pools and writes

- **SQLite through `modernc.org/sqlite`.** The pure-Go driver keeps `CGO_ENABLED=0`, and FTS5 covers search.
- **WAL mode, foreign keys on, and a 5s `busy_timeout` named in the DSN.** The driver defaults the timeout to zero, so a lock held from outside the process would fail a write instantly instead of waiting it out.
- **The read pool is bounded to `readPoolSize` (8); the write pool holds one connection.** One connection serialises writes without a goroutine-and-channel writer.
- **Every exported write method owns exactly one transaction. A multi-step atomic write composes the `…Tx` helpers inside one `DB.Write`, never two exported methods, and a `Write` callback never calls an exported `*DB` method.** The outer call holds the pool's only connection, so the inner call blocks until the context expires. A directly nested `Write` returns `ErrNestedWrite`.
- **Every timestamp column holds fixed-width UTC RFC 3339 text (`sqliteTimeLayout`, `formatTime`).** SQLite's date functions and a plain `ORDER BY` both work on it.

## Migrations

- **Embedded SQL files under `migrations/`, one statement each, named `YYYYMMDDNN_description.sql`, applied in filename order in individual transactions and recorded in `schema_migrations`.**
- **`Open` is idempotent and runs the migrator on every start. A recorded name whose file is absent is inert.** The migrator skips recorded names, so deleting a migration is safe while the project is pre-deployment.
- **No backfill migrations. A development database older than a table is reset by deleting the file.** No deployed database predates any table, so a `SELECT` that can only match zero rows is a fixture pretending to be a guarantee.
- **Widening a CHECK constraint is a create-copy-drop-rename sequence of four migrations.** SQLite cannot alter a CHECK constraint in place.

## Schema

- **`books` carries identity and metadata and no location fields. Locations live in `book_files`, one row per path.** Content hash is identity and path is a mutable attribute, so byte-identical content at several paths is one book.
- **`derived_from` is a nullable self-reference reserved for format conversion.** See `docs/notes/design.md`.
- **`books.sort_title` is derived by `SortTitle` (one leading English article stripped, case folded) and declared `COLLATE NOCASE`.** The scanner and a title edit both derive the column, and a second copy of the rule sorts a library differently by how a title arrived.
- **`NormalizeISBN`, the `Max*` constants, `FieldLimit` and `CapField` live here as the one copy shared by `internal/scanner`, `internal/enrich` and `internal/importer`. `internal/service` shares `FieldLimit` only.** A limit restated in one writer drifts, and a value one stores but another rejects is a field the app cannot edit. A person's edit is refused rather than rewritten, and a line break they typed is an error, not something collapsed behind them.
- **`PlainDescription` lives here for `internal/epub` and `internal/googlebooks`, the two callers handed a description that may carry markup. Block tags become a line break, other tags are dropped, character references are decoded afterwards, and a `<` that starts nothing tag-shaped is left alone.** Nothing downstream renders a description as markup, so a tag left in shows a reader a literal `<p>`.
- **Only a reference terminated by `;` is decoded. A terminated reference that names nothing is left to HTML's reading.** `html.UnescapeString` alone also decodes semicolon-less legacy references, which is wrong for prose such as `Rock &copy roll`.
- **Both `PlainDescription` paths return one shape. The test for markup is a fast path over the work, never over the result.** A description capped only when it happened to contain an ampersand would render differently from a file and from a provider.
- **`internal/enrich`'s `sanitizeValue` does not call `PlainDescription`.** Open Library's description is plain, and a blanket strip answers for a source that never sends markup.
- **`CapBlankLines` lives here and is called by `CapField`, `PlainDescription` and `internal/fb2`'s `annotationText`. A person's edit is the one description nothing caps.** Copies of the rule shape descriptions by which door they came through; the blank lines someone typed are their own.
- **`CapBlankLines` is one hand-rolled pass, not the obvious fold-trim-collapse spelling, and a test keeps that spelling as its oracle. It folds CRLF and a lone CR first and strips each line's trailing whitespace before counting.** It runs on a value not yet capped, up to the 4 MiB `internal/epub` allows a package document, and `pre-line` collapses a line of two spaces while keeping both newlines around it.
- **Authors are a table with a `book_authors` join carrying `position`; `authors.name` has a unique index. A name credited twice in one file links once, at its first position.** `author_id` order is first-sight order, not the book's own, and a second insert against the `(book_id, author_id)` key would roll the whole book back.
- **`book_files.book_id` and both `book_authors` keys cascade on delete; the author row survives.** A book's locations and author links are meaningless without it.
- **`PruneMissingFiles` deletes exactly the ids it is given and verifies nothing. `ForgetMissingFile`'s guards (the row belongs to that book and is marked missing) are clauses on the `DELETE`, never a read before it. Both run `pruneOrphanedBookTx`.** The scanner is the only caller with live filesystem state, and a sweep clearing `missing_since` between a read and the delete would forget a path that had just come back. A row matching neither condition is `(false, false, nil)`.

## Provenance

- **`field_sources` is written in the same transaction as the book (`setEmbeddedFieldSourcesTx` inside `createBookTx`) and on every edit.** Every book carries its markers by construction, so an absent row means never recorded.
- **A cleared field stays `manual`. Never infer provenance from emptiness.** An empty value with a `manual` source is a decision someone made.
- **`CreateBookWithFile` carries the `manual` fields of a same-path predecessor left with no locations onto the replacement (`inheritFromReplacedBookTx`), empty values included and `cover` excluded. The stamp is the new book's `modified_at` read back inside the transaction, which `createBookTx` leaves to its schema default.** A cleared value is a decision, a cover path is keyed to one book's content hash, and the whole creation carries one instant. Reasoning in `docs/notes/scanner.md`.
- **A `cover` row exists only for a provider-supplied cover. The scanner-versus-provider test is "row exists", never a comparison against `embedded`.** `setEmbeddedFieldSourcesTx` never writes one for a scanner-extracted cover and `UpdateBookField` refuses `FieldCover`, so `embedded` matches nothing.
- **`FieldCover` stays out of `metadataFields`, so `ParseMetadataField("cover")` returns false. `UpdateBookField` refuses `FieldCover` and `authors` with `ErrInvalidMetadataField` as a second guard.** `cover_path` is a path `internal/cover.Store` produced, not text a person types, and accepting the name would give the web layer a route answering 500 where it should 404. Authors live in the join table.
- **`ApplyEnrichedFields` is the only writer creating a `cover` row; `ClearProviderCover` and `UpdateBookCoverPath` remove one through `forgetCoverTx`.**
- **Writing `cover_path` clears `cover_retry`, from every writer.** The marker makes the scanner skip its stat check, so a fresh path beside it sends the next sweep past the check with no evidence about the file.
- **`ClearProviderCover` and `RecordUnusableCover` take the `cover_path` the caller observed and refuse to blank anything else. Their bool means cleared, not exists. Their `DELETE` is scoped to `(book_id, field)`. Neither checks provenance itself.** An enrichment run finishing between the scanner's snapshot and its write would otherwise lose its new cover. Dropping the field clause would erase the book's whole provenance while passing every assertion. The scanner has already checked provenance, and a second copy of the predicate drifts.
- **`UpdateBookField` and `UpdateBookAuthors` update value, provenance and the FTS row in one transaction and return `(false, nil)` for an unknown book. They share `updateBookColumnTx` and `updateBookAuthorsTx` with `ApplyEnrichedFields`, which passes a per-field `sourceName`.** A job that pulled fields from two providers records each under the one that answered it.
- **`ApplyEnrichedFields` re-checks `fieldIsStillMissingTx` per field inside its own transaction, iterates `metadataFieldOrder` while validating every key up front, and returns only the fields it wrote.** Iterating the fixed order makes the same input read the same way twice. `Resolve`'s snapshot can be minutes stale, so a late answer must never overwrite a value someone filled or cleared. Iterating the known fields alone would pass over a typo'd constant. The return is what `enrichment_jobs.updated_fields` records.

## Full-text search

- **`books_fts` is a plain FTS5 table with `tokenize='unicode61 remove_diacritics 2'`, not `content='books'`.** `authors` comes from a join, and a contentless table cannot delete by rowid.
- **Deletion is a trigger. Insert and update go through `syncBookFTSTx`, which recomputes the row through a `group_concat` join inside the writer's transaction.** Books die on several independent Go paths, and every future one is covered for free.
- **`SearchBooks` orders by `(sort_title, id)`, not relevance.** A grid someone scans while typing must not reorder under them.
- **`MatchedSearchFields` is four `EXISTS`, each scoped with `{col} : (expr)`. The parentheses are load-bearing.** Without them the filter binds to the first term only.
- **`SanitizeFTSQuery` is the one place raw input becomes a `MATCH` expression. It quotes and prefix-terms every token and bounds the input.** A future caller is bounded by construction rather than by remembering to clip.
- **`MaxSearchBytes` (256) bounds the input and `maxSearchTerms` (16) bounds the work. Neither subsumes the other. Terms past the sixteenth are dropped silently.** 256 bytes still admits 128 single-letter tokens, and a search box has nowhere to show a refusal.
- **The byte cap is applied by `NormalizeSearchQuery`, exported so `internal/web` renders back the string that was searched, and the cut lands on a rune boundary.** A clip in the handler lands before control characters are stripped and is a different cut. Half a character is a U+FFFD in the input and every paging URL.
- **Two query shapes become bare digits: a complete ISBN however punctuated (10 or 13 characters once hyphens and spaces are stripped, trailing `X` permitted), and a partial hyphenated one (digits and hyphens only, at least two hyphens, at least four digits, at most thirteen). The partial shape accepts hyphens but not spaces.** Two hyphens keeps `1984-2001` a title query, four digits keeps `9-1-1` one, and a space separates tokens for the rest of the box.

## Paging cursor

- **`BookPage` is a keyset cursor shared by `ListBooks` and `SearchBooks`, not `LIMIT`/`OFFSET`.** The scanner inserts wherever a `sort_title` falls, so under `OFFSET` an insert above the reader repeats a card and a delete skips one.
- **`AfterID` is part of the cursor.** `sort_title` is not unique, and a cursor on a non-unique column loops or skips on the collision.
- **The comparison is the row-value form `(sort_title, id) > (?, ?)` with no explicit `COLLATE NOCASE`, pinned by a test whose titles differ only in case.** The row-value form seeks on `books_sort_title_id` where the expanded `OR` form scans, and an explicitly collated expression is not the indexed one.
- **`Limit: 0` is unbounded.** The scanner and the tests read the whole library.

## Single-book lookups

- **`FindBookByID`, `ListBookFiles` and `ListAuthorsForBook` are the detail page's queries; `ListFilesUnder("")` and `ListBookAuthors` load the whole library and are for the grid. `ListAuthorsForBook` returns an empty non-nil slice for a book with no authors.**
- **`CountFilesByBook` counts missing rows too.** The detail page lists them for the whole `MISSING_GRACE` window, and two screens linking to each other must agree.

## Recipients and send log

- **`recipients.address` is `COLLATE NOCASE` with a unique index, and `CreateRecipient` is idempotent across case.** Re-adding a known address is a slip, not a failure.
- **`ListRecipients` orders `last_used_at DESC, address` with no `NULLS LAST`.** SQLite sorts `NULL` first in descending order, so never-used addresses land last and the picker's default is the first row.
- **`DeleteRecipient` returns false for an unknown address and never touches `send_log`.**
- **`send_log.book_id` is `ON DELETE SET NULL` with `book_title` denormalised beside it, and `recipient_address` is a plain string. Every other FK cascades. `ListSendsSince` reads those columns and never joins `books` or `recipients`.** The log records that a thing happened after the scanner has deleted the book, and a join would drop exactly those rows.
- **`status` is CHECK-constrained.** A typo in a Go constant fails at the write rather than producing a job no worker claims.
- **A same-path replacement re-points `send_log` rows onto the replacement (`repointSendLogTx`, before the orphan is deleted); `book_title` stays as written.** The book is still on the shelf under new bytes, and the status box and the history answer read `book_id`.
- **`EnqueueSend` inserts the `queued` row and bumps `last_used_at` in one transaction, at enqueue and not delivery. The insert is guarded against a `queued` or `sending` row for the same `(book_id, recipient_address)`, and a blocked call returns the pending id with `inserted = false`.** A failed send must not reset the picker's default, a `sending` row is a message in flight, and one book to two devices is legitimate.
- **`ClaimNextSend` selects the oldest `queued` row and flips it to `sending` in one transaction.** The claim is the one place a second worker would corrupt.
- **`Mark*` terminal writes are scoped to the in-progress status and are a silent no-op otherwise.** A late or duplicate call can never rewrite a terminal row.
- **`FailInterruptedSends` fails every row still `sending` at startup and never requeues.** Which side of the in-flight request a dead process was on is unknowable, and requeueing risks a silent duplicate delivery.
- **`send_log_queued_at` exists for the history view's unfiltered `ORDER BY queued_at DESC`.** Neither other index serves it.

## Enrichment jobs

- **`enrichment_jobs` mirrors `send_log`'s shape: CHECK-constrained `status`, oldest-first claim, `WHERE status = 'running'` terminal guards.**
- **`enrichment_jobs.book_id` cascades. `bookGoneReason` exists only for the claim-then-delete race.** An enrichment job is a pending intention about a book and is meaningless once the book is gone.
- **`RequeueInterruptedEnrichment` puts a `running` row back to `queued` with `queued_at` reset, or deletes it when the book has a fresh `queued` sibling. It never marks it `done`.** Running a job again lands the same values, two queued promises would double the provider calls, and `done` would misreport a crash as a success.
- **`EnqueueEnrichment` dedups against `queued` only.** A `running` job may already be past the point where a new request could influence it.
- **`updated_fields` is a comma-separated list, not a join table. `MarkEnrichmentDone` stores it in the same statement as the terminal state, and empty is the ordinary "nothing to add" success.** It is display text for one fragment that is never queried.
- **`FieldSourcesForBook` reads an absent field as an empty source.** The missing-field rule treats that as not-`manual`.

## storagetest

- **`storagetest.Open` hands a test a copy of a template migrated once per test binary. Every package above `internal/storage` but `cmd/server` opens its database this way, and `SeedSends` seeds `send_log` directly.** Migrating afresh costs twenty times a copy, and seeding `send_log` alone is faithful because history never joins. See `docs/notes/testing.md`.
