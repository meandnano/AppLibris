# Step: Enrichment queue and resolver (no network)

## Position in the sequence

**First of four.** The remaining in-scope work on this project is metadata
provider enrichment — DESIGN.md's last designed-but-unwritten feature —
plus library grid paging, which is independent. Ordered:

| # | Plan | Depends on |
|---|---|---|
| **04** | **Enrichment queue and resolver (this one)** | — |
| 05 | Open Library and Google Books providers | 04 |
| 06 | Enrichment in the UI | 04, 05 |
| 07 | Library grid paging | — (independent) |

This one goes first because it is where the design risk is, and because
DESIGN.md says the risky part must be buildable without touching a
network:

> The **resolver logic is kept separate** from the providers so ordering
> and merging are testable without any real provider.

So this step builds the queue, the worker, the provider *interface*, and
the resolver, with a fake provider in tests and none in production. It
ships nothing a user sees. That is the point: the merge rules are the
part that can silently destroy hand-fixed metadata, and they should be
proven before anything real is wired to them.

## Context

`field_sources` has been written on every create and edit since inline
editing shipped, and has been read by nothing. DESIGN.md is explicit that
this was deliberate:

> It is deliberately write-only for now — nothing reads a source until
> there is a resolver to consult one — and that is the point rather than
> an oversight: a book edited today has to carry its marker by the time
> the enrichment step arrives, or that step overwrites hand-fixed values
> with no way to tell they were hand-fixed.

This step is that consumer arriving. Every row written since is what
makes it safe.

There is a strong local precedent to copy rather than invent:
`internal/sender` is already a single-worker queue over a status-tracked
table — claim one row, process it, write a terminal state, woken by a
poke or a ticker. The enrichment worker is the same shape with different
rules, and the places where the rules *differ* are the interesting part
(see "Interrupted jobs requeue" below).

## Scope

In scope: an `enrichment_jobs` table, a single-worker queue in
`internal/enrich`, the `Provider` interface, and the resolver that
decides which fields to ask for and how to merge what comes back.

Out of scope, with reasons:

- **Real providers** — step 05. The whole argument for splitting here is
  that HTTP clients, rate limiting and retry are a separate kind of risk
  from merge correctness, and mixing them means neither gets a clean test.
- **Any UI** — step 06. Nothing here is user-visible, and inventing a
  surface before the mechanism works is how a UI ends up designed around
  the wrong states.
- **Cover enrichment.** `field_sources.field` is CHECK-constrained to the
  seven text fields; a cover has no provenance row and cannot get one
  without a migration. Whether covers are worth fetching at all depends
  on what the providers actually return — Open Library's cover images are
  frequently low-resolution or absent — so the decision belongs in step
  05, next to the evidence, and the migration with it.
- **Automatically enriching on scan.** This step gives the queue an
  enqueue method and a worker that drains it; what *puts* books on the
  queue is left to step 06, where a person can see it happen. A scanner
  that silently enqueues every new book would mean the first real
  provider run happens with no way to watch it.

## Decision 1: what "missing" means — the rule the whole step exists for

DESIGN.md says each provider is asked "only for the fields still
missing", and separately states the rule that is easy to get backwards:

> **a cleared field stays `manual`.** An empty value with a `manual`
> source is a decision someone made, not metadata that is missing. A
> resolver inferring provenance from emptiness would undo exactly the
> edits this table exists to protect.

So "missing" is **not** "the column is empty". It is:

```
missing(field) := value is empty AND source(field) != "manual"
```

Both halves matter, and they fail differently:

- Dropping the emptiness test means re-enrichment overwrites good
  embedded metadata with a provider's guess.
- Dropping the `manual` test means someone who deliberately cleared a
  wrong publisher gets it silently refilled on the next run — the exact
  failure `field_sources` was created to prevent, arriving through the
  code that was supposed to honour it.

Write this as one function with both halves and a table test per branch,
not as a `WHERE value = ''` in a query. It is the single most important
thing in this step.

A field with a *non-empty* value is never asked for, whatever its source.
That means enrichment never "improves" an embedded value, and that is
intended: the scanner's reading of the file is a fact about the file,
while a provider's answer is a guess about the work.

## Decision 2: interrupted jobs requeue — the opposite of a send

`internal/sender` fails interrupted jobs and never requeues them, because
which side of the transport call the process died on is unknowable and
requeueing risks a duplicate *delivery*.

**Enrichment inverts that, and for the same underlying reason.** The
question is always "is the side effect repeatable?", and here it is: an
enrichment job's only effect is writing fields the resolver computed, the
resolver is deterministic given the same inputs, and running it twice
lands the same values. There is no outbound irreversible act.

So startup recovery requeues `running` jobs rather than failing them, and
the worker may safely re-run a job that died mid-flight. State this
inversion in the code comment *with* the sender contrast, because the two
workers otherwise look identical and a reader who has seen one will
assume the other follows it.

The one thing that is not repeatable is the provider's rate-limit budget,
which is step 05's problem and is why retries are decorators there rather
than a loop here.

## Decision 3: one job per book, not one per field

A job names a book. The resolver computes the missing set at claim time,
not at enqueue time — the same "resolve late" rule the sender applies to
file locations, and for the same reason: a queue is a promise to act
later and the record moves underneath it. Someone may have filled a field
by hand between enqueue and claim, and a per-field job would ask a
provider for something that is no longer missing.

Enqueue is idempotent per book: a book with a job already `queued` does
not get a second one. `INSERT … WHERE NOT EXISTS`, in one statement.

## Storage

Migration `2026090302_create_enrichment_jobs_table.sql`:

```sql
CREATE TABLE enrichment_jobs (
    id             INTEGER PRIMARY KEY,
    book_id        INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    status         TEXT NOT NULL
                   CHECK (status IN ('queued','running','done','failed')),
    failure_reason TEXT NOT NULL DEFAULT '',
    queued_at      TIMESTAMP NOT NULL,
    started_at     TIMESTAMP,
    finished_at    TIMESTAMP
);
```

and `2026090303_create_enrichment_jobs_status_index.sql`:

```sql
CREATE INDEX enrichment_jobs_status_queued_at ON enrichment_jobs(status, queued_at);
```

`book_id` **cascades** here, unlike `send_log.book_id`. The contrast is
worth a comment: a send log entry is the record that a thing happened and
must outlive its book, while an enrichment job is a *pending intention*
about a book — once the book is gone the intention is meaningless, and
keeping it would mean a worker claiming jobs for rows that no longer
exist.

Methods in `internal/storage/enrichment.go`, mirroring `sends.go`:

- `EnqueueEnrichment(ctx, bookID, now) (queued bool, err error)` —
  idempotent per book, per the decision above.
- `ClaimNextEnrichment(ctx, now) (*EnrichmentJob, error)` — one
  transaction: oldest `queued`, flip to `running`, return it.
- `MarkEnrichmentDone` / `MarkEnrichmentFailed` — both scoped
  `WHERE status = 'running'`, the same guard `MarkSend*` uses so a
  terminal row cannot be rewritten by a late call.
- `RequeueInterruptedEnrichment(ctx, now) (n int, err error)` — startup
  recovery, per Decision 2. Note the name: it is deliberately *not*
  `FailInterrupted*`, so the difference from the sender is visible at the
  call site in `cmd/server`.
- `FieldSourcesForBook(ctx, bookID) (map[MetadataField]string, error)` —
  the first read of `field_sources` in the project's history.

Also needed: a write that applies a resolved field *with* its provenance
in one transaction. `UpdateBookField` already writes value + source + FTS
row together, and so does `UpdateBookAuthors` — but both hardcode
`"manual"` at the `setFieldSourceTx` call. Generalise rather than adding a
parallel path: thread the source through as a parameter, with the two
existing methods passing `"manual"` and a new `ApplyEnrichedFields`
passing the provider name. Two code paths writing the same three things is
how provenance ends up right in one and forgotten in the other — and there
are already two, so a third would be the one that gets it wrong.

`ApplyEnrichedFields(ctx, bookID, values map[MetadataField]string,
source string, modifiedAt time.Time)` applies the whole resolved set in
**one** transaction, so a book is never left half-enriched.

## The provider interface

Per DESIGN.md, deliberately tiny, and defined in `internal/enrich` — the
consumer side, the same way `sender.Transport` lives with its consumer
rather than in `internal/resend`:

```go
// Provider is one metadata source. Implementations are HTTP clients;
// nothing here knows that.
type Provider interface {
	// Name is used for logging and for the provenance written to
	// field_sources, so it is a stable identifier, not a display string.
	Name() string
	// ByISBN looks a book up by its normalised ISBN. A book with no
	// ISBN never reaches it.
	ByISBN(ctx context.Context, isbn string) (Metadata, error)
	// Search is the fallback when there is no ISBN.
	Search(ctx context.Context, title string, authors []string) (Metadata, error)
}
```

`Metadata` is a struct of the same seven fields, all optional. Returning
a zero value with a nil error means "no answer", which is not an error
condition and must not fail the job — most books will get nothing from
most providers.

## The resolver

A pure function, given the book, its field sources, and an ordered
provider list. No database, no clock:

```go
func Resolve(ctx context.Context, book storage.Book, authors []string,
	sources map[storage.MetadataField]string, providers []Provider,
) (values map[storage.MetadataField]string, sourceName map[storage.MetadataField]string, err error)
```

The loop DESIGN.md specifies:

1. Compute the missing set (Decision 1).
2. If empty, return immediately — no provider is called at all.
3. For each provider in order: ask it (by ISBN when the book has one,
   else by title+author), keep only the fields in the missing set that it
   actually answered, record the provider's name as their source, and
   remove them from the set.
4. When the set empties, **stop early** and skip the remaining providers.

Step 4 is in DESIGN.md as an API-call saving and should be asserted, not
assumed: a test with two providers where the first answers everything
must show the second is never called. A fake provider that records its
calls is how.

A provider returning an error is logged and skipped, not fatal — the
chain continues to the next one, and a job that reached at least one
provider is `done` rather than `failed`. `failed` is reserved for the
job itself going wrong (the book vanished, a write failed), because a
provider having nothing to say is the common case and marking it a
failure would make the queue's status column meaningless.

## The worker

`internal/enrich/worker.go`, mirroring `internal/sender`: single worker,
`Notify()` poke on a capacity-1 channel plus a `pollInterval` ticker, one
job at a time. Copy the shape deliberately — the poke is the
optimisation, the tick is what catches a row left `queued` by a crash
between insert and notify.

Per job: load the book, its authors and its field sources; call
`Resolve`; apply the result with `ApplyEnrichedFields`; mark done. The
terminal write goes under `context.WithoutCancel` with a short timeout,
the same as the sender's — but note the reason is weaker here (a lost
verdict costs a re-run, not a duplicate delivery), so it is consistency
with the neighbouring worker rather than a hard requirement.

`cmd/server` wires it beside the sender: `RequeueInterruptedEnrichment`
once before starting, then the worker on the same `scanCtx`, joined
through the existing `waitForBackground`. With no providers configured
the worker still runs and every job resolves to "nothing missing, no
providers" — which keeps the wiring exercised before step 05 gives it
something to call.

## Tests

**Resolver** — the bulk of the value, all with fakes:

- A field that is empty with source `embedded` is asked for.
- A field that is empty with source `manual` is **not** asked for, and is
  not written. The regression this whole step exists to prevent.
- A field with a value is never asked for, whatever its source.
- A book with no missing fields calls no provider at all.
- Field-level merge: provider A answers publisher, provider B answers
  description, both land, each with its own name as source.
- Early stop: provider B is never called once A has answered everything.
- A provider error is skipped, the next one still runs, the job is done.
- A provider answering a field that was *not* missing has that answer
  discarded — a provider must not be able to widen its own mandate.

**Storage:**

- Enqueue is idempotent while a job is `queued`, and enqueues again once
  the previous job is terminal.
- `ClaimNextEnrichment` takes the oldest and flips it atomically.
- `MarkEnrichment*` are no-ops on a row that is not `running`.
- `RequeueInterruptedEnrichment` puts `running` rows back to `queued` —
  the inverse of `FailInterruptedSends`, asserted as such.
- Deleting a book removes its jobs (the cascade).
- `ApplyEnrichedFields` writes value, provenance and the FTS row in one
  transaction, and rolls all of it back on failure.

**Mutation checks:**

1. Drop the `manual` half of the missing test → the cleared-field test
   fails.
2. Drop the emptiness half → the populated-field test fails.
3. Remove the early-stop → the never-called-provider test fails.
4. Change `RequeueInterrupted` to fail instead → its test fails.

## CLAUDE.md

A new `internal/enrich` bullet covering the resolver's missing-set rule
(both halves, and what each protects), the requeue-versus-fail inversion
against `internal/sender` with the reason, and the resolve-at-claim-time
rule. The `internal/storage` bullet gains the enrichment methods and
notes that `field_sources` finally has a reader.

## DESIGN.md (on `init`)

The Metadata section's status moves from "Embedded and provenance are
built; providers are not" to note that the chain, interface and resolver
exist and only the provider implementations are outstanding. The status
table row stays `Partial` until step 05.

## Verification

- `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test -race ./...`.
- The four mutations above.
- Manual: enqueue a job against a library with no providers configured
  and confirm it resolves to `done` having called nothing, and that a
  book's fields and `field_sources` rows are untouched.
