# Backlog: the cover fetch will connect to any address a provider names

## Problem

`internal/enrich.FetchCover` checks only the URL's scheme, and
`CheckCoverRedirect` checks the hop count and the scheme of each hop:

```go
if !coverSchemeAllowed(parsed.Scheme) {
	return nil, fmt.Errorf("cover url scheme %q is not http or https", ...)
}
```

Neither looks at where the host resolves. Confirmed with a scratch
server: `FetchCover` fetched `http://127.0.0.1:<port>/direct` and followed
a 302 from one host to `http://127.0.0.1:<port>/admin`, both bodies
returned.

The URL comes from a provider's response (`imageLinks`, `cover_i`), and
the redirect targets come from whichever host answered. A hostile or
compromised answer, or a hijacked `covers.openlibrary.org` redirect,
makes the NAS issue a GET to `169.254.169.254`, a router admin page, or
another container on the Docker network, authenticated by network
position alone.

## Why this is backlog, not a plan

The request is blind. Only bytes that decode as an image are persisted,
and then only as a cover thumbnail. No response body reaches a log, the
database or the UI as text, so there is no read primitive, and a GET
with no body has little write capability against anything reasonable.
The URL source is over TLS from two well-known hosts. The realistic
exposure is a home network with a device that acts on unauthenticated
GETs, which exists but is not this project's threat model.

It is recorded because the redirect check reads as if it guards hops and
it guards only their scheme, and because the fix is small and standard.

## Re-validate before acting

- Whether `FetchCover` and `CheckCoverRedirect` still check only the
  scheme.
- Whether the worker's `coverClient` still uses the default transport.

## Sketch

A `Transport` on the worker's `coverClient` whose `DialContext` resolves
the host and refuses loopback, RFC 1918, link-local (v4 and v6),
multicast and unspecified addresses at connect time. Checking at dial
covers every redirect hop and DNS rebinding, which a check on the URL
string cannot. `net.IP.IsPrivate`, `IsLoopback`, `IsLinkLocalUnicast`
and `IsLinkLocalMulticast` are the whole test.

Do not use a host allowlist instead: `covers.openlibrary.org` legitimately
redirects to archive.org backends, and Google's `books.google.com` image
hosts vary.

Refused connections should surface as a fetch failure, which the worker
already tolerates, logged at Debug with the resolved address so a
misconfigured local mirror is diagnosable.
