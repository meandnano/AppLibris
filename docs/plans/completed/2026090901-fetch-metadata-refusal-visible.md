# Step: make a fetch-metadata refusal visible where the person is

## Context

Review of `2026090801-require-fetch-metadata` (PR 60) found that the
default mode's refusal, a bare 403, is invisible in the browser: every
mutation in the UI is an `hx-post`, and the vendored htmx 2.0.10 does not
swap a 4xx. That is the same fact that makes `metadataError` answer a
rejected fragment with 200, and CLAUDE.md names a silent no-op Save as
the worst failure the page has. On a deployment that violates the HTTPS
requirement, Save, Send and Fetch metadata did nothing visible, and the
only explanation was a Warn on the server's stderr.

The same review found the requirement documented for agents (CLAUDE.md)
but not operators (README, which still called a LAN-only deployment
supported), the wrapper choice in `cmd/server` untested in the direction
that matters, and a test helper that swaps the global logger with no
warning about `t.Parallel()`.

The plan being corrected is completed and therefore immutable; this is
the fix's own plan.

## Changes

- `RequireFetchMetadata` answers an htmx fragment request with a **200**
  carrying a one-line refusal (`fetch-metadata-refused` in
  `partials.html`) and `HX-Reswap: afterbegin`, so the line lands as the
  first child of whatever the posting form already targets and the
  control survives beneath it. Generic across every POST route because it
  leans on each form's own `hx-target` rather than knowing which control
  posted. Repeated presses insert repeated lines; the first press is the
  one that has to explain itself. Every other client keeps the 403, with
  a body that now says what to do about it.
- `.refused` in `app.css`, beside `.send__error`.
- `cmd/server`: the wrapper choice becomes `fetchMetadataGuard(require,
  next)` and a table test pins both directions, so a swapped branch fails
  a test instead of a checklist.
- `captureLog` in `internal/web`'s tests says it must not be used from a
  parallel test.
- README gains a "Deployment requirement: an HTTPS front" section with
  both halves of the requirement, the env var, and what a violation looks
  like; the `METADATA_PROVIDERS=` sentence no longer implies plain-HTTP
  LAN browsing is a supported mode.
- CLAUDE.md's fetch-metadata paragraph records the two refusal shapes and
  why.

## Verification

- `go build ./... && go vet ./... && go test -race ./...`, `gofmt -l .`
- `curl -X POST -H 'HX-Request: true' localhost:8080/books/1/enrich`
  answers 200 with `HX-Reswap: afterbegin` and the message; the same
  without `HX-Request` answers 403 with the sentence; GET is untouched.
