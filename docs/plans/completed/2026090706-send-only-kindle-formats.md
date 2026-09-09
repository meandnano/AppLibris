# Step: do not offer to send a file the Kindle will drop

## Position in the sequence

Independent of the other send plans (`2026090707`, `2026090708`,
`2026090709`); all four touch `internal/sender` or the send control and
can land in any order.

## Context

Found in the 2026-09-07 review. The send control renders for every book
regardless of `Format`, and the worker sends the file under its on-disk
name: `filepath.Base(f.FilePath)`, so `Book.fb2` or `Book.fb2.zip`.
`MarkSendDelivered` fires as soon as Resend accepts the message.

Amazon's Send to Kindle by email accepts EPUB, PDF, DOC, DOCX, TXT, RTF,
HTM, HTML and a handful of image formats. It does not accept FB2, and it
does not accept a ZIP archive. Unsupported attachments are dropped
silently on Amazon's side, so the book detail page shows "Delivered" and
the history page agrees, for a book that never reaches the device.

DESIGN.md describes the library as EPUB with some FB2 and lists format
conversion as deferred. Until conversion exists, the honest surface is a
control that says why it is not offered.

## Scope

In scope: a per-book "sendable" decision in the service layer, and the
send control rendering an explanation instead of a button when the
answer is no.

Out of scope, with reasons:

- **Converting FB2 to EPUB.** DESIGN.md's deferred list; it needs a
  converter dependency and a storage story for the derived file. This
  step is the stopgap that decision's absence needs.
- **Refusing the POST for an FB2 book.** The button will not be rendered,
  and the handler still queueing a send for a hand-crafted request is
  harmless. A 4xx there is a second rule to keep in step with the first
  for no user-visible gain.
- **Deciding sendability at enqueue time in the worker.** The worker
  resolves the file at send time, but the format is a property of the
  book and known at render time, which is when the person needs it.

## Decision 1: sendability is a property of the format, decided in `internal/service`

`BookDetail` gains `Sendable bool` and `SendableNote string`, computed
from `Format` by one function with a table of the formats Amazon lists.
Today only `epub` is sendable, since those are the only two formats the
scanner indexes, but the function is written over the format string so a
future PDF or MOBI needs a table entry and nothing else.

It lives in the service rather than the transport because a future API
would answer the same question, and per DESIGN.md's layering a handler
stays "parse request, call service method, render."

## Decision 2: the control shows the reason, in the disabled treatment

When `Sendable` is false the send control renders the same visual state
it renders when Resend is unconfigured, with a different sentence:
"Kindle doesn't accept FB2 — convert to EPUB to send." The form is not
rendered at all, so there is no button to disable and nothing for a
poll to swap in.

A book that was sent before this change (an FB2 with a `send_log` row)
still shows its status box, because that is history and history is not
edited: it says "Delivered" for a send Resend accepted, which is what
that row records. The new sentence above it is what tells the reader
why it is not offered again.

## Changes

- `internal/service/service.go`: `Sendable`/`SendableNote` on
  `BookDetail`, filled by `sendableFormat(format string)`.
- `internal/web/book.go`: `bookDetailPage` carries both; `applySendState`
  is unchanged; `populateSendControl` copies them.
- `internal/web/templates/partials.html`: the `send-control` partial
  branches on `Sendable` before rendering the form, sharing the
  `send__disabled` treatment.
- The send POST and status fragment handlers pass the fields through so
  a fragment render agrees with the full page.

## Tests

- `internal/service`: `sendableFormat` table: `epub` yes, `fb2` no with
  the note, an unknown format no with a generic note.
- `internal/web`: an FB2 book's detail page renders no send form and
  renders the note; an EPUB's renders the form as before; the send
  fragment for an FB2 book (via the status poll route, for a historical
  send) renders the status box and the note and no form.

## CLAUDE.md

The `internal/web` send-control paragraph gains the sixth state and its
reason. The `internal/service` send paragraph gains `Sendable` beside
`Recipients`.

## Verification

- Open an FB2 book: no Send button, the note is there. Open an EPUB: the
  button is there. With JavaScript off the same two pages render the
  same way.
