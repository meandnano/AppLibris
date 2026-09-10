# Send to Kindle

Rationale for `internal/resend`, `internal/sender` and the send surface of
`internal/service`. The package map and the invariants that must hold are
in CLAUDE.md.

## Transport and size limit

The transport is Resend's API: one message, one attachment. Amazon requires
the sending address to be on the Kindle's Approved Personal Document Email
List, and Resend sends only from a verified domain, so `RESEND_FROM` and
the approved address must be configured to agree.

Resend is the binding size constraint, not Amazon. Amazon accepts personal
documents up to 200 MB; Resend caps the whole message, base64-encoded
attachment included, at 40 MB. Base64 inflates by 4/3, so 40 MB of message
holds about 30 MB of raw file, and `MaxAttachmentSize` is 28 MB to leave
headroom for headers and the JSON body. The worker checks the file's size
with a stat before reading it, so an oversized file is refused with both
numbers in the failure reason and is never loaded into memory.

The attachment is held unstreamed, as raw bytes, base64 text, the marshaled
body and a reader over it at once, about 3.3x the file at the ceiling.
That is acceptable because there is exactly one worker: the cost is a bound
on the whole process, not a per-request multiplier. Revisit on measurement.

`Client` sets no `http.Client.Timeout`. The deadline belongs to the caller's
context, because the smaller of a client timeout and the context wins, and
a fixed timeout here would silently cap the large send the worker's scaled
deadline exists to allow. `SendTimeout` (5 minutes) is exported as the
floor of that deadline, not the deadline itself.

`Send` sets a non-empty text body because Resend requires one of `text`,
`html` or `react`.

## The queue worker

`internal/sender` runs a single `Worker` over `send_log`, claiming rows one
at a time in queue order. One worker is deliberate: a handful of sends a
week gains nothing from concurrency, and one in-flight send is what keeps
the memory cost above bounded. The `Transport` interface lives on the
consumer side, in `internal/sender`; `*resend.Client` satisfies it
without an adapter.

`Run` wakes on a `Notify` poke or a once-a-minute `pollInterval` tick. The
poke is the optimisation, the tick is the mechanism: a row left `queued` by
a crash between insert and notify is picked up by the tick. The poke
channel has capacity 1 and is written non-blockingly, so a burst of
enqueues coalesces into one wake-up.

A book's file is resolved at send time, not enqueue time. A queue is a
promise to act later and the library moves underneath it, so the worker
takes the first `ListBookFiles` row whose `missing_since` is `NULL` when it
claims the job. Three sentences are kept apart because they mean different
things to the person reading the status box, decided by four conditions:

- No live location (pruned, or every copy missing) is "the file is no
  longer in the library".
- A storage error reading the index is "could not read the library index";
  it says nothing about whether the book is still there, and folding it
  into the first reason would be a confident lie about a file that is
  probably fine.
- A filesystem error other than `fs.ErrNotExist` (`EACCES`, `EIO`, `ESTALE`,
  a path component that is no longer a directory) is "could not read the
  file", with the OS error and path logged at Error, since the status box
  carries a sentence rather than an errno.
- An `fs.ErrNotExist` **under a top-level directory that no longer holds
  any books** is "could not read the file" too, logged at Warn with the
  path. A volume that unmounts and leaves its mountpoint as an empty
  directory fails every stat beneath it with `ENOENT`, which is the
  ordinary shape and the one a single errno gets wrong: "no longer in the
  library" would be written into `send_log` for every book on that disk and
  kept by the history page for a month, where "try again" is true of a disk
  that is coming back. A root-level file has no such directory and keeps
  the plain answer, the exception `reconcileMissing` makes for the same
  reason.

Only an `ErrNotExist` that survives that directory test means gone. The
test is `scanner.TopLevelDirHasBooks`, borrowed rather than restated here:
it is the same rule the sweep applies before refusing to prune, two copies
of a rule about the same directory drift, and the scanner's is the one with
tests. The test failing is itself an unknown and takes the same branch as a
directory that came back empty.

The transport call runs under a deadline `sendDeadline` computes from the
attachment: its base64 length over `minUplinkBytesPerSecond` (1 Mbit/s, a
slow domestic line) plus half of `SendTimeout` as slack, floored at
`SendTimeout`. A small file keeps five minutes; a 28 MB one gets about
seven and three-quarters instead of being cut off mid-upload. The floor is
a `Worker` field so tests can drive the timeout path in milliseconds.

A transport error's text is recorded verbatim as the failure reason,
truncated to `maxFailureReason` (500 bytes) on a UTF-8 boundary, because
Resend's API errors already read as sentences.

## Known and unknown outcomes

Two rules govern the job model, and both are about whether the outcome is
known.

**Interrupted jobs fail; they never requeue.** A job in flight when the
process stops is left `sending`, and `cmd/server` runs
`storage.FailInterruptedSends` at startup before the worker starts. Which
side of the request a dead process was on is unknowable, and requeueing
risks a silent duplicate delivery, while failing surfaces the ambiguity and
leaves retry one click away.

**A panic inside `process` is recovered** and recorded as `failed` with
`crashedReason`, the panic value and `debug.Stack()` going to the log at
Error, the same shape `internal/enrich` uses and for the same reasons
(`enrichment.md`, Job outcomes): the reason is a sentence for the status
box, since a panic value carries nothing a person can act on, and the
recovery is per job rather than in `Run`'s loop, where the send id is no
longer in hand. The blast radius here is smaller than enrichment's —
`FailInterruptedSends` would fail the row at the next start rather than
requeue it into a loop — but one restart of the whole server for one send
is still the server down, and the guard is the same few lines.

**Retry is a new row.** The retry button re-posts the form and calls
`EnqueueSend` again rather than flipping the failed row back to `queued`.
The log keeps the fact that the first attempt failed, which is what makes
it a history rather than a status field, and the storage guards
(`MarkSendDelivered`/`MarkSendFailed` update only a `sending` row) never
have to reason about a transition out of a terminal state.

The terminal writes follow the same line. A send Resend accepted, and a
failure decided locally (file gone, oversized, unreadable, index unreadable,
a rejection from the transport), are definite verdicts, so they are written
under `context.WithoutCancel` with a short `markTimeout`. A shutdown landing
in that gap must not cost a verdict already reached, or startup recovery
rewrites a delivered book as failed and invites a duplicate send. Only a
transport call abandoned mid-flight, whose error arrives with `ctx` already
cancelled, skips the write and leaves the row `sending` for recovery.

A send whose own deadline expired is the same unknown: the upload may have
completed with only the response outstanding. But no restart is coming to
resolve it, and leaving it `sending` would have the UI poll it until one
did, so it is written `failed`, the one terminal state that offers Retry,
with `timedOutReason` carrying the doubt the state cannot ("check the
Kindle before sending again"). It is detected with
`errors.Is(err, context.DeadlineExceeded)` after the `ctx.Err()` check,
because a parent cancellation also expires the child context; the resend
client wraps its error so that check keeps working. Resend's idempotency
keys would make the retry itself safe and are the right long-term answer.

## Recipients

`QueueSend` in `internal/service` owns the rules, so a future API answers
the same questions the UI does. It validates with `net/mail.ParseAddress`
and stores only `addr.Address`, so a pasted `Mike <mike@kindle.com>` saves
the mailbox and not the display name; a parse failure is `ErrInvalidAddress`
and queues nothing. It reads the book directly from storage rather than via
`GetBook`, since a title snapshot has no use for the author and location
joins, and returns `nil, nil` for an unknown id.

Saving a recipient is idempotent across case (`recipients.address` is
`COLLATE NOCASE` with a unique index) because re-adding a known address is a
slip, not a failure. `last_used_at` is bumped at enqueue, not at delivery:
"most recently used" means "the one I last chose", and a failed send must
not silently move the picker's default to a different address. Because
SQLite sorts `NULL` below every value, `ORDER BY last_used_at DESC, address`
puts never-used addresses last on its own, and the picker's default is
simply the first option.

`EnqueueSend` guards against a double submit: a `queued` or `sending` row
for the same `(book_id, recipient_address)` makes the call return that
row's id with `inserted = false`. Both pending states count, because a
`sending` row is a message already in flight and a second one queued
behind it is exactly the duplicate being prevented. The address is part of
the key because sending one book to two devices is legitimate.
`Service.Notify` is called only when a row was actually inserted; it is a
function field rather than an interface because `internal/service`
importing `internal/sender` would be a cycle.

`BookDetail.Sendable` and `SendableNote` come from `sendableFormat`, a
table of the formats Amazon lists that the scanner can index, today only
`epub`. Amazon drops an FB2 or ZIP attachment silently, so a send Resend
accepted would read "Delivered" for a book that never reached the device.
Until format conversion exists, the honest surface is a control that says
why it is not offered. `QueueSend` does not refuse an unsendable book: the
button is not rendered, and a hand-crafted POST queueing one is harmless,
where a 4xx would be a second rule to keep in step with the first.

There is no separate recipient management screen. A saved address is
removed from the picker itself (`RemoveRecipient`, over
`storage.DeleteRecipient`, which returns `false` rather than an error for
an unknown address). There is no edit: removing a wrong address and adding
the right one is the same number of actions and needs no second validation
path. Removal never touches `send_log`, which the schema guarantees rather
than the code remembering to: `send_log.recipient_address` is a plain
string, not a foreign key.

## History

The send log exists to answer "did I already put this on the Kindle?", so
it is built to survive everything around it. `send_log.book_id` is the
schema's one non-cascading foreign key (`ON DELETE SET NULL`) with
`book_title` denormalised beside it: the scanner deletes books routinely,
and a cascade would erase the evidence a book was ever sent. `status` is
`CHECK`-constrained to `queued`/`sending`/`delivered`/`failed`, so a typo in
a Go constant fails at the write rather than producing a job no worker
claims.

`SendState` collapses "when did this happen" to one `At` field
(`finished_at` once terminal, else `queued_at`), through `sendAt`, and the
history row (`SendRecord`) uses the same function, so the detail page and
the history table can never show a different instant for one send.

`SendHistory` returns the trailing 30 days (`sendHistoryWindow`), measured
from the service's own clock so it is testable without waiting, capped at
`SendHistoryLimit` (500 rows, exported because `internal/web` names the
number in the scope line). It reads `book_title` and `recipient_address`
straight off `send_log` rather than joining `books` or `recipients`; a join
would drop exactly the pruned-book and removed-recipient rows the
denormalisation exists to keep.

Truncation is reported, not hidden. Storage is asked for one row past the
limit, and getting `SendHistoryLimit+1` back proves the cap cut something,
with no second count query. The two wrong answers this page can give are
not symmetric: a false "yes, already sent" costs a moment's doubt, while a
false "no" causes the duplicate delivery the job model works to prevent. At
a handful of sends a week, 500 rows is roughly a decade, so the truncated
scope line should never appear; it exists so the page degrades into telling
the truth, and doubles as the signal that paging or filtering has become
worth building.
