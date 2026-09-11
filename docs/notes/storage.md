# Storage

Rationale for `internal/storage`. The package map and the invariants that
must hold are in CLAUDE.md.

## Engine, pools and writes

SQLite through `modernc.org/sqlite`, chosen over a key-value store because
FTS5 gives full-text search without hand-rolled indexes. The pure-Go
driver keeps `CGO_ENABLED=0`, so the binary cross-compiles and the image
is a single static file. The driver is slower than the C one under heavy
concurrent writes, which does not matter here: writes arrive in scan
bursts and reads dominate.

The database runs in WAL mode with foreign keys on. The DSN names a
5-second `busy_timeout` because the driver applies one only when asked and
otherwise defaults to zero: a lock held from outside the process (a backup
tool, `sqlite3 library.db` opened by hand, WAL recovery after a crash)
would fail a write instantly instead of waiting out the few seconds such a
lock actually holds.

Two pools. The read pool is bounded to `readPoolSize` (8) open and idle
connections, so concurrency above `database/sql`'s default idle ceiling of
two reuses connections rather than opening and discarding one per request.
The write pool holds a single connection, which serialises writes without
a goroutine-and-channel writer. Each exported write method owns exactly
one transaction. A multi-step atomic write composes the package-internal
`…Tx` helpers inside one `DB.Write` call, never two exported methods: the
outer call already holds the pool's only connection, so a nested exported
call blocks until the context expires. A directly nested `Write` is
detected and returns `ErrNestedWrite`.

Every timestamp column holds fixed-width UTC RFC 3339 text
(`sqliteTimeLayout`, `formatTime`), so SQLite's date functions and a plain
`ORDER BY` both work on it.

## Migrations

Embedded SQL files under `migrations/`, one statement each, named
`YYYYMMDDNN_description.sql`, applied in filename order in individual
transactions and recorded in `schema_migrations`. `Open` is idempotent and
runs on every start. The migrator iterates the embedded files and skips
those already recorded, so a recorded name whose file no longer exists is
inert. That is what makes deleting a migration safe while the project is
pre-deployment.

There are no backfill migrations. The service has never been deployed, so
no database predates any table; a `SELECT` that can only match zero rows
is a fixture pretending to be a guarantee. A local development database
older than `books_fts` or `field_sources` is reset by deleting the file
and letting the next sweep rescan.

SQLite cannot alter a CHECK constraint in place, so widening one is a
create-copy-drop-rename sequence of four migrations. `field_sources` and
`book_files` were both rebuilt that way.

## Schema

`books` carries identity and metadata and no location fields. Locations
live in `book_files`, one row per physical path keyed by `book_id`, so
byte-identical content at several paths is one book with several rows.
Content hash is identity; path is a mutable attribute. `derived_from` is
a nullable self-reference reserved for format conversion (see
`design.md`).

`books.sort_title` is derived from the title (one leading English article
stripped, case folded) rather than copied from it, and is declared
`COLLATE NOCASE` so every `ORDER BY sort_title` is case-insensitive without
a per-query clause. `SortTitle` lives here because two writers derive the
column, the scanner on first sight and a title edit, and a second copy of
the rule is a library that sorts differently depending on how a title
arrived.

`NormalizeISBN` and the `Max*` length constants are here for the same
reason, one step further: this package sits below every writer of a
metadata column. Three of them exist — `internal/service` for a person's
edit, `internal/enrich` for a provider's answer, `internal/scanner` for
what a file had embedded in it — and a rule or a number restated in one of
them drifts. For the limits the drift is concrete: a value one writer
stores but another's validation would reject is a field the app can no
longer edit. `formats.md` carries what `NormalizeISBN` accepts and why.

`PlainDescription` is here on the same argument, for a different set of
callers: everything that can be handed a description carrying markup.
`internal/epub` is one, since `dc:description` legally holds escaped HTML,
and `internal/googlebooks` is the other, since the Volumes API documents
its description as HTML. Nothing downstream renders a description as
markup — `html/template` escapes the detail page's — so a tag left in shows
a reader a literal `<p>` and then offers them the same markup to hand-fix
in the edit textarea. Block tags become a line break, every other tag is
dropped, and entities are unescaped only afterwards, so text that was
itself escaped markup survives as the characters an author wrote. A `<`
that starts nothing tag-shaped is left alone, which is what lets a book
about inequalities keep its prose.

`internal/enrich`'s `sanitizeValue` deliberately does not call it. Open
Library's description is plain to begin with, and a strip applied to every
provider answers for a source that never sends markup.

Authors are a table with a `book_authors` join, not a comma-separated
column, so correcting a spelling and browsing by author both stay cheap.
The join carries `position`: `author_id` order is first-sight-in-the-
library order, which is not the book's own once an author is shared. A
name credited twice in one file links once, at its first position, since
the primary key is `(book_id, author_id)` and a second insert would roll
the whole book back. `authors.name` has a unique index.

`book_files.book_id` and both `book_authors` keys cascade on delete. A
book's locations and author links are meaningless without it; the author
row survives.

Two methods delete locations, and they differ in who is trusted.
`PruneMissingFiles` deletes exactly the ids it is given and verifies
nothing, because its caller is the scanner, the only place with live
filesystem state. `ForgetMissingFile` deletes one row for a person, and so
carries its own conditions — the row belongs to that book, and is currently
marked missing — as clauses on the `DELETE` rather than a read before it: a
sweep clearing `missing_since` inside the window between such a read and
the delete would forget a path that had just come back. Both then run
`pruneOrphanedBookTx`, so a bookless book cannot be created either way. A
row matching neither condition is `(false, false, nil)`, the same
absent-isn't-an-error contract `DeleteRecipient` holds.

## Provenance

`field_sources` records per field where the current value came from:
`embedded`, `manual`, or a provider's name. It is written in the same
transaction as the book itself (`setEmbeddedFieldSourcesTx` inside
`createBookTx`) and on every edit, so every book carries its markers by
construction and the resolver can trust an absent row to mean "never
recorded".

The rule that is easiest to get backwards: **a cleared field stays
`manual`.** An empty value with a `manual` source is a decision someone
made, and a resolver that inferred provenance from emptiness would undo it.

`CreateBookWithFile` carries the `manual` fields of a book it is about to
orphan onto the book replacing it, when the two share the path and the old
one is left with no locations at all (`inheritFromReplacedBookTx`, the
reasoning in `scanner.md`). The empty `manual` value comes across with the
rest, by the rule above: someone who cleared a wrong publisher does not
want the file's wrong publisher back. `cover` never does, even where a row
claims to be `manual`, since its value is a path keyed to one book's
content hash. The stamp is the new book's own `modified_at`, read back
inside the transaction rather than taken from a clock, so the whole
creation carries one instant — `createBookTx` leaves that column to its
schema default and does not insert it.

`cover` is the eighth field and behaves differently from the other seven.
A `cover` row exists only when a provider supplied the image:
`setEmbeddedFieldSourcesTx` never writes one for a scanner-extracted
cover, and `UpdateBookField` refuses `FieldCover`, so `manual` is
unreachable. The discriminator between a provider's cover and the
scanner's is therefore "a row exists" versus "no row", never a comparison
against `embedded`, which matches nothing. `FieldCover` is also absent
from `metadataFields`, so `ParseMetadataField("cover")` fails. That map
gates `internal/web`'s per-field edit routes and
`service.UpdateBookMetadata`, and `cover_path` holds a path
`internal/cover.Store` produced, not text a person types; accepting the
name would give the web layer a route that reaches `UpdateBookField`,
gets `ErrInvalidMetadataField` and answers 500 where it should 404.

`ApplyEnrichedFields` is the only writer that creates a `cover` row.
`ClearProviderCover` and `UpdateBookCoverPath` remove one, both through
`forgetCoverTx`. Writing `cover_path` from either side also clears
`cover_retry`: that marker means "a store failed, try again next sweep"
and makes the scanner skip its stat check, so leaving it set beside a
fresh path sends the next sweep past the check with no evidence about the
file. A book with an embedded cover would then be re-extracted over the
provider's image, and one without would have a good provider cover
forgotten.

`ClearProviderCover` and `RecordUnusableCover` take the `cover_path` the
caller observed and refuse to write if the row now holds a different
one. The scanner decides from a snapshot and then stats, parses a book
file and reads provenance before its write lands; an enrichment run that
finished inside that window would otherwise have its new cover thrown
away. The returned bool means *cleared*, not *exists*. The `DELETE` is
scoped to `(book_id, field)`, since dropping the field clause would pass
every assertion while erasing the book's whole provenance. Neither method
checks provenance itself: the scanner already has, and a second copy of
the predicate is how the two drift.

`UpdateBookField` writes one scalar and `UpdateBookAuthors` replaces the
author list, each updating value, provenance and the FTS row in one
transaction. `UpdateBookField` refuses `authors` (`ErrInvalidMetadataField`)
because that lives in the join table. Both return `(false, nil)` for an
unknown book, the same contract as the finders. Both call the shared
`updateBookColumnTx`/`updateBookAuthorsTx` with source `manual`, and
`ApplyEnrichedFields` calls the same helpers with a per-field
`sourceName` map, so a job that pulled fields from two providers records
each under the one that answered it.

Before writing each field, `ApplyEnrichedFields` calls
`fieldIsStillMissingTx` inside its own transaction and skips a field
that is no longer missing. `Resolve`'s snapshot can be minutes stale by
the time a provider answers, and with a single write connection any
concurrent manual edit has already committed, so a provider's late answer
can never overwrite a value someone filled or deliberately cleared. It
iterates `metadataFieldOrder` rather than the caller's map so the same
input reads the same way twice, but still validates every key up front:
iterating the known fields alone would pass silently over a typo'd
constant. It returns the fields actually written, which is what
`enrichment_jobs.updated_fields` records.

## Full-text search

`books_fts` is a plain FTS5 table (`title`, `authors`, `description`,
`isbn`, `tokenize='unicode61 remove_diacritics 2'`), not `content='books'`:
`authors` is assembled from a join rather than being a books column, and
a contentless table cannot support delete-by-rowid. Sync is asymmetric on
purpose. Deletion is a trigger, because books die on several independent
Go paths and every future one should be covered for free. Insert and
update go through `syncBookFTSTx`, which recomputes the row via a
`group_concat` join rather than tracking deltas, and runs inside the same
transaction as `createBookTx` and the metadata writers.

`SearchBooks` orders by `(sort_title, id)`, not relevance, so a grid
someone is scanning while they type does not reorder under them. That
happens to be exactly the total ordering paging needs (below).
`MatchedSearchFields` reports which columns produced hits in one round
trip of four `EXISTS`, each scoped with FTS5's `{col} : (expr)` filter;
the parentheses are load-bearing, since without them the filter binds to
the first term only.

### Query sanitisation

`SanitizeFTSQuery` is the one place raw input becomes a `MATCH`
expression. It quotes and prefix-terms every whitespace-separated token,
so nothing reaches `MATCH` unescaped, and it bounds the input, so a
future caller (a programmatic API) is bounded by construction rather than
by remembering to clip.

Two caps, and neither subsumes the other. `MaxSearchBytes` (256) bounds
the input; `maxSearchTerms` (16) bounds the work, since 256 bytes still
admits 128 single-letter tokens, and each search spends its expression on
three queries (`SearchBooks`, `CountSearchBooks`, `MatchedSearchFields`)
against a read pool of eight. Unbounded, one pasted query of 100,000
tokens stalled every page of the application. Terms past the sixteenth
are dropped silently: a search box has nowhere to show a refusal, and
"the first sixteen words were searched" is a result.

The byte cap is applied by `NormalizeSearchQuery`, exported so
`internal/web` renders back the string that was searched rather than a
same-numbered clip made at a different point in the pipeline (the
handler sees the query before control characters are stripped, so a clip
there is not the same cut). The cut lands on a rune boundary for the two
places it is observable: the final term would otherwise search a token
ending in a byte no title contains, and the same string is rendered into
the input and every paging URL, where half a character is a U+FFFD. FTS5
itself accepts a term ending mid-rune and simply matches nothing.

Two query shapes skip the per-word path and become bare digits, matching
the index's own `replace(replace(isbn, '-', ''), ' ', '')`:

- A **complete** ISBN however punctuated: 10 or 13 characters once
  hyphens and spaces are stripped, trailing `X` permitted.
- A **partial hyphenated** ISBN still being typed: digits and hyphens
  only, at least two hyphens, at least four digits, at most thirteen.

The two lower bounds decide which numeric queries stop being title
queries. Two hyphens keeps `1984-2001` a phrase that finds *Collected
Essays 1984–2001*; at one hyphen it would become `"19842001"*` and match
nothing. Four digits keeps `9-1-1` and `1-2-3`, both of which title
books, as title queries, and costs an ISBN-13 nothing since `978-0-`
already carries four. The accepted costs: an ISO date such as
`2026-09-06` reads as an ISBN, and an ISBN-10 with a two-digit registrant
(`0-19-`, `0-14-`) reaches its second hyphen three digits in and waits one
keystroke longer than an ISBN-13. The thirteen-digit cap is observable
only from above: every thirteen-digit digits-and-hyphens query strips to
a complete ISBN and takes the first shape, so exactly thirteen never
reaches the partial one. The partial shape accepts hyphens but not
spaces, because a space separates tokens for the rest of the search box
and `1984-85 2000-01` must stay two terms. An unpunctuated partial is a
single digit token and the per-word path already produces the identical
prefix term.

## Paging cursor

`BookPage` (`AfterTitle`, `AfterID`, `Limit`; zero value is the first
page) is a keyset cursor shared by `ListBooks` and `SearchBooks`. Keyset
rather than `LIMIT`/`OFFSET` for a reason specific to this application:
the library changes underneath the reader. The scanner inserts wherever a
book's `sort_title` falls, so under `OFFSET` an insert above the reader's
position repeats a card on the next page and a delete skips one. A
cursor naming the last row seen has no such window.

`AfterID` is part of the cursor because `sort_title` is not unique: two
editions of one book collide by construction, and a cursor on a
non-unique column loops or skips on the collision. The comparison is
SQLite's row-value form `(sort_title, id) > (?, ?)`, which plans as a
seek on `books_sort_title_id`; the expanded `a > ? OR (a = ? AND b > ?)`
plans as a full index scan. The comparison carries **no** explicit
`COLLATE NOCASE`: the collation is inherited from the column, and
spelling it out turns the seek back into a scan because an explicitly
collated expression is no longer the indexed one. Agreement with the
`ORDER BY` is guaranteed by the schema and pinned by a test whose titles
differ only in case. `Limit: 0` is unbounded, for the scanner's and the
tests' whole-library calls.

## Single-book lookups

`FindBookByID`, `ListBookFiles` and `ListAuthorsForBook` are targeted
queries for the detail page. `ListFilesUnder("")` and `ListBookAuthors`
load the whole library and are right for the grid, wrong for one book.
`ListAuthorsForBook` returns an empty non-nil slice rather than an error
when a book has no authors. `CountFilesByBook` is one `GROUP BY` over
`book_files` for the grid's multi-location badge; it counts missing rows
too, because the detail page lists them for the whole `MISSING_GRACE`
window and two screens linking to each other must agree. A book absent
from the map counted zero rows, which the scanner's orphan pruning is
meant to make unobservable.

## Recipients and send log

`recipients.address` is `COLLATE NOCASE` with a unique index, so
`CreateRecipient` (`INSERT … ON CONFLICT DO NOTHING`, then select) is
idempotent across case: re-adding a known address is a slip, not a
failure. `ListRecipients` orders `last_used_at DESC, address`; SQLite
sorts `NULL` first in descending order, so never-used addresses land last
with no `NULLS LAST` and the picker's default is simply the first row.
`DeleteRecipient` returns `false` for an unknown address for the same
reason and never touches `send_log`.

`send_log.book_id` is the schema's one non-cascading foreign key
(`ON DELETE SET NULL`), with `book_title` denormalised beside it, and
`recipient_address` is a plain string rather than a key. A send log entry
is the record that a thing happened, and the scanner deletes books
routinely; cascading would erase the evidence a book was ever sent, which
defeats the log's purpose of answering "did I already put this on the
Kindle?". `ListSendsSince` reads both denormalised columns straight off
`send_log` rather than joining: a join would silently drop exactly the
pruned-book and removed-recipient rows the denormalisation exists to
keep. `status` is CHECK-constrained to the four states, so a typo in a Go
constant fails at the write rather than producing a job no worker claims.

`book_id` goes `NULL` only when the book is really gone. A same-path
replacement re-points those rows onto the replacement instead
(`repointSendLogTx`, called before the orphan is deleted): the book is
still on the shelf under new bytes, and both the detail page's status box
and the "did I already send this?" answer read `book_id`. `book_title`
stays as it was, since it records what was sent rather than what the book
is called now.

`EnqueueSend` inserts the `queued` row and bumps `recipients.last_used_at`
in one transaction. The bump belongs at enqueue, not delivery: "most
recently used" means "the one I last chose", and a failed send must not
reset the picker's default. The insert is guarded (`INSERT … SELECT …
WHERE NOT EXISTS`) against a `queued` *or* `sending` row for the same
`(book_id, recipient_address)`: a `sending` row is a message in flight,
and a second one queued behind it is exactly the duplicate the guard
prevents. The address is part of the key because sending one book to two
devices is legitimate. A blocked call returns the pending row's id with
`inserted = false`, so the caller can still bump `last_used_at` without
treating it as a fresh enqueue. The existing `(book_id, queued_at)` index
already bounds the guard's lookup to one book's sends.

`ClaimNextSend` selects the oldest `queued` row and flips it to `sending`
in one transaction, atomic even though today's single worker cannot
contend, because the claim is the one place a second worker would
corrupt. `MarkSendDelivered` and `MarkSendFailed` scope their `UPDATE` to
`status = 'sending'`, so a late or duplicate call can never rewrite a
terminal row; the guard is a silent no-op, not an error.
`FailInterruptedSends` fails every row still `sending` at startup and
never requeues: which side of the in-flight request a dead process was on
is unknowable, and requeueing risks a silent duplicate delivery, while
failing surfaces the doubt and leaves retry a click away. `send_log_queued_at`
exists because neither other index serves the history view's unfiltered
`ORDER BY queued_at DESC`.

## Enrichment jobs

`enrichment_jobs` mirrors `send_log`'s shape (CHECK-constrained `status`,
oldest-first claim, `WHERE status = 'running'` terminal guards) with two
deliberate differences.

`book_id` **cascades**. A send log entry must outlive its book; an
enrichment job is a pending intention about a book and is meaningless
once the book is gone. The `bookGoneReason` the worker records exists
only for the narrow claim-then-delete race.

Startup recovery **requeues** rather than fails
(`RequeueInterruptedEnrichment`). A send's side effect leaves the process
and is not repeatable; an enrichment job only writes fields a pure
function computed from data already in the database, and running it
again lands the same values. A `running` row goes back to `queued` with
`queued_at` reset, unless the book already has a fresh `queued` sibling,
in which case the interrupted row is deleted: requeueing both would give
the book two queued promises and double the provider calls, while the
sibling's own run recomputes the missing set from scratch anyway.
Marking it `done` would misreport a crash as a success.

`EnqueueEnrichment` is idempotent against a `queued` row only. A
`running` job does not block a fresh promise, since it may already be
past the point where a new request could influence it.

`updated_fields` is a comma-separated list of the fields a run wrote,
display text for one fragment that is never queried, which is why it is
not a join table. `MarkEnrichmentDone` stores it in the same statement as
the terminal state; empty is the ordinary "nothing to add" success.

`FieldSourcesForBook` is the resolver's read of provenance. A field
absent from the map reads as an empty source, which the missing-field
rule treats as not-`manual`.
