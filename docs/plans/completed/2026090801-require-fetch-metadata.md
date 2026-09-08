# Step: make the HTTPS deployment requirement visible when it is violated

## Position in the sequence

Independent of every plan in the 2026-09-07 batch. It touches
`internal/web/web.go`, `cmd/server` and CLAUDE.md, and removes one backlog
item it makes moot.

## Context

The 2026-09-07 review's first finding. `sameSiteOnly` admits any request
whose `Sec-Fetch-Site` header is empty, and browsers send that header only
to a potentially trustworthy origin: HTTPS, or localhost. On a plain
`http://` deployment on a LAN or tailnet address the header is absent from
every request, cross-site ones included, so the guard admits every
cross-site POST. Any page in the person's browser could send a book to its
own address or edit metadata.

The deployment this project targets fronts the service with Tailscale
Serve, which terminates TLS and proxies to the plain listener. Behind it
the header arrives and the guard works. That holds only while the plain
listener is unreachable from any browser except through the proxy: a
container that also publishes port 8080 on the LAN has the requirement
half-met, which is the same as unmet.

So the requirement is documented in two parts, and the code makes a
violation show up rather than pass silently.

## Scope

In scope: the documented requirement; a wrapper that refuses a
state-changing request with no fetch metadata; an opt-out that logs one
tripwire line instead; removing the `Host`-header backlog item.

Out of scope, with reasons:

- **An `Origin`/`Referer` fallback check.** Behind a reverse proxy the
  `Host` the app sees may be the proxy's rewrite rather than the browser's,
  so comparing `Origin` against it needs `X-Forwarded-Host` handling and a
  trusted-proxy notion the app does not have. The requirement plus the
  fail-closed wrapper covers the same ground without that machinery.
- **A CSRF token.** It would work over plain HTTP, but it is a session
  mechanism in an app with no sessions, and the requirement is a one-line
  compose change.
- **Terminating TLS in the app.** Tailscale Serve, Caddy and every NAS
  reverse proxy already do it with certificate management; a second
  implementation is more to get wrong.

## Decision 1: a second wrapper around the whole handler, not a parameter on `Routes`

`Routes(svc, coversDir, sendEnabled, enrichEnabled)` has about ninety call
sites in the package's tests. A fifth positional parameter for one flag
touches all of them. Instead `web.RequireFetchMetadata(next http.Handler)`
is exported and `cmd/server` wraps it around `web.Routes(...)`. It refuses
any request whose method is not GET, HEAD or OPTIONS and whose
`Sec-Fetch-Site` is empty, with a 403, and passes everything else through
untouched.

`sameSiteOnly` is unchanged. It answers the question it can answer, "the
browser said cross-site", and keeps admitting the empty header, because
the opt-out mode below depends on that. The two compose: the strict mode
is `RequireFetchMetadata` outside, `sameSiteOnly` inside.

## Decision 2: fail closed by default

`REQUIRE_FETCH_METADATA` defaults to `true`, parsed with `ParseBool` like
`WATCH_ENABLED`, an unparseable value failing startup. Every browser
released since 2023 sends the header over HTTPS. The only clients a
fail-closed default turns away are a browser older than that and a
scripted client, and the app has no API for scripts. Against that, the
default is what makes a listener accidentally published on the LAN produce
a 403 and a Warn on the first edit, instead of running for months with the
guard silently inert.

Each refusal is logged at Warn, per request rather than once: a person
whose edit was refused needs the log to say why, and a flood of them is the
symptom of exactly the exposure being surfaced.

## Decision 3: the opt-out is a tripwire, not silence

`REQUIRE_FETCH_METADATA=false` swaps in `web.WarnMissingFetchMetadata`,
which admits everything and logs one Warn per process on the first
state-changing request with no fetch metadata. Once, because in this mode
the requests are admitted and a script posting routinely would write the
same line forever. `cmd/server` also logs a Warn at startup naming the
setting, so the choice is visible before any request arrives.

## Decision 4: the `Host`-header backlog item goes

`docs/backlog/2026090721-host-header-not-validated.md` recorded DNS
rebinding against the unchecked `Host`. Under the requirement, a rebound
hostname fails certificate validation against the HTTPS origin, and the
plain listener is not reachable from a browser at all. CLAUDE.md records
why there is no `Host` allowlist; the item is deleted rather than kept as
a note saying "do nothing".

## Changes

- `internal/web/web.go`: `RequireFetchMetadata`, `WarnMissingFetchMetadata`,
  `isStateChanging`, `hasFetchMetadata`; `sameSiteOnly`'s comment rewritten
  to say why it passes the empty header and who decides whether that is
  acceptable.
- `cmd/server/main.go`: parse `REQUIRE_FETCH_METADATA`; wrap the UI routes
  in one wrapper or the other; startup Warn on opt-out. `/healthz` is
  outside the wrapper.
- `CLAUDE.md`: the `internal/web` guard paragraph gains the requirement,
  the two wrappers and the `Host` note; the `cmd/server` paragraph gains
  the env var.
- `docs/backlog/2026090721-host-header-not-validated.md`: deleted.

## Tests

- `internal/web`: table over method and header for `RequireFetchMetadata`
  (POST without header refused and `next` not called; POST with
  `same-origin`, `none` and `cross-site` all passed on, the last because
  refusing it is `sameSiteOnly`'s job; GET, HEAD, OPTIONS without header
  passed on). Every refusal is logged. `WarnMissingFetchMetadata` admits a
  same-origin POST without logging, admits three metadata-less POSTs and
  logs exactly once.
- `internal/web`'s existing `TestSendHandlerAllowsSameOriginAndMetadataLessPosts`
  is unchanged: it tests `Routes` directly, where the empty header is still
  admitted by design.
- `cmd/server`: `REQUIRE_FETCH_METADATA=maybe` fails startup, in the
  existing bad-configuration table.

## Verification

- `go build ./... && go vet ./... && go test -race ./...` clean.
- Start the server. `curl -X POST localhost:8080/books/1/enrich` answers
  403 and the log carries a Warn naming the env var. `curl localhost:8080/`
  answers 200.
- `REQUIRE_FETCH_METADATA=false`: the same POST is admitted, one startup
  Warn, one request Warn on the first, none on the second.
- Through Tailscale Serve in a browser, an edit and a send both work
  unchanged.
