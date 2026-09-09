# Step: a search query cannot pin the read pool

## Position in the sequence

Independent of every other plan. `internal/storage/ftsquery.go` and one
handler in `internal/web`.

## Context

Found in the 2026-09-07 review and reproduced. `SanitizeFTSQuery` turns
every whitespace-separated token of the query into a quoted prefix term
and joins them all:

```go
fields := strings.Fields(input)
...
terms[i] = `"` + escaped + `"*`
return strings.Join(terms, " ")
```

Nothing bounds the token count or the total length. `libraryHandler`
passes `params.Get("q")` straight in, and `http.Server` keeps Go's default
1 MB `MaxHeaderBytes`, so a query string of several hundred kilobytes
arrives intact. A `q` of 100,000 single-letter tokens against a 3,000-book
table was still executing inside `CountSearchBooks` when the test binary's
ten-minute timeout fired.

Each search request runs the expression three times over: `SearchBooks`,
`CountSearchBooks`, and `MatchedSearchFields`'s four `EXISTS`. The read
pool holds eight connections. Eight such requests, and every page render
in the application blocks waiting for a connection. `WriteTimeout` does
not cancel `r.Context()`, so the queries run until the client disconnects
or SQLite finishes.

Behind Tailscale only a tailnet user can send the request, so this is a
robustness defect rather than an exposure. It still turns one mistaken
paste into an unresponsive server, and it is the only unbounded input
the search box accepts.

## Scope

In scope: a token count and byte cap in `SanitizeFTSQuery`; a matching
clip in the handler so the URL that is pushed to history is also bounded.

Out of scope, with reasons:

- **A per-request statement timeout via `context.WithTimeout` in the
  handler.** Worth doing generally but a different change: it bounds
  every query, not this input, and the FTS expression would still be
  built and parsed. It also needs deciding for the whole transport, not
  in a search fix.
- **Lowering `MaxHeaderBytes`.** It bounds the whole request line and
  headers together; cutting it far enough to matter here breaks cookies
  and long legitimate URLs before it protects the search.
- **Rate limiting.** No login means no principal to limit by, and a
  tailnet is a trusted network.

## Decision 1: the cap lives in `SanitizeFTSQuery`, where raw input becomes a `MATCH` expression

CLAUDE.md names that function as "the one place raw user input becomes
one." The cap belongs at the same choke point so a future caller (the API
DESIGN.md defers) is bounded by construction rather than by remembering
to clip first.

Two limits, both named constants with their reasoning beside them:

- `maxSearchTerms = 16`. A person types a title fragment and an author,
  which is under ten tokens; sixteen is generous and still small enough
  that the expression is trivially cheap.
- `maxSearchBytes = 256`. Applied to the input after control characters
  are stripped and before `Fields`, cut on a rune boundary. A title plus
  an author plus an ISBN fits several times over.

Tokens past the sixteenth are dropped, not an error: a search box has no
place to show one, and "the first sixteen words were searched" is a
result rather than a refusal. The ISBN shapes are unaffected: both accept
far less than 256 bytes.

## Decision 2: the handler clips `q` before pushing it anywhere

`libraryHandler` renders `Query` back into the input's `value`, the
no-results heading and the paging URLs. Clipping there, to the same
`maxSearchBytes`, keeps every rendered copy consistent with what was
actually searched, and stops a multi-kilobyte string round-tripping
through `hx-push-url` into browser history on every keystroke.

The constant is exported from `storage` for this reason, the same way
`service.MaxDescriptionBytes` is exported so `internal/web` sizes its
body cap from it rather than restating the number.

## Changes

- `internal/storage/ftsquery.go`: `MaxSearchBytes` and `maxSearchTerms`;
  `SanitizeFTSQuery` cuts the input and drops excess tokens.
- `internal/web/web.go`: `libraryHandler` clips `query` to
  `storage.MaxSearchBytes` on a rune boundary before use.

## Tests

`internal/storage`:

- Seventeen tokens sanitise to sixteen terms; the seventeenth is absent.
- Input over `MaxSearchBytes` is cut on a rune boundary (a multibyte
  character straddling the cut is dropped whole, never split into an
  invalid sequence that FTS5 would choke on).
- A complete ISBN with surrounding whitespace still takes the ISBN path
  after the cut, since it is far under the cap.
- A search of 100,000 tokens against a small fixture library completes in
  well under a second. This is the regression test for the reproduction
  and should carry a short deadline.

**Found while implementing:** that last test does not exercise
`maxSearchTerms` at all, so it is not on its own a regression test for
both caps. `MaxSearchBytes` is applied first, and 256 bytes admits at most
128 single-letter tokens — so with the byte cap in place and the term cap
lifted to a million, the 100,000-token search still completed in 0.25s
and the test passed. It fails (17s, deadline exceeded) only with both caps
lifted. Measured, not reasoned: the term cap earns its place because 128
prefix terms took 0.25s for one request's three queries against a
200-book fixture, which a library fifteen times that size no longer
answers promptly. Recorded in CLAUDE.md as the reason neither cap
subsumes the other.

`internal/web`:

- A `q` over the cap renders the clipped value in the input and in the
  paging URLs, not the original.

## CLAUDE.md

`internal/storage`'s `SanitizeFTSQuery` paragraph gains the two caps and
the reason they live there. `internal/web`'s search paragraph notes the
handler clips to the exported constant.

## Verification

- Paste a very long string into the search box: the grid answers
  immediately, the URL bar shows the clipped query, and other tabs' page
  loads are unaffected.
- `curl` the same URL eight times concurrently and load the library page
  in a browser at the same time: it renders.
