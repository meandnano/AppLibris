# Backlog: the server answers any `Host` header

## Problem

Nothing in `cmd/server` or `internal/web` checks the request's `Host`.
Combined with no authentication, that leaves DNS rebinding open: a page
on an attacker's site can point its own hostname at the server's private
address after the browser has loaded it, and then read responses from
the library as if it were same-origin, because to the browser it is.

What is readable that way is the whole browsing surface: the grid, every
book's metadata and file paths, the send history with its recipient
addresses. State-changing routes are separately guarded by
`sameSiteOnly`, and `Sec-Fetch-Site` on a rebound request is
`same-origin`, so that guard does not help here either; a `Host` check
is the standard defence for both.

## Why this is backlog, not a plan

Behind Tailscale the server's address is in the CGNAT range
(`100.64.0.0/10`), which Chrome's Private Network Access treats as
private and increasingly blocks from public pages. The attack needs the
person to be on the tailnet and visiting a hostile page at the same
time, and what it reads is a personal library's titles and one Kindle
address. Real, small, and behind two other controls.

## Re-validate before acting

- Whether `cmd/server` still mounts `web.Routes` with no host check.
- Whether the deployment has moved to `tailscale serve` or another TLS
  front, which changes what `Host` values are legitimate.

## Sketch

An `ALLOWED_HOSTS` environment variable (comma-separated, default empty
meaning "any", so a fresh install keeps working), enforced by a wrapper
around the mux that answers `421 Misdirected Request` for a `Host`
outside the list. Compare as a host, not a string: case-insensitive,
default port and explicit port alike, trailing dot ignored, the way
`internal/googlebooks.sameHost` already does for the opposite direction.
Log the rejected value at Warn once per distinct value so a
misconfigured list is diagnosable without flooding.
