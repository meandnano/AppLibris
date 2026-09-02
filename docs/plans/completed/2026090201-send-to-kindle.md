# Step: Send to Kindle

## Context

This is the feature the project exists for. DESIGN.md's opening line —
"see what books I have, and send one to a Kindle by email" — names two
actions; only the first is built, and the second is, in that document's
own words, "today, not reachable from the app." Its status table says so
three times over: the transport is *Built, but nothing calls it*, while
the schema, the job model and the recipient picker are all *Not built*.

Everything else in scope is a distant second. The filesystem watcher is,
by DESIGN.md's own framing, "an optimisation, not the mechanism" — the
periodic rescan already does the job. Provider enrichment is a large step
that makes an already-browsable library slightly better rather than
making an unreachable feature reachable. The multi-location badge is a
grid-level echo of what the detail page now shows directly.

**Why this is one step and not three.** DESIGN.md names three
prerequisites and stresses they are "independent of each other": the
`recipients`/`send_log` schema, the queued-job model, and the recipient
picker. They are independent in the sense that none *contains* another —
but they are strictly ordered by usefulness, and any prefix of them
delivers nothing. `internal/resend` is the standing proof: a correct,
tested, thoroughly commented package that has sat unreachable since
`2026083104` because it landed without its caller. Splitting the schema
and the queue into their own step would produce a second, larger
`internal/resend` — more code whose only defence is "the next step will
call it." So this step goes from the button to the delivered state.

What keeps that from being unbounded is aggressive trimming at the
edges — the send *history* view, recipient *management*, delivery
webhooks and automatic retries are all cut below (see Scope), each for a
reason DESIGN.md or UI.md already supplies.

**Dependencies — both have landed; this is written against master.**

- `#28` (`docs/plans/completed/2026090106-full-text-search.md`) vendored
  htmx and established the partial-swap pattern. The send control is the
  second of the three interactions DESIGN.md allocates htmx to, and it
  reuses that pattern wholesale: the fragment-versus-full-page split, the
  fragment-rendering parameter on `render`, the `Vary` discipline.
  Note what that split actually keys on after #28's review round — not
  `HX-Request` alone, but `HX-Request` *without*
  `HX-History-Restore-Request`, since htmx sets both on the request it
  issues when the user goes Back past its history cache and then swaps
  the response into the whole body. Any new route here that serves two
  bodies at one URL copies that rule and names both headers in `Vary`.
- `#29` (`docs/plans/completed/2026090107-book-detail-page.md`) is where
  the control mounts. That plan deliberately left the slot in place and
  empty:

  ```html
  {{/* Send-to-Kindle mounts here once the job model exists — kept in
       plate 03's source position … but empty and collapsed, since
       there's nothing to swap into it yet. */}}
  <div class="detail__send"></div>
  ```

  This step fills exactly that element. Plate 06 of the handoff
  (`ui-handoff/`, `init` branch) draws its four states, SCREENS.md 06
  carries the build notes — including the `min-height: 148px` that plan
  deferred "until the control exists to swap" — and everything visual
  below follows those rather than improvising.

**This step absorbs `docs/backlog/2026083117-resend-client-hardening.md`,
which is deleted in the same change** (per CLAUDE.md's rule for acting on
a backlog item). Its three findings all re-validated against current
`internal/resend`: `NewClient` still hands out `http.DefaultClient` (no
timeout); `sendRequest.Text` is still never assigned and marshals as
`"text":""` with no `omitempty`; a 28MB attachment still exists as bytes,
base64 string, JSON body and reader at once. That item deferred itself
explicitly until "the send-to-Kindle step, with the caller in front of
us" — the caller is now in front of us, and "Resend client" below settles
all three.

## Scope

In scope: `recipients` and `send_log` schema; the queued job model with
its `queued → sending → delivered | failed(reason)` states and a
single-worker queue; `RESEND_API_KEY`/`RESEND_FROM` wiring; the recipient
picker and send button on the book detail page, with the status area
polling until terminal; retry.

Out of scope, each with its reason:

- **Send history view.** UI.md files it as "secondary view, lower
  priority than the two above", and it is a separate screen reading
  exactly the schema this step builds. Its absence is why no
  `ListSendLog` appears below — an uncalled list method is the very thing
  this step is reacting against.
- **A recipient management UI.** DESIGN.md: "Adding a new address is
  possible inline from the same control but is deliberately secondary …
  No separate management UI." Editing and deleting recipients are not
  designed and not built.
- **Automatic retries.** DESIGN.md asks for "a retry button", not a
  retry policy. A send that failed for a reason the user can see
  (attachment too large, address rejected) should not be retried behind
  their back, and this server always has its one human available. Retry
  is a user action that queues a *new* job — see "Retry is a new row."
- **Delivery/bounce webhooks.** Persisting the message id `Client.Send`
  already returns is the whole prerequisite for correlating one later.
  Note that `delivered` therefore means "Resend accepted the message",
  not "arrived on the device" — the state name is DESIGN.md's and is kept
  rather than renamed, so say so in the column comment.
- **Field provenance and inline editing**, unchanged from `#29`: still
  gated on provider design, still not this step.
- **Streaming the attachment body** — considered and rejected below.

## Schema: five migrations

House rules unchanged: one statement per file,
`YYYYMMDDNN_description.sql`, applied in filename order, each in its own
transaction. Master's newest is `2026090103` (#28's FTS trigger); these
start a new day, and migrations number independently of plans and backlog
files, so `2026090202`/`2026090203` already existing under
`docs/backlog/` is not a clash.

1. `2026090201_create_recipients_table.sql`

   ```sql
   CREATE TABLE recipients (
       id           INTEGER PRIMARY KEY,
       address      TEXT NOT NULL COLLATE NOCASE,
       label        TEXT NOT NULL DEFAULT '',
       last_used_at TEXT,
       added_at     TEXT NOT NULL
   );
   ```

   `COLLATE NOCASE` on the column rather than per-query, the same
   arrangement `books.sort_title` uses: it makes the unique index below
   case-insensitive for free, which is what stops `Mike@kindle.com` and
   `mike@kindle.com` becoming two entries in a two-entry list. (SMTP
   local parts are formally case-sensitive; no real mailbox relies on it,
   and Amazon's addresses certainly don't.) `last_used_at` is nullable —
   a freshly added address has never been used, and that is a different
   thing from having been used at time zero.

2. `2026090202_create_recipients_address_index.sql` —
   `CREATE UNIQUE INDEX recipients_address ON recipients(address);`

3. `2026090203_create_send_log_table.sql`

   ```sql
   CREATE TABLE send_log (
       id                  INTEGER PRIMARY KEY,
       book_id             INTEGER REFERENCES books(id) ON DELETE SET NULL,
       book_title          TEXT NOT NULL,
       recipient_address   TEXT NOT NULL,
       status              TEXT NOT NULL
                           CHECK (status IN ('queued','sending','delivered','failed')),
       provider_message_id TEXT NOT NULL DEFAULT '',
       failure_reason      TEXT NOT NULL DEFAULT '',
       queued_at           TEXT NOT NULL,
       started_at          TEXT,
       finished_at         TEXT
   );
   ```

   Three decisions worth stating, since each departs from a pattern used
   elsewhere in this schema:

   - **`recipient_address` is a string, not an FK.** Straight from
     DESIGN.md: "deleting a recipient never orphans or rewrites history."

   - **`book_id` is `ON DELETE SET NULL`, and `book_title` is
     denormalised beside it.** Every other FK in this schema cascades
     (`book_files.book_id`, both `book_authors` sides) because those rows
     are meaningless without their book. A send log entry is the
     opposite: it is the *record that a thing happened*, and the scanner
     deletes books routinely — orphan pruning when a path is reassigned,
     and `PruneMissingFiles` when a book's last location stays gone past
     `MISSING_GRACE`. Cascading would mean deleting a book erases the
     evidence it was ever sent, which defeats the history's stated
     purpose ("did I already put this on the Kindle?"). Same reasoning as
     the recipient address, applied to the other end of the row: the
     snapshot survives, the link is a convenience.

   - **`status` carries a `CHECK`.** The four states are DESIGN.md's and
     closed; a typo in a Go constant should fail at the write rather than
     produce a job no worker ever claims.

   Timestamps are the usual fixed-width UTC RFC 3339 text via
   `formatTime`; `started_at`/`finished_at` are nullable because a
   `queued` job has neither.

4. `2026090204_create_send_log_status_index.sql` —
   `CREATE INDEX send_log_status_queued_at ON send_log(status, queued_at);`
   The worker's claim query is `WHERE status = 'queued' ORDER BY
   queued_at LIMIT 1`; this is that query's index.

5. `2026090205_create_send_log_book_id_index.sql` —
   `CREATE INDEX send_log_book_id_queued_at ON send_log(book_id, queued_at);`
   Serves the detail page's "latest send for this book" lookup, and gives
   the `ON DELETE SET NULL` above an index to work against instead of a
   table scan per book deletion (the scanner deletes books in a loop).

## Storage

New file `internal/storage/sends.go` — `books.go` is already 500 lines
and none of this shares helpers with it beyond `formatTime`.

```go
type Recipient struct {
    ID         int64
    Address    string
    Label      string
    LastUsedAt sql.NullTime
    AddedAt    time.Time
}

type SendStatus string

const (
    SendQueued    SendStatus = "queued"
    SendSending   SendStatus = "sending"
    SendDelivered SendStatus = "delivered"
    SendFailed    SendStatus = "failed"
)

type Send struct {
    ID                int64
    BookID            sql.NullInt64
    BookTitle         string
    RecipientAddress  string
    Status            SendStatus
    ProviderMessageID string
    FailureReason     string
    QueuedAt          time.Time
    StartedAt         sql.NullTime
    FinishedAt        sql.NullTime
}
```

Methods, each one transaction, per the house rule:

- `ListRecipients(ctx) ([]Recipient, error)` — ordered
  `last_used_at DESC, address` so the most recently used address comes
  first and never-used ones trail in a stable order. No `NULLS LAST`
  clause and no `last_used_at IS NULL` leading term: SQLite treats NULL
  as smaller than any value, so `DESC` already sorts never-used addresses
  last (verified against the module's own `modernc.org/sqlite`). The
  picker's default is then "the first option", with no ordering logic in
  the transport.
- `CreateRecipient(ctx, address, label string, now time.Time) (int64, error)`
  — `INSERT … ON CONFLICT(address) DO NOTHING` followed by a select, so
  re-adding an address already saved returns the existing row rather than
  failing. Adding an address you already have is a user slip, not an
  error worth a page.
- `EnqueueSend(ctx, bookID int64, title, address string, now time.Time) (int64, error)`
  — inserts the `queued` row **and** sets `recipients.last_used_at = now`
  for that address, in one `DB.Write` via two package-internal `…Tx`
  helpers, per CLAUDE.md's composition rule. The bump belongs at enqueue
  because "most recently used" means "the one I last chose", and a send
  that fails should not send the picker back to the other address.
- `ClaimNextSend(ctx) (*Send, error)` — one transaction: select the
  oldest `queued` row, flip it to `sending` with `started_at`, return the
  claimed row; `nil, nil` when the queue is empty. Atomic even though a
  single worker makes contention impossible today — the claim is the one
  place a second worker would corrupt, and writing it correctly costs a
  `RETURNING`-less second statement inside a transaction we already have.
- `MarkSendDelivered(ctx, id int64, messageID string, at time.Time) error`
- `MarkSendFailed(ctx, id int64, reason string, at time.Time) error` —
  both set `finished_at`; both scope the `UPDATE` to
  `WHERE id = ? AND status = 'sending'`, so a terminal row can never be
  rewritten by a late worker.
- `FailInterruptedSends(ctx, reason string, at time.Time) (int, error)` —
  `UPDATE send_log SET status='failed', failure_reason=?, finished_at=?
  WHERE status='sending'`. Startup recovery; see below.
- `GetSend(ctx, id int64) (*Send, error)` — the status poll.
- `LatestSendForBook(ctx, bookID int64) (*Send, error)` — the detail
  page's initial render, `ORDER BY queued_at DESC, id DESC LIMIT 1`.

## The job model

New package `internal/sender`: the queue worker and the state machine.
Not in `internal/service` — service is the layer transports call, and
nothing calls a worker; not in `internal/resend`, which is deliberately
"a thin wrapper over the single POST /emails endpoint, not a general mail
abstraction."

```go
// Transport is what the worker needs from a mail provider. Declared here,
// on the consumer side, rather than in internal/resend — which notes it
// has no Sender interface "because nothing else implements one yet."
// *resend.Client satisfies this without changes.
type Transport interface {
    Send(ctx context.Context, to string, a resend.Attachment) (string, error)
}

type Worker struct {
    db         *storage.DB
    transport  Transport
    libraryDir string
    notify     chan struct{} // capacity 1
}

func New(db *storage.DB, t Transport, libraryDir string) *Worker
func (w *Worker) Notify()                       // non-blocking poke
func (w *Worker) Run(ctx context.Context)       // blocks until ctx is done
```

**Wake-up: a poke plus a periodic tick.** `Run` drains the queue, then
waits on `w.notify`, a `time.Ticker` (const `pollInterval = 1 * time.Minute`)
or `ctx.Done()`. The poke makes a send start the instant the button is
pressed; the ticker is the safety net that catches anything the poke
missed — a row left `queued` by a crash between insert and notify, most
obviously. This is deliberately the same shape as the scanner's
watcher-versus-periodic-rescan relationship, where DESIGN.md is explicit
that the fast path is an optimisation and the timer is the mechanism. The
channel has capacity 1 and `Notify` sends into it in a `select` with a
`default`, so a burst of enqueues coalesces and no handler ever blocks on
a busy worker. No env var: unlike `SCAN_INTERVAL`, there is no deployment
whose queue latency wants tuning.

**One job at a time, in queue order.** Concurrency buys nothing at a
handful of sends a week, and costs the memory bound that dissolves the
absorbed backlog item's peak-usage question: one send in flight means one
attachment resident.

**Processing one job:**

1. `ClaimNextSend`. Nil means the queue is empty — go back to waiting.
2. Resolve a file. `ListBookFiles(bookID)` (on master since #29), take the
   first location whose `missing_since` is NULL, join it onto
   `libraryDir`. No such location — the book was pruned, or every copy is
   marked missing — fails the job with "the file is no longer in the
   library". Resolution happens at *send* time, not enqueue time: a queue
   is a promise to act later, and the library moves underneath it.
3. `os.Stat` and compare against `resend.MaxAttachmentSize` **before
   reading**, failing with a human sentence that quotes both numbers
   ("14.2 MB exceeds the 28 MB limit"). The client's own check stays as
   the backstop it is; this one exists so an oversized file is never
   loaded into memory at all, and so the user gets the size in the
   failure reason.

   Format that sentence with a plain `fmt.Sprintf("%.1f MB", float64(n)/(1<<20))`
   rather than reaching for a general byte formatter. #29 added
   `humanSize`, but it is unexported in `internal/web` and belongs to
   that page's rail; a worker in `internal/sender` cannot call it, and
   copying it here would be the same duplication #29's review took out of
   the detail page. It would also be complexity for nothing: everything
   `humanSize` does is choose a unit, and both numbers in this sentence
   are megabytes by construction — the limit is 28MB, so a file that
   fails this check is between 28MB and whatever the filesystem holds,
   never bytes and never kilobytes. If a size ever needs rendering
   somewhere else, that is the point to give it a shared home, not now.
4. `os.ReadFile`, then `transport.Send` under a per-job
   `context.WithTimeout(ctx, sendTimeout)`.
5. `MarkSendDelivered` with the returned message id, or `MarkSendFailed`
   with the reason.

**Failure reasons are user-facing strings**, written for the person
reading the status box, and every one of them is produced here rather
than by stringifying a wrapped error into the database. A transport
failure records `err.Error()` truncated to a sane length (const
`maxFailureReason = 500`) — Resend's API errors are already sentences —
prefixed by nothing, since the box's label already says the send failed.

**Cancellation.** A job in flight when `ctx` is cancelled fails its
context and is left `sending` in the database, because the process is
going away and the send may or may not have reached Resend. That row is
recovered at next startup:

**Startup recovery: interrupted jobs fail, they do not requeue.**
`cmd/server` calls `FailInterruptedSends(ctx, "interrupted by a restart —
send again if it didn't arrive", now)` before the worker starts. A row
in `sending` means the process died between "handed the bytes to Resend"
and "recorded the answer", and *which side of the request it died on is
unknowable*. Requeueing risks a silent duplicate delivery; failing it
surfaces the ambiguity to the one person who can resolve it (by looking
at the Kindle) and leaves the retry a click away. Erring toward the
visible, reversible outcome is the same instinct the scanner's two-phase
missing-file handling encodes.

**Retry is a new row.** The retry button re-posts the same form, which
calls `EnqueueSend` again. The failed row stays failed. A history that
answers "did I already put this on the Kindle?" should show that the
first attempt failed and the second succeeded — mutating the row back to
`queued` would erase exactly the fact the log exists to record. It also
means there is no in-place transition out of a terminal state, which is
what lets `MarkSend*` guard on `status = 'sending'`.

## Resend client

Three changes, closing the absorbed backlog item:

- **Timeout.** `NewClient` builds its own `&http.Client{Timeout:
  sendTimeout}` instead of taking `http.DefaultClient`. `sendTimeout = 5
  * time.Minute`, a named constant carrying its arithmetic the way
  `MaxAttachmentSize` does: 28MB inflates to ~37MB of base64, which needs
  roughly four minutes on a 1.5 Mbit/s domestic uplink, so five minutes
  is the smallest round number that doesn't fail a legitimate large send
  on a slow line. Note in the comment that `http.Client.Timeout` covers
  the *whole* exchange including the upload — the backlog item's warning
  — so it must not be re-tuned as if it were a connect timeout. Dial and
  TLS-handshake bounds already come from `http.DefaultTransport`, which
  the client keeps. The worker's per-job context deadline uses the same
  constant and is the mechanism that makes a send abandonable at
  shutdown; the client's `Timeout` is the backstop for a caller that
  passes `context.Background()`.
- **`Text` is set.** `Send` gains a one-line body: `"Sent from the
  library."` This dissolves the backlog item's open question rather than
  answering it — Resend requires one of `text`/`html`/`react`, and
  whether an empty string counts as present is now moot, so the live-API
  experiment that item asked for is not needed. A body is also what a
  human sees if the message ever lands somewhere other than Amazon's
  converter.
- **Memory: deliberately unchanged.** The `io.Pipe` + `base64.NewEncoder`
  rework the backlog sketched is not done. With one worker there is one
  send in flight, so the ~100MB peak is a bound on the whole process
  rather than a per-request multiplier, and the pre-read stat check means
  the worst case only occurs for a file that will actually be sent. This
  is a decision to revisit on measurement, not an oversight — record it
  in the `Send` doc comment so the next reader doesn't rediscover it as a
  finding.

`internal/resend`'s package doc gains one line: it now has a caller.

## Service

`internal/service` grows the send surface the transport calls:

```go
type SendState struct {
    ID            int64
    Status        string
    Recipient     string
    FailureReason string
    At            time.Time   // finished_at when terminal, else queued_at
}

type RecipientOption struct{ Address, Label string }

func (s *Service) Recipients(ctx) ([]RecipientOption, error)
func (s *Service) QueueSend(ctx, bookID int64, address, newLabel string) (*SendState, error)
func (s *Service) SendState(ctx, sendID int64) (*SendState, error)
func (s *Service) LatestSend(ctx, bookID int64) (*SendState, error)
```

`QueueSend` is where the business rules live, per DESIGN.md's layering
note — the handler must stay "parse request, call service method,
render":

- Validate the address with `net/mail.ParseAddress` and store
  `addr.Address` (so a pasted `Mike <mike@kindle.com>` saves the mailbox,
  not the display name). An unparseable address is a
  `service.ErrInvalidAddress` the handler renders as a field error, not a
  500.
- `CreateRecipient` (idempotent, above) then `EnqueueSend`, taking the
  book's title from `GetBook` for the `book_title` snapshot; a book id
  that doesn't exist returns `nil, nil`, matching `GetBook`'s
  absent-is-not-an-error contract.
- The worker poke is *not* called from here — service does not know
  about the worker. `Service` gains an optional `Notify func()` set by
  `cmd/server` (nil in tests and when sending is unconfigured), invoked
  after a successful enqueue. A function field rather than an interface:
  it is one nullary call, and an interface would make `internal/service`
  depend on `internal/sender`, which depends on storage — a cycle waiting
  to happen.
- `SendState.At` collapses "when did this happen" to one field so the
  template has no branching; the shaping rule (finished if terminal, else
  queued) is service's, not the template's.

## Web transport

Two routes, both under the existing detail page:

- `POST /books/{id}/send` — form-encoded `recipient` (an address from the
  picker) or `new_address` + `new_label`. Renders the status fragment.
- `GET /books/{id}/sends/{sendID}` — the status fragment, for polling.
  Scoped under the book so a wrong pairing 404s rather than showing
  another book's send.

**Markup.** A new `send-control` template in `partials.html`, rendered
into the element `#29` left behind as `<div class="detail__send"></div>`.
Two small things that element needs first, both consequences of how #29
actually landed:

- **It has a class, not an id**, so there is nothing for `hx-target` to
  point at. Give it `id="send"` and keep the class for styling, matching
  how the grid is `#book-grid` with its own classes.
- **`bookDetailPage.ID` is gone.** #29's review removed it as assigned
  but never read; the send form's `action="/books/{id}/send"` is the
  first thing that needs it, so it comes back with a caller this time.

The whole control is one swap target: form and status are the same
region, because plate 06's states replace each other rather than
coexisting.

- *idle* — the picker (`<select name="recipient">` over
  `svc.Recipients()`, first option selected), a "Send to Kindle" button,
  and a `<details>` disclosure holding the `new_address`/`new_label`
  inputs. `<details>` is what `#29` already used for the location
  reveal, so it needs no new pattern and works with JS off. With zero
  saved recipients the disclosure renders open and the select is omitted
  — the first-run state, which is what a fresh install shows.
- *sending* — the status box with `hx-get` on the poll route and
  `hx-trigger="load delay:2s"`, `hx-swap="outerHTML"`. The fragment
  re-arms its own poll each time it comes back non-terminal, and the
  terminal fragments carry no `hx-*` at all, so polling stops by
  construction — nothing has to remember to cancel it.
- *delivered* — the confirmation, plus a plain "Send again" that renders
  the idle control back into the slot.
- *failed* — the reason, and a retry that is the idle form pre-selected
  to the same address. No new route: retry is the ordinary POST.

The form posts with `hx-post` and `hx-target="#send"`; without JS it is a
plain form POST, and the handler answers a non-`HX-Request` POST with
`303 See Other` back to `/books/{id}`, where the page's initial render
picks the job up from `LatestSend`. Progressive enhancement falls out of
the same handler, exactly as search's does.

`bookDetailPage` gains a `Send *service.SendState` field so a page loaded
while a send is in flight starts polling immediately, and one loaded
after a completed send shows its outcome rather than a bare button. The
service type goes in as it comes, not copied into a per-page struct:
#29's review removed exactly such a pass-through (`bookLocationView`,
which duplicated `service.FileLocation` field-for-field under the same
names), and `Locations` on that same view model is the precedent to
follow.

`POST /books/{id}/send` answers htmx with a fragment and everyone else
with a 303, so it names both headers in `Vary` per the rule above. The
poll route needs no `Vary` at all — it serves one body to every caller,
and a header that varies nothing is noise a later reader has to reason
about.

**When sending is unconfigured** (`RESEND_API_KEY` or `RESEND_FROM`
unset), `Routes` takes `sendEnabled bool`; the slot renders a single
subdued line — "Sending is not configured" — and the POST route returns
`503` with that same fragment. Registering the route and refusing is
better than omitting it: a stale open tab gets an explanation instead of
a 404, and `cmd/server` has already logged the reason at startup.

No CSRF token. DESIGN.md's authentication section is explicit that the
server is bound to a trusted network, and this step changes nothing about
that assumption — but it *is* the first state-changing POST in the app,
so note it in that section's status when DESIGN.md is next revised.

## cmd/server

- Read `RESEND_API_KEY` and `RESEND_FROM`. Both set → build the client
  and worker, run `FailInterruptedSends` once, start `worker.Run` on its
  own goroutine. Either missing → log once at Warn ("sending disabled:
  RESEND_FROM is not set") and pass `sendEnabled: false` into
  `web.Routes`. Browsing must work on a dev machine with no API key;
  refusing to start would make the library unbrowsable over a feature the
  user might not be using yet.
- The worker runs on `scanCtx` — the existing independently-cancellable
  child — and joins the same bounded shutdown wait. Generalise
  `waitForScan` into `waitForBackground(cancel, done, deadline, name)`
  taking the goroutine's name for the warning message, and call it for
  both; the ordering contract it documents (never close the database
  until this returns) is exactly as load-bearing for an in-flight send as
  for a sweep.
- `WriteTimeout` needs no change, and the comment in `main.go` that
  reasons about send-to-Kindle ("60s here is headroom, not a design
  constraint") becomes accurate rather than anticipatory: no handler
  reads a book off disk, because the worker does.

## Tests

`internal/storage` (`sends_test.go`):

- Enqueue then claim: status flips to `sending`, `started_at` set; a
  second claim on an empty queue returns `nil, nil`; claims come back in
  `queued_at` order.
- `MarkSendDelivered`/`MarkSendFailed` set `finished_at` and refuse a row
  that is not `sending` (assert the row is unchanged, not that an error
  is returned — the guard is a no-op `UPDATE` by design).
- The `CHECK` rejects an unknown status (drive it through a raw `Exec`,
  since the Go API can't produce one).
- Deleting a book through `PruneMissingFiles` leaves the send row with
  `book_id` NULL and `book_title` intact — the denormalisation is the
  point of the column, so it gets the test.
- `CreateRecipient` twice with differing case returns the same id and
  leaves one row; `ListRecipients` orders most-recently-used first with
  never-used last; `EnqueueSend` bumps `last_used_at`.
- `FailInterruptedSends` fails only `sending` rows and returns the count.

`internal/sender`: a stub `Transport` (no HTTP), a temp library dir, and
a real database.

- Happy path: queued → `delivered`, message id persisted, the stub saw
  the right address and the file's bytes and base name.
- Transport error → `failed`, reason recorded, the worker keeps running
  and claims the next job (one bad send must not wedge the queue).
- Oversized file → `failed` with both sizes in the reason, and the stub
  is never called.
- Missing file, and a book whose every location is marked missing → each
  fails with the file-gone reason.
- `Notify` while idle starts the job without waiting for the tick;
  cancelling mid-run returns from `Run` and leaves the claimed row
  `sending`, which `FailInterruptedSends` then resolves — the recovery
  contract, tested as one sequence.

`internal/service`: an invalid address returns `ErrInvalidAddress` and
queues nothing; a valid one creates the recipient and the job and calls
`Notify` exactly once; `QueueSend` on an unknown book returns `nil, nil`.

`internal/web`: POST enqueues and returns the sending fragment (asserting
it carries `hx-get`); the poll fragment for a terminal send carries no
`hx-` attribute (this is the "polling stops" contract, so it is asserted
directly); a mismatched book/send pair 404s; a non-`HX-Request` POST
303s; with `sendEnabled` false the slot renders the disabled line and
POST is 503; the failure reason is HTML-escaped (drive one through a
stubbed transport that returns `<script>`).

`internal/resend`: extend the existing `httptest` server assertion to
require a non-empty `text` — item 2 of the absorbed backlog is a test
gap as much as a bug, and this is the assertion whose absence let it
sit.

`cmd/server`: the worker joins the shutdown wait — assert
`waitForBackground` is called for it, in the shape the existing scan test
uses.

## CLAUDE.md

- New `internal/sender` bullet: the queue, single worker, poke-plus-tick
  wake-up, per-job file resolution at send time, and the two rules that
  are easy to undo by accident — interrupted jobs fail rather than
  requeue, and retry writes a new row.
- `internal/storage`: `recipients` and `send_log`, the `ON DELETE SET
  NULL` plus title snapshot and why it differs from every cascading FK
  around it, the claim-in-one-transaction contract, and the
  `status = 'sending'` guard on the terminal transitions.
- `internal/resend`: it has a caller; the client owns a timeout; `text`
  is non-empty; the memory decision.
- `internal/service` and `internal/web`: the send surface, the two
  routes, the self-terminating poll, and the unconfigured state.
- `cmd/server`: `RESEND_API_KEY`/`RESEND_FROM`, sending disabled when
  either is missing, the worker on `scanCtx` and the renamed
  `waitForBackground`.
- Remove send-to-Kindle from the still-missing list; the remaining
  entries are inline editing, provider enrichment, the watcher, the send
  history view, near-duplicate detection and conversion.
- Delete `docs/backlog/2026083117-resend-client-hardening.md` in this
  change.

## Verification

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...` clean.
- Manual, with a real `RESEND_API_KEY`/`RESEND_FROM` and the from-address
  on Amazon's Approved Personal Document Email List (DESIGN.md's
  requirement — the send fails silently *at Amazon*, not at Resend, if it
  isn't): open a book, add a Kindle address inline, send. The control
  swaps to sending, polls, and settles on delivered without a page
  reload; the book appears on the device. Reload mid-send and confirm the
  page resumes polling rather than showing an idle button.
- Failure path: send with a deliberately wrong from-address and confirm
  the failure reason shown is Resend's sentence, not a Go error string,
  and that retry produces a second row rather than reusing the first.
- Oversized file: point the library at a >28MB EPUB and confirm the
  failure names both sizes and that no request was made (Resend's
  dashboard shows nothing).
- Restart recovery: kill the process mid-send (`SIGKILL`, not `SIGTERM`),
  restart, and confirm the job shows failed-by-restart rather than
  sending again.
- With JS disabled: the form still posts, redirects, and the page shows
  the job's state.
- With `RESEND_API_KEY` unset: the library browses normally and the send
  slot explains itself.
