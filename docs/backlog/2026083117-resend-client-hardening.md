# Backlog: Resend client hardening

## Problem

Three issues in `internal/resend`, all latent because nothing calls
`Client.Send` yet.

**1. No HTTP timeout.** `NewClient` uses `http.DefaultClient`, which has
none. A connection that opens and then stalls — a hung TLS handshake, a
proxy that accepts and never responds — blocks `Send` indefinitely. `Send`
takes a `ctx`, so a caller *can* bound it, but the default is unbounded and
the first caller will very likely not think about it. Under DESIGN.md's
queued-job model with a single worker, one hung send stalls every
subsequent send in the queue.

**2. The `text` field is always empty.** `sendRequest` marshals
`Text: ""` on every request, because nothing ever sets it:

```go
body, err := json.Marshal(sendRequest{
    From:    c.from,
    To:      []string{to},
    Subject: "Convert",
    Attachments: ...,   // Text is never assigned
})
```

Resend's API requires at least one of `text`, `html` or `react`. Whether an
empty string counts as present or absent **has not been verified against
the live API** — the package's tests use an `httptest.Server` that asserts
from/to/attachment and never looks at `text`, so they pass either way. If
Resend treats it as absent, every send fails with a 422 and the failure will
be read as a credentials or configuration problem rather than a body one.

The completed plan (`docs/plans/completed/2026083104-resend-mail.md`)
specified "a static subject ... and empty/minimal `text` body", so this is
the plan being followed literally rather than an implementation slip — but
"empty/minimal" was written without checking which of the two the API
accepts.

**3. Peak memory per send.** A 28MB attachment (the enforced maximum) exists
simultaneously as `a.Content`, a ~37MB base64 string, the marshalled JSON
body, and the `bytes.Reader` over it — on the order of 100MB resident for
one send, on a machine that is plausibly a Raspberry Pi or a small NAS.

## Why this is backlog, not a plan

Nothing calls `Send`. There is no job queue, no `recipients`/`send_log`
schema, and no `RESEND_API_KEY`/`RESEND_FROM` wiring in `cmd/server` — all
three are separate prerequisites DESIGN.md names, and none exists.

Fixing these in isolation means guessing at the caller. The timeout value
depends on the job model's retry policy; whether streaming the body is worth
it depends on whether sends run one-at-a-time in a worker or concurrently.
Better to settle them as part of the send-to-Kindle step, with the caller
in front of us.

The exception is item 2, which is worth **verifying** early even though the
fix waits — one real API call answers it, and knowing the answer changes
nothing about the design but removes a guaranteed false start.

## Sketch

- Give `NewClient` an `http.Client` with an explicit `Timeout` (30s is a
  reasonable starting point for a 28MB upload on a home connection — note
  that `http.Client.Timeout` covers the whole request including the body
  upload, so it must not be tuned as if it were a connect timeout).
- Either set a one-line `Text` body ("Sent from Bookshelf.") or add
  `omitempty` and set it — decide by testing the API. A body has the side
  benefit of being what appears if the message ever lands somewhere a human
  reads it.
- For memory: Resend's JSON API requires base64 in the body, so streaming
  means hand-building the JSON with an `io.Pipe` and a
  `base64.NewEncoder`, avoiding the intermediate string and byte slice.
  That is a real complication and should only happen if measurement shows
  it matters on the target hardware.

## Validate before planning

- **Make one real `Send` against Resend with an empty `text`** and record
  the response. This is the whole question for item 2 and takes minutes.
- Re-read the size arithmetic in `MaxAttachmentSize` against Resend's
  current published limit — the 40MB figure is from DESIGN.md and may have
  moved.
- Check whether the job model that lands wants `Send` to be cancellable
  mid-upload (it should be, via `ctx`), which affects whether
  `http.Client.Timeout` or a per-request context deadline is the right
  mechanism.
