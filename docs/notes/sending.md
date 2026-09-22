# Send to Kindle

Rules for `internal/resend`, `internal/sender` and the send surface of
`internal/service`.

## Transport and size limit

- **The transport is Resend: one message, one attachment. `RESEND_FROM`
  must be verified at Resend and on the Kindle's Approved Personal
  Document Email List.** Each side accepts mail only from its own list, so
  the two must agree.
- **`MaxAttachmentSize` is 28 MB, set by Resend's 40 MB message cap, not
  Amazon's 200 MB. The worker stats the file before reading it and refuses
  an oversized one with both numbers.** Base64 inflates the file by 4/3
  and the JSON body needs headroom; the stat keeps a refused file out of
  memory.
- **`resend.Client` sets no `http.Client.Timeout`. The caller's context
  deadline is the only bound and `SendTimeout` is its floor, not the
  deadline.** The smaller of a client timeout and the context wins, so a
  fixed timeout would silently cap the large send the scaled deadline
  exists to allow.
- **`Send` sets a non-empty text body.** Resend requires one of `text`,
  `html` or `react`.

## The queue worker

- **`internal/sender` is a single `Worker` over `send_log`, one row at a
  time in queue order, and the attachment is held unstreamed, about 3.3x
  the file.** One in-flight send makes that cost a bound on the process
  rather than a per-request multiplier.
- **The `Transport` interface lives on the consumer side in
  `internal/sender`; `*resend.Client` satisfies it without an adapter.**
- **`Run` wakes on a `Notify` poke or the `pollInterval` tick. The poke
  channel has capacity 1 and is written non-blockingly.** The tick is the
  mechanism, picking up a row left `queued` by a crash between insert and
  notify; the poke only shortens the wait, so a burst may coalesce.
- **A book's file is resolved at send time: the first `ListBookFiles` row
  whose `missing_since` is `NULL`.** A queue is a promise to act later and
  the library moves underneath it.
- **Three failure sentences are never folded together: no live location is
  "the file is no longer in the library", a storage error reading the
  index is "could not read the library index", any other filesystem error
  is "could not read the file".** An index error says nothing about
  whether the book is there, and the first sentence would be a confident
  lie about a file that is probably fine.
- **Only an `fs.ErrNotExist` under a top-level directory that still holds
  books, per `scanner.TopLevelDirHasBooks`, means gone. Under an emptied
  directory it is "could not read the file" at Warn; a root-level file
  keeps the plain answer, as `reconcileMissing` does.** An unmounted
  volume leaves an empty mountpoint whose every stat fails with `ENOENT`,
  and "gone" would be written into `send_log` for every book on a disk
  that is coming back.
- **The directory test is borrowed from the scanner, never restated, and
  its own failure takes the unreadable branch.** Two copies of a rule
  about one directory drift, and the scanner's has the tests; the test
  failing is itself an unknown, and an unknown is never evidence of gone.
- **A filesystem error other than `fs.ErrNotExist` logs the OS error and
  path at Error.** The status box carries a sentence, not an errno.
- **The transport call runs under `sendDeadline`: base64 length over
  `minUplinkBytesPerSecond` plus half of `SendTimeout`, floored at
  `SendTimeout`. The floor is a `Worker` field.** A 28 MB file on a slow
  line outlasts five minutes; the field lets a test drive the timeout in
  milliseconds.
- **A transport error's text is recorded verbatim, cut at
  `maxFailureReason` (500 bytes) on a UTF-8 boundary.** Resend's API errors
  already read as sentences.

## Known and unknown outcomes

- **Interrupted sends fail and never requeue: `cmd/server` runs
  `storage.FailInterruptedSends` before the worker starts.** Which side of
  the request a dead process was on is unknowable; requeueing risks a
  silent duplicate, failing leaves retry one click away.
- **`process` recovers a panic and writes the row `failed` with
  `crashedReason`; the panic value and stack go to the log.** The recover is
  per job, in `process` and not in `Run`'s loop, because that is where the
  send id is in hand; the panic value carries nothing a person can act on. Same shape as `docs/notes/enrichment.md`.
- **Retry is a new `send_log` row through `EnqueueSend`, never a mutation
  of a failed one, and `MarkSendDelivered`/`MarkSendFailed` update only a
  `sending` row.** The log keeps the failed attempt, which is what makes it
  a history, and no write reasons about leaving a terminal state.
- **Definite verdicts, delivered or a failure decided locally, are written
  under `context.WithoutCancel` with `markTimeout`. Only a transport call
  abandoned with `ctx` already cancelled leaves the row `sending`.** A
  shutdown in that gap must not cost a verdict already reached, or startup
  recovery rewrites a delivered book as failed and invites a duplicate.
- **A send whose own deadline expired is written `failed` with
  `timedOutReason`, detected by `errors.Is(err, context.DeadlineExceeded)`
  after the `ctx.Err()` check; the resend client wraps its error so the
  check works.** No restart is coming to resolve that unknown, so a row
  left `sending` would poll forever; a parent cancellation also expires
  the child context, so only the order tells the two apart.

## Recipients

- **`QueueSend` in `internal/service` owns validation and queueing.** A
  future API answers the same questions the UI does.
- **The address is validated with `net/mail.ParseAddress` and only
  `addr.Address` stored; a parse failure is `ErrInvalidAddress` and queues
  nothing.** A pasted `Mike <mike@kindle.com>` saves the mailbox, not the
  display name.
- **`QueueSend` reads the book from storage rather than `GetBook`, and
  returns `nil, nil` for an unknown id.** A title snapshot has no use for
  the author and location joins.
- **Saving a recipient is idempotent across case: `recipients.address` is
  `COLLATE NOCASE` with a unique index.** Re-adding a known address is a
  slip, not a failure.
- **`last_used_at` is bumped at enqueue, not delivery.** A failed send
  must not move the picker's default to another address.
- **Recipients are ordered `last_used_at DESC, address`; the default is
  the first option.** SQLite sorts `NULL` last, so never-used addresses
  need no extra clause.
- **`EnqueueSend` dedups against `queued` and `sending` for the same
  `(book_id, recipient_address)`, returning the existing id with
  `inserted = false`.** A `sending` row is already in flight, so a second
  behind it is the duplicate being prevented; the address is in the key
  because one book to two devices is legitimate.
- **`Service.Notify` fires only when `EnqueueSend` reports `inserted`, and
  is a function field, not an interface.** `internal/service` importing
  `internal/sender` would be a cycle.
- **Only formats Amazon accepts are offered: `BookDetail.Sendable` and
  `SendableNote` derive from `sendableFormat`, today `epub`. `QueueSend`
  does not refuse the rest; the button is not rendered.** Amazon drops FB2
  silently, so an accepted send would read "Delivered" for a book that
  never arrived; a refusal would be a second rule to keep in step with the
  first.
- **There is no recipient screen and no edit: `RemoveRecipient` over
  `storage.DeleteRecipient`, which returns `false` for an unknown address,
  removes from the picker and never touches `send_log`.** Remove and
  re-add costs the same as an edit without a second validation path, and
  `send_log.recipient_address` is a plain string, not a foreign key.

## History

- **`send_log.book_id` is `ON DELETE SET NULL` with `book_title`
  denormalised beside it, and history reads `book_title` and
  `recipient_address` off `send_log` without joining `books` or
  `recipients`.** The scanner deletes books routinely; a cascade or a join
  would drop exactly the rows the log exists to keep.
- **`status` is `CHECK`-constrained to the four states.** A typo in a Go
  constant fails at the write rather than producing a job no worker
  claims.
- **`SendState` and `SendRecord` both take their instant from `sendAt`:
  `finished_at` once terminal, else `queued_at`.** The detail page and the
  history table can never show two instants for one send.
- **`SendHistory` returns the trailing `sendHistoryWindow` (30 days) from
  the service's own clock, capped at `SendHistoryLimit` (500).** The
  clock makes it testable without waiting; the limit is exported because
  `internal/web` names it in the scope line.
- **Truncation is reported: storage is asked for one row past the limit,
  and `SendHistoryLimit+1` back means the cap cut something; there is no
  second count query.** A false
  "not sent" causes the duplicate the job model prevents, so the page must
  say when it is incomplete; the line appearing is the signal that paging
  is worth building.
