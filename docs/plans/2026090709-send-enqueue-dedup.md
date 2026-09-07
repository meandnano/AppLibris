# Step: a double submit queues one send

## Position in the sequence

Independent of the other send plans. `internal/storage/sends.go` and
`internal/service`.

## Context

Found in the 2026-09-07 review. `EnqueueSend` is a plain `INSERT`:

```go
INSERT INTO send_log (book_id, book_title, recipient_address, status, queued_at)
VALUES (?, ?, ?, ?, ?)
```

`EnqueueEnrichment` for the same shape of job is `INSERT … WHERE NOT
EXISTS (… status = 'queued')`, so pressing Fetch twice makes one promise.
Sending, the one queue whose side effect cannot be undone, has no such
guard. Two POSTs from a double click with JavaScript off (a path this
codebase deliberately supports), or from two open tabs, produce two
`queued` rows and two emails to the Kindle.

The htmx path makes it less likely, since the button is disabled while
the control shows Sending, but the swap that disables it arrives after
the response, and a second click before that lands is a second POST.

## Scope

In scope: `EnqueueSend` returning the existing pending row instead of
inserting a second, when one is `queued` or `sending` for the same book
and address.

Out of scope, with reasons:

- **Dedup across addresses.** Sending one book to two devices is a
  legitimate action, and the picker exists for it.
- **Dedup against a delivered row.** "Send again" is an explicit
  affordance; the guard is against accidental repeats within one pending
  window, not against sending twice on purpose.
- **A client-side disable on first click.** It helps the htmx path and
  does nothing for the no-JS one, which is the path that needs it most.

## Decision 1: the guard is `queued` or `sending`, keyed on `(book_id, recipient_address)`

The enrichment guard covers only `queued`, on the reasoning that a
`running` job does not block a fresh promise once it finishes. A send is
different: a `sending` row is a message in flight, and a second one
queued behind it is exactly the duplicate this guards against. So both
pending states block.

The address is part of the key because two devices is one book sent twice
on purpose. `recipient_address` is `COLLATE NOCASE` on `recipients` but a
plain string on `send_log`, so the comparison here uses the address as
`QueueSend` already normalised it (`parsed.Address`), which is what both
rows would hold.

## Decision 2: `EnqueueSend` reports whether it inserted, and `QueueSend` returns the existing state either way

`EnqueueSend` becomes one statement, `INSERT … SELECT … WHERE NOT EXISTS`,
followed by a lookup of the pending row's id for that key when nothing
was inserted. The `last_used_at` bump happens in both cases: the person
did choose that address just now.

`QueueSend` calls `Notify` only when a row was inserted, since a poke for
a row the worker already holds is harmless but pointless, and returns
`GetSend` of whichever id came back. The handler renders it exactly as it
renders a fresh send, so the second click's response is the same Sending
box the first one produced.

## Changes

- `internal/storage/sends.go`: `EnqueueSend` returns `(id int64,
  inserted bool, err error)`; the insert carries the `NOT EXISTS` guard;
  the pending row is looked up when it does not insert.
- `internal/service/service.go`: `QueueSend` threads `inserted` to the
  `Notify` call.
- A migration adding an index on `send_log (book_id, recipient_address,
  status)` if the existing `send_log_status_index` does not already
  serve the guard's lookup; check the query plan first.

## Tests

`internal/storage`:

- Two `EnqueueSend` calls for the same book and address: one row, the
  second returns the first's id with `inserted == false`, and
  `last_used_at` moved on the second call.
- Same book, different address: two rows.
- A `sending` row blocks a second insert; a `delivered` or `failed` one
  does not.

`internal/service`:

- `Notify` is called once across two `QueueSend` calls for the same
  pending send.

`internal/web`:

- Two POSTs in succession render the same send id in the poll URL.

## CLAUDE.md

The `internal/storage` sends paragraph gains the guard and its key, with
the contrast to `EnqueueEnrichment` (both pending states, not one, and
why). The `internal/service` `QueueSend` bullets gain the `Notify`
condition.

## Verification

- Disable JavaScript, open a book, double-click Send: the history page
  shows one row.
