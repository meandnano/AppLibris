# Step: Enrichment in the UI

## Position in the sequence

**Third of four.** Depends on `2026090304` (queue, resolver) and
`2026090305` (providers). After this, DESIGN.md's metadata feature is
complete and the only remaining plan is `2026090307` (grid paging),
which is independent of all three.

Steps 04 and 05 build a mechanism that runs invisibly. This step is what
makes it something a person can ask for, watch, and trust — and it is
deliberately last of the three, because a UI designed before the
mechanism works ends up shaped around states that turn out not to exist.

## Context

This is the one step in the sequence with **no mockup**. The handoff's
seven plates cover the library, search, book detail, inline editing, the
send control and send history; `ui-handoff/SCREENS.md` §05 describes
inline metadata editing without mentioning provenance at all, and
DESIGN.md designs the enrichment mechanism without designing a surface
for it.

That absence should shape the step rather than be papered over. The rule
followed here: **add the smallest surface that answers the questions
enrichment creates, reusing existing patterns rather than inventing new
ones.** Concretely, enrichment raises exactly three questions a person
will have, and the plan is one affordance each:

1. *Where did this value come from?* — provenance on the detail page.
2. *Can I fetch metadata for this book now?* — a trigger.
3. *Did it do anything?* — a result the trigger swaps to.

Anything beyond those three is invention, and is scoped out below.

## Scope

In scope: showing a field's source on the book detail page, a per-book
"fetch metadata" control, and the queue-state feedback it swaps into.

Out of scope, with reasons:

- **A library-wide "enrich everything" button.** It would put every book
  on the queue, take hours behind a rate limiter, and have no honest
  progress display short of building one. A person who wants that can
  want it after using the per-book version — and if they do, the queue
  already supports it and the missing piece is only the progress surface.
- **An enrichment history page.** `/history` exists for sends because a
  send is an irreversible outbound act you may need to prove happened.
  Enrichment is repeatable and its result is visible in the fields
  themselves, so a log would be a page nobody opens.
- **Editing provenance.** A source is a fact about where a value came
  from, not a setting.
- **Automatic enrichment on scan.** Kept out of steps 04 and 05 for the
  same reason and still out here: the first thing a person should see is
  enrichment they asked for, on a book they chose. Making it automatic is
  a one-line change once the manual path has been watched working, and it
  is much easier to add than to take back.

## Decision 1: provenance is shown, quietly, and only where it is not obvious

Every one of the seven fields has a source, but rendering seven source
labels would double the visual weight of the metadata block to say
"embedded" seven times — which is the default and therefore not
information.

**Decision: show a source marker only for fields whose source is a
provider name.** `embedded` and `manual` render nothing.

The reasoning is what the marker is *for*. A value the scanner read out
of the file is a fact about the file. A value someone typed is theirs. A
value a third-party API guessed at is the only one whose origin changes
how much you should trust it — and the only one where "where did this
come from?" is a question that gets asked. Marking all three treats them
as equivalent; marking one treats provenance as a caveat, which is what
it is.

The marker reuses the mono `--fg-faint` treatment the detail page already
uses for secondary annotation (the same register as a missing location's
note), sitting after the value. No new colour, no badge, no icon.

`manual` renders nothing *deliberately* even though it is the source the
resolver most cares about: the person who typed it does not need telling,
and a "manual" label next to a field with an edit affordance is noise.

## Decision 2: the trigger reuses the send control's state machine

The per-book control has the same shape as send-to-Kindle: a button that
posts, becomes a pending state that polls, and resolves to a terminal
state. That is not a coincidence to be tidied away — it is the same
underlying thing, a queued background job against one book.

So reuse the pattern deliberately and visibly:

- `POST /books/{id}/enrich` — enqueues, wrapped in `sameSiteOnly` like
  every other state-changing route.
- `GET /books/{id}/enrichment` — the poll target, scoped under the book
  id so a mismatched pairing 404s rather than leaking one book's job
  state under another's page, exactly as `GET /books/{id}/sends/{sendID}`
  does.
- One swap region with a stable id; a terminal fragment carries no
  `hx-trigger`, so polling stops by construction rather than by a
  counter.
- Progressive enhancement by the same single-markup-path rule: the form
  carries both an `action` and an `hx-post`, and a non-htmx request gets
  a `303` back to the book page, whose initial render picks the job up.

Where it *differs*, and why: there is no recipient picker, so the control
is a single button; and the terminal states are "updated N fields" or
"nothing to add" rather than delivered/failed. "Nothing to add" being a
success is the important one — it is the common outcome for a book with
complete embedded metadata, and rendering it as a failure would train
people to distrust a working feature.

## Decision 3: the result names what changed

A terminal fragment that just says "done" makes the person hunt for what
moved. The result names the fields: *"Added publisher, description."* —
composed in the handler, the `searchSummary` convention.

This requires the job to record what it wrote, which step 04's schema
does not carry. Add a column rather than re-deriving it:

Migration `2026090308_add_enrichment_jobs_fields_column.sql`:

```sql
ALTER TABLE enrichment_jobs ADD COLUMN updated_fields TEXT NOT NULL DEFAULT '';
```

A comma-separated list of field names, written by the worker in the same
transaction as the terminal state. Comma-separated rather than a join
table because it is display text for one fragment, never queried, and a
table would imply it is data.

The alternative — diffing the book before and after — was rejected: it
cannot distinguish a field enrichment filled from one a concurrent edit
filled, and the whole point of the line is to say what *this* run did.

## Service

- `Service.EnrichBook(ctx, bookID) (*EnrichmentState, error)` — enqueues
  and returns the state, `nil, nil` for an unknown book (the `GetBook`
  contract).
- `Service.EnrichmentState(ctx, jobID)` and `LatestEnrichment(ctx,
  bookID)` — mirroring `SendState`/`LatestSend`, including the same
  collapse of "when did this happen" into one `At` field through a shared
  shaping helper.
- `BookDetail` gains `FieldSources map[string]string` so the detail page
  can render Decision 1's markers.

Note the symmetry with sending is now three pairs deep (state, latest,
shaping). If a third such job type ever appears, that is the moment to
factor a generic job surface — not now, on two, where the abstraction
would have exactly two users and no third to test its shape.

## Web transport

`makeFieldViews` already builds all seven field views in one place, which
is where the source marker belongs — one place, so a whole-page render
and a single-field fragment cannot disagree about provenance. Add
`SourceNote string` to the field view, set only for provider sources per
Decision 1.

**The interaction worth getting right:** editing a field must clear its
provider marker, because saving sets the source to `manual`. The existing
`POST /books/{id}/metadata/{field}` returns the read-view fragment, and
that fragment is rebuilt through `makeFieldViews`, so this works
correctly *provided* the handler reloads the book rather than echoing the
submitted value. It already reloads. Add a test pinning it, because it is
exactly the kind of thing a later "optimisation" removes.

## Tests

- A field sourced `openlibrary` renders its marker; `embedded` and
  `manual` render none.
- Editing a provider-sourced field and saving clears the marker in the
  returned fragment.
- `POST /books/{id}/enrich` enqueues and returns the pending fragment;
  the pending fragment carries a poll trigger and a terminal one does not.
- A job id belonging to another book 404s on the poll route.
- The result line names the fields written, and "nothing to add" renders
  as a success rather than a failure.
- Non-htmx `POST` 303s back to the book page.
- `sameSiteOnly` rejects a cross-site enrich POST.

**Mutation checks:**

1. Render the marker for `manual` too → the no-marker test fails.
2. Leave the poll trigger on the terminal fragment → the stops-polling
   test fails.
3. Make "nothing to add" a failure state → its test fails.

## CLAUDE.md

The `internal/web` bullet gains the two routes and Decision 1's rule with
its reasoning — that provenance is a caveat, so only the caveat-worthy
source is shown. The `internal/service` bullet gains the enrichment state
surface and the note about *not* generalising it over the send surface
yet.

## DESIGN.md (on `init`)

The Metadata section gains a short note that enrichment is reachable per
book from the detail page and that provider-sourced fields are marked,
plus the sentence that the UI for this was not in the handoff and was
kept to three affordances deliberately. The Web UI section's list of
htmx interactions grows from three to four.

## Verification

- `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test -race ./...`.
- The three mutations above.
- Manual, end to end with real providers: a sparse FB2 book with no
  publisher or description, enriched from the detail page, showing the
  pending state, then the fields filled with markers; then edit one and
  confirm its marker clears.
- Manual: a book with complete metadata, confirming "nothing to add"
  reads as success.
- Manual: with `METADATA_PROVIDERS=` the control should not offer an
  action it cannot perform — the same disabled treatment the send control
  uses when Resend is unconfigured, and for the same reason.
- Both themes.
