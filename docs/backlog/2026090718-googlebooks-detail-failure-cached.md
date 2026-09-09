# Backlog: a transient failure of Google's detail request is cached for the process lifetime

## Problem

`internal/googlebooks.enrichVolume` makes the second request
(`GET /volumes/{id}`) that upgrades a matched volume's cover from the
195px thumbnail and takes its fuller description. It fails silently by
design: any non-200, malformed body or transport failure leaves the list
answer's thumbnail and description in place and returns `err == nil`.

That is the right choice for the book in hand. But the answer then goes
into `enrich.WithCache`, keyed by ISBN or title, and stays for the
process lifetime (`DefaultCacheSize` entries, no TTL). A later run for
the same book, or another edition sharing the key, is served the degraded
answer without the detail request ever being retried. Pressing Fetch
again on the book does nothing different until the server restarts.

`WithCache` deliberately never caches an error because the four-case
contract treats one as transient. `enrichVolume` converts a transient
failure into a non-error, so it slips past that rule.

## Why this is backlog, not a plan

The degraded answer is still a correct answer with a smaller cover and a
flatter description, both hand-fixable, and the only trigger is a
person's button press. A restart clears it. It is a fidelity gap in a
background nicety, exactly the category `docs/backlog/` is for.

## Re-validate before acting

- Whether `enrichVolume` still swallows its error.
- Whether `WithCache` still has no TTL and caches every nil-error answer.

## Sketch

Two options, the first simpler:

- `enrichVolume` returns a sentinel (`errDetailUnavailable`) alongside
  the still-usable `Metadata`; `Search` and `ByISBN` return that
  `Metadata` with `nil` error as today but mark it (a `Partial bool` on
  `Metadata`, or an unexported wrapper the decorator can see), and
  `WithCache.put` skips a partial answer. The next lookup re-asks.
- A TTL on cache entries. Broader than this problem and changes the
  "shelf of obscure books re-asked every sweep" argument `WithCache`
  was built on; only worth it if a second reason for expiry appears.

Whichever lands, the resolver must not treat a partial answer as a
failed provider: it did answer, and `Failed == Asked` would otherwise
misreport the run.
