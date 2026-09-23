# Backlog: every outbound client carries its own copy of one policy

## Problem

Four outbound HTTP clients fetch URLs a third party chose: the Open Library
and Google Books providers, the enrichment worker's cover fetch, and the
importer's `Fetcher`. `internal/netguard` owns which addresses they may
dial, but the rest of their shared policy is copied per client:

- **The user agent.** The same literal is declared four times:
  `userAgent` in `internal/openlibrary/openlibrary.go` and
  `internal/googlebooks/googlebooks.go`, `coverUserAgent` in
  `internal/enrich/cover.go`, and `fetchUserAgent` in
  `internal/importer/fetch.go`. Changing it means finding all four.
- **The redirect policy.** `enrich.CheckCoverRedirect`,
  `enrich.CheckLookupRedirect` and `importer`'s `checkFetchRedirect` each
  re-check the scheme and bound the hop count, all at 5 but counted
  differently: the importer follows five hops and refuses the sixth, while
  both `enrich` policies count requests, as net/http does, and refuse the
  fifth. On top of that shared core the lookup policy keeps a lookup on
  its starting host, which guards Google's API key, and only the importer
  strips `Referer`, since a download link can carry a signed token and a
  public API's URL carries none.

Nothing is wrong today: each copy is correct for its fetch and tested.
The risk is drift, where a change to one client's policy, such as a new
agent string or a scheme rule, reaches three of the four.

## Sketch

Export one `UserAgent` constant and one scheme-and-hop-count check from
`internal/netguard` (or a sibling package if netguard should stay about
addresses), settle on one hop-counting convention, and let each client
wrap the check with its own extra rule: the lookup's host pinning, the
importer's `Referer` stripping. Every client and its redirect tests then
move over in one change, which is why it did not ride along with
importing from a link.
