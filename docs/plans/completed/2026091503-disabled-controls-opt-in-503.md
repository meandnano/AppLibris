# The disabled send and enrich controls opt their 503 in

## Context

`POST /books/{id}/send` with sending unconfigured, and
`POST /books/{id}/enrich` with no provider, answer 503 with the disabled
`send-control` / `enrich-control` fragment so a stale open tab gets an
explanation instead of a dead link. The `htmx-config` meta tag keeps `5xx`
in `noSwap`, and no element names 503, so htmx swaps nothing: the button
in a stale tab does nothing at all. Only a caller with JavaScript off, who
navigates to the 503 body, sees the sentence. The handler comments,
`cmd/server`'s, `docs/notes/web.md` and the enrich test's comment all
promise the explanation.

The status is honest and stays. What is missing is the other half of the
pairing the Web invariant states for 4xx: a route that answers an error
body pairs it with the matching `hx-status` on the form that posts.

The two 503s are the only 5xx in `internal/web` that carry a fragment
body; every 500 is a plain `http.Error`. `noSwap`'s `5xx` entry is doing
its job for those, so the fix belongs on the two forms and not in the meta
tag.

## How htmx 4 decides

`#handleStatusCodes` walks `"503"`, `"50x"`, `"5xx"` in that order and at
each step consults `noSwap` before the source element's
`hx-status:<pattern>`. With `noSwap` holding `"5xx"`, `hx-status:503` on
the posting form wins at the first step, and `hx-status:5xx` can never be
reached. The opt-in therefore names the exact status, the same way the
422 forms do.

## Steps

1. `internal/web/templates/partials.html`: `send__form` gains
   `hx-status:503="swap:outerHTML"` beside its `hx-status:422`;
   `enrich__form` gains `hx-status:503="swap:outerHTML"`. Both forms
   already target `#send` / `#enrich` with `outerHTML`, so the disabled
   paragraph replaces the whole control, as the 422 rejection does.
2. `internal/web/send.go` and `internal/web/enrich.go`: the 503 branch
   gains a comment saying the form carries `hx-status:503`, which is what
   lets htmx swap the disabled fragment past `noSwap`'s `5xx`, in the shape
   of the comment on the 422 branch in `send.go`. The two handler doc
   comments keep "a stale open tab gets an explanation" and name the
   attribute that makes it true.
3. Tests, in `internal/web/send_test.go` and `internal/web/enrich_test.go`:
   - the enabled `send__form` and `enrich__form`, as rendered on the book
     page with the feature on (`Routes(..., true, true)` or `enrichRoutes`)
     and in the re-rendered send fragment, carry
     `hx-status:503="swap:outerHTML"`; the send form still carries its
     `hx-status:422`. The opt-in lives on the form the stale tab holds,
     not in the 503 body, so it is asserted where the form renders.
   - `TestSendControlWhenDisabled` and
     `TestEnrichHandlerDisabledServesTheDisabledFragment` keep asserting
     503 and the explanation text.
   - `TestHTMXConfigContract` is unchanged: it pins the meta tag and the
     `<body>` 403 opt-in and runs with both features disabled.
4. `docs/notes/web.md`: the pairing list under the `htmx-config` paragraph
   gains a third bullet, that the disabled send and enrich controls answer
   503 and each form carries `hx-status:503="swap:outerHTML"`, and the
   "pair" sentence says an error body, 4xx or 5xx, rather than a 4xx body.
   Add the rule that the opt-in names the exact status because `noSwap`
   is consulted before the element at every wildcard step. The send and
   enrich paragraphs keep their 503 sentences.
5. `CLAUDE.md` Web invariant: "A route that gains a 4xx body gains the
   matching `hx-status`" becomes an error body, 4xx or 5xx, with the 503
   pair named beside the 422 and 403 ones, and the exact-status rule
   stated in one sentence. `.revmux/profile.md`'s Web bullet says the
   same in its own words.

Out of scope: `POST /recipients/remove` and the two status polls answer 200
with the disabled fragment when the feature is off and need no opt-in, and
the poll `<div>`s need no `hx-status` because the status routes never
answer 503.

## Verification

`go test ./...`, `go vet ./...`, `go test -race ./internal/web/...`. Then
in a browser: start with `RESEND_API_KEY` and `RESEND_FROM` set and
`METADATA_PROVIDERS=openlibrary`, open a book page, restart with both
unset and `METADATA_PROVIDERS=`, and press Send and Fetch metadata in the
stale tab: each control is replaced by its "not configured" sentence.
With JavaScript off the same presses land on the 503 page as before.
