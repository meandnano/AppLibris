# Backlog: a refused redirect is classified retryable and retried three times

## Problem

Both provider clients set a `CheckRedirect` policy, and both misclassify
the error it produces.

`internal/openlibrary.checkRedirect` and `internal/googlebooks.checkRedirect`
refuse a hop that exceeds the bound, names a scheme other than http/https,
or — in the Google one — leaves the host the lookup started against. The
error surfaces through `httpClient.Do`, and both clients treat *every*
`Do` error the same way:

```go
resp, err := c.httpClient.Do(req)
if err != nil {
    return enrich.Metadata{}, fmt.Errorf("…: %w: %w", enrich.ErrRetryable, c.redactKey(err))
}
```

So a refused redirect wraps `enrich.ErrRetryable`, and `enrich.WithRetry`
spends all `DefaultRetryAttempts` on it with doubling backoff. Every
attempt is refused at the same hop for the same reason: the policy is a
pure function of the URL, and the URL does not change between attempts.

This is the failure the four-case contract's own reasoning rules out
elsewhere. `internal/googlebooks` classifies a 400 and a 403 as
non-retryable precisely because "another attempt answers those
identically", and records that in a comment. A refused redirect answers
identically too.

## Why this is backlog, not a plan

The condition does not occur. Neither `www.googleapis.com` nor
`openlibrary.org` was observed to redirect at all during the live check
that produced `docs/plans/completed/2026090608-googlebooks-live-fidelity.md`,
let alone off-host or past five hops. If it started happening, the retries
would be wasted work against a background job that nothing waits on, paced
by `WithRateLimit` — not a correctness problem, and not one a person would
notice.

It also costs nothing today in the one place it might: `Resolve` skips a
provider that errors and continues the chain either way, so the outcome for
the book is the same whether the failure was retried once or three times.

## Re-validate before acting

- Whether both clients still wrap every `Do` error in `ErrRetryable`.
- Whether either API has started redirecting. If one has, this stops being
  theoretical and the retries become real traffic against a host that is
  already telling us to go somewhere else.

## Sketch

A sentinel is enough, and it has to be per-package because the two
`checkRedirect` functions are deliberately separate (their error text
reaches different failure paths, which is why `internal/openlibrary`'s
comment says it is not a call into `enrich.CheckCoverRedirect`):

```go
var errRedirectRefused = errors.New("redirect refused")
// …each return in checkRedirect wraps it…
return fmt.Errorf("%w: to %q, which leaves the host…", errRedirectRefused, req.URL.Host)
```

`http.Client.Do` wraps a `CheckRedirect` error in `*url.Error`, which has
an `Unwrap`, so `errors.Is(err, errRedirectRefused)` reaches it from the
call site. Then classify:

```go
if err != nil {
    if errors.Is(err, errRedirectRefused) {
        return enrich.Metadata{}, fmt.Errorf("…: %w", c.redactKey(err))
    }
    return enrich.Metadata{}, fmt.Errorf("…: %w: %w", enrich.ErrRetryable, c.redactKey(err))
}
```

Do both packages in one change or neither. They are documented as shaped
identically, and fixing one would make that sentence the next thing to go
stale.
