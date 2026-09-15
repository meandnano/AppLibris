# Step: a rejected send address survives the no-JavaScript path

## Context

`POST /books/{id}/send` validates the address in `service.QueueSend`, which
answers `ErrInvalidAddress` and queues nothing for a value
`net/mail.ParseAddress` refuses. The htmx caller gets the send control back
at 422 with `SendError`, `SendNewAddress` and `SendNewLabel` set, swapped
in through the form's `hx-status:422`. The plain form
POST does not: `sendHandler` checks `isHTMXFragment` before it looks at the
error and answers every non-fragment request with a 303 to the book. A
person without JavaScript who mistypes an address lands back on the page
with no message, the typed address and label gone, and nothing queued, so
the send appears to have silently done nothing. `2026091501-htmx-4.md`
left this out of scope as not htmx's; this step closes it.

The metadata editors already solve the same problem: `metadataError`
renders the whole `book.html` at 422 through `renderStatus` with the
rejected field open. The send control follows that.

## Decisions

1. **Plain POST, rejected address: the whole page at 422.** `sendHandler`
   redirects only when the request is not a fragment *and* the address was
   accepted. A rejected plain POST loads the book (404 if unknown, as the
   fragment path does), builds the page through `makeBookDetailPage` and
   renders `book.html` through `renderStatus` at 422, the status the
   fragment path and `metadataError` already answer, and a browser renders
   a 4xx body on a navigation like any other.
2. **The page keeps the send result already on screen.**
   `makeBookDetailPage` calls `populateSendControl`, which reads
   `LatestSend`, so a Delivered or Failed status stays beside the error
   without a second read.
3. **One setter for the rejection.** `setPageSendError` writes the message
   and the two typed values for both shapes, so the fragment and the page
   cannot word the refusal differently or carry back different fields.
4. **`sendHandler` takes `enrichEnabled`.** The whole page renders the
   enrichment control too, and `Routes` already has the flag.
5. **Successful plain POST stays a 303.** Unchanged, and
   `TestSendHandlerNonHXRequestRedirects303` keeps pinning it.

## Tests

`TestSendHandlerNonHXInvalidAddressRendersWholePage422` in
`internal/web/send_test.go`: a book with a delivered send, a plain POST with
`new_address=not-an-address` and `new_label=Spare Kindle`. Asserts 422, no
`Location`, the whole page (`<html` and the detail `<main>`), `send__error`,
both typed values carried into their inputs, `Delivered` still shown, and
`send_log` still holding only the one row.

## Documentation

`docs/notes/web.md`: the send control section says how a rejected address
answers on each path. CLAUDE.md's invariant that a rejection answers 422 on
both paths already names the send address, and now holds for it.
