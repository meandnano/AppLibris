# Backlog: a refused redirect is classified retryable and retried three times

## Problem

Both provider clients set a `CheckRedirect` policy, and both misclassify
the error it produces.

`internal/openlibrary.checkRedirect` and `internal/googlebooks.checkRedirect`
refuse a hop that exceeds the bound, names a scheme other than http/https,
or — in the Google one — leaves the host the lookup started against, or
downgrades off TLS. The error surfaces through `httpClient.Do`, and on the
**lookup path** of each client every `Do` error is classified the same way:

```go
// internal/googlebooks.search
resp, err := c.httpClient.Do(req)
if err != nil {
    return enrich.Metadata{}, fmt.Errorf("…: %w: %w", enrich.ErrRetryable, c.redactKey(err))
}

// internal/openlibrary — the same shape, without a redactKey, since that
// client has no API key to scrub
resp, err := c.httpClient.Do(req)
if err != nil {
    return enrich.Metadata{}, fmt.Errorf("…: %w: %w", enrich.ErrRetryable, err)
}
```

So a refused redirect wraps `enrich.ErrRetryable`, and `enrich.WithRetry`
spends all `DefaultRetryAttempts` on it with doubling backoff.

`internal/googlebooks.volumeByID` is the exception and needs no change:
`enrichVolume` swallows its error at Debug and never returns it, so a
refused redirect there is already not retried. Every
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

- Whether both clients still wrap every `Do` error on their lookup path
  in `ErrRetryable` (`volumeByID` is the deliberate exception).
- Whether either API has started redirecting. If one has, this stops being
  theoretical and the retries become real traffic against a host that is
  already telling us to go somewhere else.

## Sketch

A sentinel is enough, and it has to be per-package because the two
`checkRedirect` functions are separate implementations — they differ in
what they refuse, not only in their error text:

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

Note `internal/openlibrary` has no `redactKey`, so its version of the
second snippet drops that call.

Do both packages in one change or neither. They are documented as shaped
identically, and fixing one would make that sentence the next thing to go
stale.

## The adjacent hole this item does not cover

`internal/googlebooks`' host check closes two things at once: the key leak
through `Referer`, and *adopting the answering host's whole response*. Only
the first is Google-specific — there is no key in `internal/openlibrary`,
so no `Referer` leak — but the second is equally open there:
`ByISBN`'s answer is ungated, and `Search`'s is gated only on title and
author, both of which a foreign body supplies.

A host check would probably be safe there too, but it needs checking
rather than assuming: that client genuinely relies on following redirects
(`openlibrary_test.go` covers an ISBN aliasing the canonical edition key),
and those hops are same-host as far as anyone has looked. "As far as
anyone has looked" is the part to settle before adding the check, which is
why it is recorded here rather than done.
