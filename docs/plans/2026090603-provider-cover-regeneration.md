# Step: stop the scanner fighting over a provider-fetched cover

## Position in the sequence

**Third of the enrichment trio**, and independent of the other two — it
touches `internal/scanner` and `internal/storage`, not the resolver. It is
placed last of the three because it is the least visible: reaching it takes
a provider-supplied cover *and* a lost `COVERS_DIR`, where steps 01 and 02
are wrong on the first press of the button.

## Context

`docs/backlog/2026090309-provider-cover-is-not-regenerable.md`,
re-validated. The item ends "the current behaviour — a repeated Warn and a
dangling path — should not survive step 06." Step 06 shipped without it,
which is what makes this a plan rather than a note: enrichment is now
something a person can ask for, so a book *can* acquire a
provider-supplied cover today.

`COVERS_DIR` is documented — in DESIGN.md, in CLAUDE.md and in
`internal/cover`'s own doc comment — as **disposable**: delete it and the
next sweep rebuilds every thumbnail from the book files. That property is
load-bearing; DESIGN.md's Covers section leans on it to justify storing
only thumbnails.

Step 05 introduced a second source for `books.cover_path`: an image a
provider supplied, stored through the same `internal/cover.Store` path but
with **no embedded original behind it**. `internal/scanner`'s
`maybeRegenerateCover` only knows the embedded source:

```go
coverBytes, err := readEmbeddedCover(sourcePath, matchedSuffix(sourcePath))
...
if len(coverBytes) == 0 {
	slog.Warn("regenerate cover failed", "path", sourcePath, "error", "embedded cover is missing")
	return
}
```

So for a book whose `cover_path` names a file that is now gone, and whose
cover came from a provider, every sweep logs that warning — every fifteen
minutes and on every filesystem event, forever — while `cover_path` keeps
naming a file that is not there. `partials.html` branches on `CoverURL`
being non-empty, not on the file existing, so the grid renders
`<img src="/covers/…">` against a 404: a broken image, permanently.

The ordinary path is untouched. A stored cover that is present and
non-empty short-circuits before any of this, which is why nothing in the
test suite or in normal operation shows it. It takes wiping or losing
`COVERS_DIR` — precisely the operation the directory is advertised as
supporting.

## Scope

In scope: `maybeRegenerateCover` recognising a provider-sourced cover and
clearing it instead of failing to re-extract it, plus the storage method
that does the clearing.

Out of scope, with reasons:

- **Re-fetching the cover from the provider.** The scanner is the
  filesystem indexer; giving it a provider chain, an HTTP client and a
  rate limiter duplicates `internal/enrich` inside the one package that
  has no business making outbound calls. The enrichment worker already
  does this correctly, and clearing the field is what puts the book back
  in its reach.
- **Enqueuing an enrichment job from the scanner.** Tempting — one call
  to `EnqueueEnrichment` and the cover comes back on its own — and ruled
  out on DESIGN.md's authority: "Nothing enriches automatically — not on
  scan, not on a schedule." A wiped `COVERS_DIR` on a large library would
  enqueue a job per affected book, which is the library-wide enrich that
  section declines to build, arriving sideways.
- **Storing the provider's original bytes so they can be re-derived.**
  That is a second cover store with its own lifecycle, to protect against
  an operation whose whole point is that it is cheap because nothing is
  lost.

## Decision 1: the test is "a `field_sources` row exists", not "the row names a provider"

This is the detail the backlog item spends a paragraph on, and it is worth
repeating in the plan because the natural implementation is the wrong one.

`setEmbeddedFieldSourcesTx` does **not** list `FieldCover`. So a cover the
scanner extracted records **no provenance at all** — not `embedded`, not
anything. And `UpdateBookField` refuses `FieldCover`, so `manual` is
unreachable by construction. `ApplyEnrichedFields` is the only writer of a
`cover` row.

Therefore:

| `field_sources` row for `cover` | means |
|---|---|
| absent | the scanner extracted it (or there is no cover) |
| present | a provider supplied it — the row necessarily names one |

A comparison against `"embedded"` would match nothing and the branch would
be dead code that reads as correct. The predicate is row existence.

## Decision 2: clear the cover rather than leave it dangling

Two candidate shapes were left open in the backlog. Taking the first.

When the stored file is missing or zero-byte and a `cover` provenance row
exists, the scanner clears `books.cover_path`, clears `cover_retry`, and
deletes that `field_sources` row — one transaction — logging at `Info`, and
returns without touching `readEmbeddedCover`.

What that leaves is a book that reads, everywhere, as having no cover:

- The grid renders its dashed "no cover" box, which is **honest** — the
  image really is gone — instead of a broken `<img>`.
- The repeated `Warn` stops, because the next sweep's first check
  (`book.CoverPath == "" && !book.CoverRetry`) returns immediately.
- `enrich.Resolve`'s `isMissing` sees an empty value and a source that is
  no longer `manual` (it is now nothing at all), so the cover is back in
  the missing set and **the existing Fetch button repairs it** — no new
  affordance, no new route.

The rejected alternative was to leave `cover_path` alone and downgrade the
log line to `Debug`. It is simpler and it is worse: the broken image stays
on the grid until someone notices and presses a button they have no reason
to think will help, and a `Debug`-level line about a dangling path is a
silenced symptom rather than a fixed one.

**Clearing `cover_retry` alongside is not incidental.** The marker means "a
cover store failed, try again next sweep", and the scanner skips its stat
check entirely while it is set. Leaving it would send the next sweep back
into `readEmbeddedCover` for a book with no embedded cover — reinstating
the warning loop this step exists to end. It is the same pairing
`UpdateBookCoverPath` already makes in the other direction, for the mirror
reason.

**Deleting the `field_sources` row is not incidental either.** A row naming
a provider beside an empty `cover_path` is a claim about a value that no
longer exists, and it is the row `isMissing` would consult if `cover` ever
became a source a person could set. Leaving it would be storing a fact that
is false.

## Storage

One new method in `internal/storage/metadata.go`, beside the other
`field_sources` writers:

```go
// ClearProviderCover forgets a provider-supplied cover whose stored file
// has gone: it empties cover_path, clears cover_retry (the marker means
// "retry the extraction", and there is no embedded original to extract)
// and deletes the cover row from field_sources, so the book reads as
// having no cover and the next enrichment run offers to fetch one again.
// It reports false for an unknown book — the finders' absent-isn't-an-
// error contract.
func (db *DB) ClearProviderCover(ctx context.Context, bookID int64, at time.Time) (bool, error)
```

One `DB.Write`, two statements, per the composition rule — no exported
method is called from inside the callback. It stamps `modified_at` from
`at` the way every other write does, rather than reaching for `time.Now`.

It deliberately does **not** check provenance itself. The caller has
already read `field_sources` to decide it is looking at a provider's cover,
and a method that re-derived that decision would either duplicate Decision
1's subtle predicate or invite a second, differently-wrong copy of it.

## Scanner

`maybeRegenerateCover` gains one read and one branch, both on the uncommon
path — after the stat check has already established the file is missing or
empty, and before `readEmbeddedCover`:

```go
sources, err := db.FieldSourcesForBook(ctx, book.ID)
if err != nil {
	slog.Warn("read cover provenance failed", "book_id", book.ID, "error", err)
	return
}
if _, fromProvider := sources[storage.FieldCover]; fromProvider {
	// See Decision 1: a row exists only when a provider supplied the
	// image — the scanner's own covers record no provenance at all — so
	// there is no embedded original to re-extract. Clear the field
	// instead of warning about it every sweep, which puts the cover back
	// in enrichment's missing set.
	...
	return
}
```

A failure to read provenance returns without clearing and without
re-extracting: a storage error says nothing about where the cover came
from, and guessing either way is how a scanner-extracted cover gets
silently discarded. It warns and lets the next sweep try again — the same
posture the missing-file reconciliation takes toward an ambiguous
`Lstat` failure.

`FieldSourcesForBook` already exists (`internal/enrich`'s worker is its
other caller), so this needs no new query.

## Tests

`internal/storage/metadata_test.go`:

- `ClearProviderCover` empties `cover_path`, clears `cover_retry`, removes
  the `cover` row from `FieldSourcesForBook`, and bumps `modified_at`.
- It leaves every *other* `field_sources` row alone — the row-scoped
  delete, which a `DELETE FROM field_sources WHERE book_id = ?` typo would
  otherwise pass every other assertion.
- Unknown book id → `(false, nil)`.

`internal/scanner`:

- A book with a `cover` provenance row whose stored file has been deleted:
  one sweep clears the path and the row, logs no `regenerate cover failed`,
  and a **second** sweep does nothing at all — the second sweep is the
  assertion that actually pins the bug, since the first would pass under
  the "downgrade the log line" alternative too.
- A book with **no** provenance row whose stored cover has been deleted:
  the existing re-extract path still runs and still restores the cover.
  This is the regression guard for the common case, and it should be an
  existing test that keeps passing rather than a new one.
- A zero-byte stored cover with a provenance row takes the same clearing
  path as a missing one.
- A book with `cover_retry` set and a provenance row: cleared, and the next
  sweep does not re-enter extraction.

## CLAUDE.md

`internal/scanner`'s paragraph currently says a sweep "re-extracts a
recorded cover whose file is missing or zero bytes and refreshes its
stored path, making `COVERS_DIR` disposable." That is now true only of
covers the scanner itself extracted; add the provider case and the
row-existence predicate, since the "a row exists, not a row naming a
provider" rule is exactly what a later reader would try to tidy into a
string comparison. `internal/storage`'s `field_sources` paragraph gains
`ClearProviderCover` as a second reader-and-writer of the `cover` row.

## DESIGN.md (on `init`)

The Covers section's status note claims the "regenerable from the source
file" property "now holds". It holds for embedded covers and cannot hold
for fetched ones — there is no source file to regenerate from. One clause:
a provider-supplied cover is instead *forgotten* when its file goes, which
keeps `COVERS_DIR` disposable by a different route — the library ends up
in a state a person can repair with the button that produced the cover in
the first place.

## Verification

On a dev instance with `METADATA_PROVIDERS=openlibrary`:

- Find or make a book with no embedded cover. Press Fetch and confirm a
  cover appears and `field_sources` gains a `cover` row naming
  `openlibrary`.
- `rm -rf` the covers directory. Run a sweep.
  - The grid shows the dashed "no cover" box for that book, not a broken
    image.
  - `cover_path` is empty and the `cover` row is gone.
  - No `regenerate cover failed` line for it, on that sweep or the next.
  - Books with embedded covers have their thumbnails back, as before.
- Press Fetch on the cleared book: the cover comes back.
