# Step: Send history view, and removing a saved recipient

## Context

DESIGN.md's send-to-Kindle section is **Built** except for one row in the
status table — `Send to Kindle: history view, recipient management` —
and it is precise about what each half means:

> Not built: **the send history view**. The job model and log that make it
> possible are in place, and the detail page shows a book's own most
> recent send, but there is no page listing sends across the library — so
> "did I already put this on the Kindle?" is answerable per book and not
> yet in general. Recipient management is a subtler gap. "No separate
> management UI" above is a decision and still the right one — but its
> consequence was not thought through: a mistyped address, once saved, is
> permanent and sits in the picker forever, because saving happens as a
> side effect of sending and nothing can remove a row. That is a defect of
> this design rather than of its implementation, and the fix is probably a
> delete affordance on the picker rather than the management screen this
> section rules out.

So this step is **not** "build recipient management". It is the history
view, plus the one affordance that repairs a mistyped address. Those are
one step rather than two because the second is roughly twenty lines and
would otherwise wait behind a page it has nothing to do with.

Everything the history needs already exists. `send_log` keeps
`book_title` denormalised beside a nullable `book_id`, `recipient_address`
as a plain string rather than a foreign key, and `failure_reason` — all
three chosen so the log survives its subjects. From the send-to-Kindle
plan's own reasoning, restated in CLAUDE.md: a send log entry is *the
record that a thing happened*, which is why `book_id` is the schema's one
non-cascading foreign key. This step is what finally reads those columns.

**The design is drawn.** Plate 07 of `ui-handoff/mockups/Bookshelf
Mockups.dc.html` (on the `init` branch) is this page, and
`ui-handoff/SCREENS.md` §07 carries the build notes:

> Built to answer one question: is this book already on the Kindle?
>
> **Data:** `send_log` joined to books and recipients. Not built yet.

Note that "joined to books and recipients" is the one place the handoff
is out of date with the schema, and following it would be a bug: the log
denormalises the title and address precisely so it does *not* join. A
delivered book that was later deleted from the library still has to
appear in its history, and an inner join would silently drop it. Read the
columns; do not join.

## Scope

In scope:

- `GET /history` — the page plate 07 draws, listing sends across the
  library.
- A "History" item in the masthead nav, which today hardcodes a single
  current-page link.
- Removing a saved recipient, from the control that already lists them.

Out of scope, with reasons:

- **A recipient management screen.** DESIGN.md rules it out, and the
  reason still holds: with two addresses in practice, a screen is
  ceremony. The delete affordance goes where the addresses already are.
- **Editing a recipient's address or label.** A mistyped address is fixed
  by removing it and adding the right one, which is the same number of
  actions as editing and needs no new route, no validation path and no
  question about what happens to history rows written under the old
  spelling. Relabelling is a nicety nobody has asked for.
- **Retrying from the history page.** Plate 07 draws no button, and retry
  already exists on the book detail page where the send control lives.
  Adding a second retry entry point means a second place that has to
  reason about `sendEnabled` and about a row whose book has been deleted.
- **Pagination and filtering.** See the scope decision below — the window
  is bounded by time, and at the volumes DESIGN.md describes ("a handful
  of sends a week") that is a page of rows, not a list needing paging.
- **Per-recipient or per-status views.** Same reason. The marker of when
  this becomes wrong is a scope line that starts truncating; see below.

## Decision 1: what the history's window is, and saying so honestly

Plate 07 puts "last 30 days" in the masthead as a scope line. That is the
right shape — an unbounded log is a page whose render cost grows forever —
but a time window alone is not a bound: nothing stops a scripted burst
from putting ten thousand rows inside it.

**Decision: a 30-day window with a row cap, and a scope line that changes
when the cap bites.** `ListSendsSince` takes both, and the handler
composes the line:

- under the cap → `last 30 days`
- at the cap → `last 30 days · 500 most recent`

The second form is the point. A fixed "last 30 days" over a truncated
list is a screen stating something false, and this page exists to be
believed — its whole job is answering "did I already send this?", where a
wrong "no" is worse than no page at all. The cap is 500 because at
DESIGN.md's stated volume it is roughly a decade of sends, so the second
form should never appear in practice; it exists so that if it ever does,
the page says so instead of lying.

Write the cap and the window as named constants with that reasoning in a
comment, the same way `resend.MaxAttachmentSize` carries its arithmetic.

## Decision 2: how a timestamp is formatted, and where the clock lives

Plate 07 renders times as `today, 14:02`, `yesterday, 22:41`, `28 Aug,
09:15` — relative for the recent ones, absolute past that. That needs a
"now", and this repo has a rule about clocks: `Service.now` is the
package's clock so writes can be asserted without timing assumptions.

But formatting is a transport concern here — `SendState.At` is a
`time.Time` and `internal/web` already formats it (`send.At.Format(...)`
in `applySendState`). Moving formatting into the service to reach its
clock would put presentation there to solve a testing problem.

**Decision: a pure function in `internal/web`, taking both times.**

```go
// relativeTime renders t the way plate 07 does — "today, 14:02",
// "yesterday, 22:41", "28 Aug, 09:15" — with now passed in rather than
// read from the clock, so every case is a table test with no sleeping
// and no injected clock. Day boundaries are calendar days in the
// server's local zone, not 24-hour spans: a send at 23:50 is "yesterday"
// at 00:10, which is what a person means by it.
func relativeTime(t, now time.Time) string
```

The calendar-day rule is the part to get right and the part a naive
implementation gets wrong: `now.Sub(t) < 24*time.Hour` is not "today",
and the difference shows up exactly once a day, at the boundary, which is
where nobody looks. Compare `y, m, d := t.Date()` against `now.Date()`.

The zone is the server's local zone, which is the only zone the server
knows — the browser's is not available to a server-rendered page without
JS. Worth one line in the doc comment so it is a decision on the record
rather than an accident of `time.Local`.

## Decision 3: where "remove address" goes, without nesting a form

The addresses are listed in exactly one place: the send control's `+ add
address` `<details>` on the book detail page. That is where DESIGN.md
says the affordance belongs.

The obstacle is structural. The send control is a single `<form>`
(`hx-post` to `/books/{id}/send`), the picker and the add-address inputs
are inside it because they are submitted with the send, and **HTML
forbids nested forms** — so a remove button cannot simply be dropped in
beside each address. Nor can it be a bare `<button>` with only `hx-post`:
this repo's rule is one markup path, not an htmx path plus a no-JS path
that drifts, and every state-changing route already has an `action`
fallback.

**Decision: a second sibling `<form>` inside the same `#send` swap
region, targeted by the `form` attribute.**

```html
<div class="detail__send" id="send">
  <form id="send-form" method="post" action="/books/{{.ID}}/send" hx-post="..." hx-target="#send">
    ... picker, button, and the add-address details ...
  </form>

  <!-- Sibling, not nested: HTML forbids nesting, and a remove control
       inside the send form would submit a send. -->
  <form id="recipient-form" method="post" action="/recipients/remove"
        hx-post="/recipients/remove" hx-target="#send" hx-swap="outerHTML">
    <input type="hidden" name="book" value="{{.ID}}">
  </form>
</div>
```

and, inside the `<details>` (which stays in the send form), each saved
address gets:

```html
<button class="send__remove" type="submit" form="recipient-form"
        name="address" value="{{.Address}}"
        aria-label="Remove {{.Address}}">remove</button>
```

Three things make this the right shape:

- `form="recipient-form"` is plain HTML — a submit button may live
  anywhere in the document and submit a form declared elsewhere. No JS,
  no nesting, no duplicated markup.
- `name="address" value="..."` on the *button* is what carries which
  address, so there is no hidden input per row and the button that was
  pressed is the only one whose value is submitted.
- The hidden `book` field is what lets the response re-render the whole
  send control for that book. Removing an address changes the picker, so
  the fragment that comes back has to be the control, not a bare list.

The route is `POST /recipients/remove`, wrapped in `sameSiteOnly` like
every other state-changing route. It is *not* under `/books/{id}` — a
recipient is not a property of a book, and scoping it there would imply
that removing an address on one book's page leaves it on another's.

**What removal must not touch: `send_log`.** `recipient_address` is a
plain string, not a foreign key, specifically so deleting a recipient
never orphans or rewrites history. That is already guaranteed by the
schema — there is no cascade to disable — so the work here is a test
pinning it, not code.

## Storage

Two new methods in `internal/storage/sends.go`.

```go
// ListSendsSince returns sends queued at or after since, newest first,
// capped at limit rows. Newest-first with a cap means the cap drops the
// oldest rows in the window, which is the right end to lose.
//
// It reads book_title and recipient_address from send_log rather than
// joining books or recipients. That is the whole point of denormalising
// them: a book the scanner has since pruned, or an address since
// removed, must still appear in its own history, and a join would
// silently drop exactly those rows.
func (db *DB) ListSendsSince(ctx context.Context, since time.Time, limit int) ([]Send, error)
```

```sql
SELECT <sendColumns> FROM send_log
WHERE queued_at >= ?
ORDER BY queued_at DESC, id DESC
LIMIT ?
```

`id DESC` breaks ties the same way `LatestSendForBook` already does, so
two sends queued in the same second order consistently between the two
screens.

**Index.** The existing indexes are `send_log(status, queued_at)` and
`send_log(book_id, queued_at)`; neither serves an unfiltered
`ORDER BY queued_at DESC`. Add one migration:

```
2026090301_create_send_log_queued_at_index.sql
CREATE INDEX send_log_queued_at ON send_log(queued_at);
```

(Numbering follows the migration convention — `YYYYMMDDNN`, one statement
per file, next same-day sequence.)

```go
// DeleteRecipient removes a saved address. Returns false when no such
// address exists, the same absent-isn't-an-error contract the finders
// use — a double-submitted remove is a slip, not a failure.
//
// send_log is deliberately untouched: recipient_address is a plain
// string rather than a foreign key precisely so removing an address
// cannot erase or rewrite the record that something was sent to it.
func (db *DB) DeleteRecipient(ctx context.Context, address string) (bool, error)
```

Match on `address` rather than id: the button submits the address, the
column is `COLLATE NOCASE` with a unique index on it, and that collation
is what makes removing `Mike@Kindle.com` remove the row saved as
`mike@kindle.com` — the same case-insensitivity `CreateRecipient` already
relies on to be idempotent.

## Service

```go
// SendRecord is one row of the history view.
type SendRecord struct {
	SendID        int64
	BookID        int64  // 0 when the book has since been deleted
	BookTitle     string
	Recipient     string
	Status        string
	FailureReason string
	At            time.Time
}

// SendHistory returns recent sends, newest first, and whether the row cap
// truncated the window — which the scope line has to say, since a fixed
// "last 30 days" over a truncated list is a claim the page cannot support.
func (s *Service) SendHistory(ctx context.Context) (records []SendRecord, truncated bool, err error)

// RemoveRecipient deletes a saved address. Reports whether one went.
func (s *Service) RemoveRecipient(ctx context.Context, address string) (bool, error)
```

`SendHistory` computes `since` from `s.now()`, which is what makes the
window testable without waiting.

`At` collapses to one field the same way `SendState.At` does —
`finished_at` once terminal, else `queued_at` — and should go through the
same unexported shaping helper (`sendStateFrom`'s sibling, or the shared
piece factored out of it) rather than a second copy of the rule. Two
screens deriving "when did this happen" differently is the defect that
one-field collapse exists to prevent.

`BookID` is flattened from `sql.NullInt64` to `0` here rather than
carried as a nullable, matching how `SendState.BookID` already reports a
pruned book. The transport then has one rule — link when non-zero — with
no null handling in a template.

## Web transport

**Route:** `GET /history` → `history.html`, alongside `library.html` and
`book.html`.

**Row view model**, with every string composed in the handler per the
`SearchSummary` convention:

```go
type historyRow struct {
	Title      string
	BookURL    string // "" when the book has been deleted — renders unlinked
	Recipient  string
	Status     string // "Delivered" / "Sending" / "Failed" / "Queued"
	StatusKind string // "ok" / "pending" / "err" / "muted" — drives the class
	Reason     string
	When       string
}
```

`StatusKind` rather than a colour: plate 07 colours the status cell and
nothing else, but the colour belongs in CSS. A `history__status--err`
class beats a `style="color: {{.Color}}"` the mockup's own data model
uses, which would put a token name in Go.

`queued` and `sending` both render **"Sending"** with the same kind, the
same collapse the send control already makes — the UI has no separate
treatment for the gap between enqueue and claim. Plate 07's data shows a
separate "Queued" in `--fg-muted`; it is drawn from a mock, and the built
control's own rule should win, since two screens naming the same state
differently is worse than either naming. (Recorded here because it is a
deliberate departure from the plate, not an oversight.)

**Markup**, from plate 07: four columns —
`minmax(0, 2.4fr) minmax(0, 1.3fr) 120px 140px`, gap `24px`,
`align-items: baseline`, rows separated by `--rule-faint` hairlines with
`16px 0` padding, no zebra striping and no card per row. Title in serif
18px, recipient and timestamp in mono, timestamp right-aligned, failure
reason on a second line under the title in mono `--err`.

Use a `<ul>`/`<li>` with CSS grid on the row, not a `<table>`. The
reason-under-title layout is two lines inside the first cell, which a
table row cannot express without a nested table or a colspan trick.

**Empty state.** No sends yet gets its own block, phrased for its own
next action, the same way `search__empty` and `empty` are deliberately
separate: "Nothing sent yet — open a book and send it to your Kindle."
Do not reuse the library's empty block.

## The masthead, which this page changes

`site-header` hardcodes one nav item as current:

```html
<nav class="masthead__nav">
  <span class="masthead__link masthead__link--current" aria-current="page">Library</span>
</nav>
```

and its right-hand slot is the book count with the template pluralizing
on `Count`. Plate 07 needs two nav items with History current, and puts
`last 30 days` where the count sits.

**Decision: give the partial one `HeaderNote` string and one `Nav`
marker, and compose both in the handlers.** The right slot becomes
`{{.HeaderNote}}` unconditionally — library and detail set it to
`"1,284 books"`, history sets it to the scope line. That removes the
template's `{{if eq .Count 1}}book{{else}}books{{end}}` branch, which is
the last piece of formatting logic in the shared header and exactly what
the `SearchSummary` convention exists to move.

This is the one part of the step that touches working pages, so it is the
one to verify deliberately: `libraryPage` and `bookDetailPage` both feed
this partial, and `bookDetailPage` only carries `Count`/`CountText` at
all because the partial demanded them. If `HeaderNote` replaces both,
delete them rather than leaving them set-but-unread — a field nothing
reads is how the next reader learns to distrust the struct.

The Library link becomes a real `<a href="/">` when it is not current,
which it never was before, since there was nowhere to go.

## Tests

**Storage:**

- `ListSendsSince` returns newest first, excludes rows older than
  `since`, and respects `limit`.
- A send whose book has been deleted still comes back, with a NULL
  `book_id` and its title intact. This is the join regression — the one
  the handoff's own note would have caused.
- A send to an address that has since been removed still comes back.
- `DeleteRecipient` removes the row, returns `false` for an unknown
  address, and matches case-insensitively.
- **`DeleteRecipient` leaves `send_log` untouched** — count the rows
  before and after. The schema guarantees it; the test is what stops a
  future "tidy up orphaned history" migration from quietly reversing the
  decision.

**Service:**

- `SendHistory` reports `truncated` only when the cap actually bites
  (cap−1 rows: false; cap+1: true).
- The window is computed from the injected clock, so a row queued 31 days
  ago is excluded and one queued 29 days ago is not.
- `At` is `finished_at` for a terminal row and `queued_at` for a pending
  one — the same assertion `SendState` already carries, because the two
  now share a helper and a regression would break only one of them.

**Web:**

- `relativeTime` as a table: same day, previous calendar day, older,
  and — the case worth writing down — 23:50 yesterday viewed at 00:10
  today rendering "yesterday", which a 24-hour-span implementation gets
  wrong.
- A delivered, a failed (with its reason rendered) and a queued row
  render with the right label and kind; `queued` renders "Sending".
- A row whose book is gone renders the title without a link.
- The empty state renders when there are no sends, and is not the
  library's empty block.
- The scope line reads `last 30 days` normally and names the cap when
  truncated.
- `POST /recipients/remove` removes and returns the re-rendered send
  control; an unknown address is not an error; a request without
  `Sec-Fetch-Site: same-origin` is rejected by `sameSiteOnly`.
- The no-JS path: the remove button carries `form="recipient-form"` and
  that form has a real `action`, so the assertion is on the rendered
  markup rather than on behaviour a test client cannot exercise.

**Mutation checks**, each guarding a decision rather than a line:

1. Change `ListSendsSince` to `JOIN books` → the deleted-book test fails.
2. Make `truncated` always false → the scope-line test fails.
3. Change `relativeTime`'s day comparison to a 24-hour span → the 23:50
   case fails.
4. Make `DeleteRecipient` also delete matching `send_log` rows → the
   untouched-history test fails.

## CLAUDE.md

- `internal/storage`: `ListSendsSince` and `DeleteRecipient`, with the
  no-join rule and the reason, and the note that removal never touches
  `send_log`.
- `internal/service`: `SendHistory` and its `truncated` flag; the shared
  `At` shaping now feeding two screens.
- `internal/web`: `GET /history`, `POST /recipients/remove`, the
  `form`-attribute arrangement and why (nested forms are illegal, one
  markup path), `relativeTime`'s calendar-day rule, and `site-header`
  gaining `HeaderNote`/`Nav`.

## DESIGN.md (on `init`)

Once this ships, the status row becomes:

```
| Send to Kindle: history view, recipient management | Built — history at /history; a saved address can be removed from the picker |
```

The section's closing paragraphs need rewriting rather than deleting:
"Not built: the send history view" goes, and the recipient-management
paragraph should keep its analysis — "no separate management UI" remains
the decision — while recording that the consequence it identified is
fixed, and how. That paragraph is a good example of the document catching
a design defect rather than an implementation one, and it should read
that way afterwards.

The out-of-scope section's "unbuilt but not deferred" list drops the send
history view, leaving provider enrichment and the multi-location badge
(the latter until `2026090302` lands).

## Verification

- `gofmt -l .`, `go vet ./...`, `go build ./...` clean.
- `go test -race -count=1 ./...`.
- The four mutations above, each confirmed to turn its own test red.
- Manual, against the real binary with `RESEND_API_KEY`/`RESEND_FROM`
  set: send a book, watch the row move `Sending → Delivered` on
  `/history`; force a failure (an oversized file) and confirm the reason
  renders under the title.
- Manual, with sending unconfigured: `/history` still renders — it is a
  log, not an action — and the nav link works.
- Remove an address, confirm it leaves the picker and that the history
  rows naming it are unchanged.
- Remove an address with JavaScript disabled, which is what the `form`
  attribute is there for.
- Both themes; and a narrow viewport, where four columns at `120px`/
  `140px` fixed are the first thing to overflow.
