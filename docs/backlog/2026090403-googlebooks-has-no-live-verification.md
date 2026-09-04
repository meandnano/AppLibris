# Backlog: `internal/googlebooks` has never been checked against the live API

## Problem

Every claim `internal/googlebooks` makes about the Google Books Volumes
API is taken from its documentation. No request has ever reached the real
service, no fixture under `internal/googlebooks/testdata/` is a capture —
its own test-file comment says so, "shaped after the Google Books Volumes
API's documented response format … rather than a live capture" — and the
package's tests therefore assert that the parser agrees with its author's
reading of the docs.

That is exactly the position `internal/openlibrary` was in until
`docs/plans/completed/2026090401-openlibrary-field-fidelity.md`. One
afternoon of live payloads found two fields wrong there (a Bulgarian
language for an English book, a publication year 75 years off), two more
left empty when the data was available, and a no-match shape — a bare
`[]` — that would have been reported as a parse error. Every test was
green throughout. There is no reason to believe this package is better
verified than that one was; there is only the absence of anyone having
looked.

The live check `2026090305`'s Verification section asked for was run for
Open Library and **could not be run for Google Books**: both attempts
returned

```
429 Quota exceeded for quota metric 'Queries' and limit 'Queries per day'
    of service 'books.googleapis.com' for consumer 'project_number:…'
```

for the anonymous consumer project, no key having been configured. So the
one field-fidelity check the whole provider chain was supposed to get is
still owed on half of it, and is blocked on a credential
(`GOOGLE_BOOKS_API_KEY`) nobody has yet.

One concrete discrepancy is already visible without a key, and is the
strongest argument for doing this. `googlebooks.go` reasons:

```go
// A 403 is Google's over-quota and rejected-key answer as well as
// its forbidden one, and none of the three is helped by asking
// twice — only 429 and 5xx are.
if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
```

But the over-quota answer actually observed was a **429**, not a 403 — and
429 is on the retryable side of that branch, so `WithRetry` spends all
three `DefaultRetryAttempts` on a per-day quota that will not clear for
hours. Either the comment's premise is wrong, or Google uses both statuses
for different conditions (a per-day quota versus a per-minute one, say,
or an anonymous project versus a rejected key). Which it is decides
whether that classification is right, and it cannot be settled from the
documentation — that is what produced the wrong belief in the first place.

## Why this is backlog, not a plan

Nothing here is known to be broken. It is a verification task, and it is
blocked on an external credential rather than on any work in this repo, so
it cannot be scheduled — it becomes doable the day a key exists and not
before.

It does not corrupt data today: nothing in production enqueues an
enrichment job (step 06,
`docs/plans/2026090306-enrichment-in-the-ui.md`, is what makes enrichment
something a person asks for), so no provider answer reaches `books` yet.
It does not block step 06 either, since that step surfaces enrichment
rather than depending on any particular provider's field fidelity.

The honest reason it is not urgent is timing, not confidence: a wrong
value written by this provider would stick the same way Open Library's
would have — `Resolve` asks for a field only when it is empty and not
`manual`, so a filled field is never reconsidered — but there is a window
before step 06 in which to look, and the key is the gate.

Re-validate before acting: check whether a key has since been configured
and whether the 403/429 comment above still reads as written.

## Sketch

Run the check `2026090401` ran for Open Library, against the same two
book shapes that step's plan named — a book with a known ISBN, and a
sparse one with none so the title/author fallback is exercised.

What to look at, ordered by what went wrong last time:

- **Is `volumeInfo.publishedDate` the edition's?** Google returns one
  volume per edition, so this is probably fine — but "probably fine" is
  what was believed about `search.json`. Confirm against an ISBN whose
  edition year is known and differs from the work's first publication.
- **Is `volumeInfo.language` the edition's**, and is it really ISO 639-1
  as CLAUDE.md claims? Open Library's was neither the right value nor the
  claimed format.
- **`description` is documented as HTML-formatted** and `plainText`
  renders it. Check a real one: whether the tags are the ones the renderer
  handles, and whether anything arrives entity-escaped.
- **Does the `intitle:`/`inauthor:` quoting actually match the intended
  book?** The quoting is right per the docs (unquoted, the qualifier binds
  to its first token only), but that was verified by reading, not asking.
- **Confirm the no-match shape** is a 200 with `totalItems: 0` and no
  `items`, as `volumes_no_match.json` assumes. Open Library's real
  no-match was an array where the code expected an object.
- **`imageLinks`**: that the largest-first preference finds a real link and
  the `https` upgrade is needed rather than already done.
- **Settle 403 versus 429.** Try a valid key over its quota if that can be
  provoked, and a deliberately malformed key, and record the status and
  body of each. Then either fix the classification or correct the comment.
- **Confirm the key is sent, works, and stays out of every log line** —
  `redactKey`/`redactKeyBytes` are unit-tested, but only against errors
  this package constructs itself.

Then replace `volumes_match.json` and `volumes_no_match.json` with the
captures, and correct the test-file comment and CLAUDE.md's fixture
sentence the way `2026090401` did for Open Library — that sentence already
distinguishes captures from documented-shape approximations, so this is
one clause, not a rewrite.

Two notes on running it:

- Go's HTTPS requests did not complete in the environment `2026090401` was
  done in (DNS and TCP:443 succeeded; requests timed out awaiting
  headers), while `curl` to the same URLs worked. Fetching payloads with
  `curl` and replaying them through the real client against an
  `httptest.Server` exercises the parser faithfully and is what was done
  there; it does **not** exercise the client's own transport, so record
  which method was used.
- Any defect this turns up gets its own plan, not a fix folded into the
  verification — `2026090401` is the precedent, and its own scope note
  says so explicitly.

Related, and deliberately left where it is: the futility of retrying a
per-day quota at all is recorded in `2026090401`'s out-of-scope section,
since telling an exhausted daily quota from ordinary throttling means
reading `Retry-After` or parsing Google's error body and is a design
decision rather than a verification. If this check settles what Google
actually sends, that item becomes answerable.
