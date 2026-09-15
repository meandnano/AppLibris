# Honest rejection statuses under htmx 4

The htmx guidance for htmx 4 handles a validation error as an honest 422
that the form opts into swapping with `hx-status:422`. The web layer
instead answered every htmx rejection with 200 and blocked every error swap
through `noSwap`, so a rejection's status said the opposite of what
happened.

## How htmx 4 decides

`#handleStatusCodes` walks the exact status, then `NNx`, then `Nxx`, and at
each step consults `noSwap` before the element's `hx-status:<pattern>`. An
`hx-status:422` on a form therefore wins over `noSwap`'s `"4xx"`, so
`noSwap` can keep `[204,304,"4xx","5xx"]`: only a status a form names ever
swaps, and a plain-text 404 or 500 still never replaces a control. The
attribute's value is merged into the request after `HX-Reswap`, so the
swap style lives in the attribute.

## Steps

1. `metadataError`, `renderImportRejection` and `sendHandler`'s
   invalid-address branch answer 422 on the fragment path as well;
   `renderField` takes the status.
2. `RequireFetchMetadata` answers an htmx caller 403 with the partial and
   no `HX-Reswap`.
3. Every form whose route answers 422 carries
   `hx-status:422="swap:outerHTML"`: the four editors, the send form, the
   upload form and the confirm form. Every page template's `<body>`
   carries `hx-status:403:inherited="swap:afterbegin"`, since the refusal
   wrapper can answer any posting control and never knows which.
4. Tests assert the status and the attribute in each rejected response;
   notes and CLAUDE.md state the pairing.

## Out of scope

With JavaScript off, an invalid send address answers a 303 back to the
book and the message is lost. That predates this change and is not htmx's.

## Verification

`go test ./...`, `go vet ./...`, then in the running app: a blank title, an
invalid send address and a non-book upload each show their message; a
refused request over plain HTTP inserts the refusal above the control; a
stray 404 swaps nothing.
