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

**Found in review, after implementing:** the second half of the first
paragraph is false and cannot be made true by clipping anywhere in the
handler. The vendored htmx 2.0.10 pushes the URL it requested unless the
response carries `HX-Push-Url`, which this handler never sets, and the
search input lives outside `#book-grid` and is therefore never swapped —
so after a paste the box still holds the full text and every keystroke
re-sends and re-pushes it. The clip reaches the input only on a full-page
render, which is exactly what the test asserted, so the test was green
while the claim was false on the one path that has `hx-push-url` at all.

What was done instead: `maxlength` on the search input, fed from
`MaxSearchBytes` through `libraryPage.SearchMaxLength`, which bounds what
the browser can send and push (approximately — it counts UTF-16 units, so
it approximates the byte cap from above and never cuts what the server
would keep). That is also what the constant's export is now for. The
handler's clip stays for the half of this decision that was true — every
rendered copy consistent with what was searched — and now calls
`storage.NormalizeSearchQuery` rather than clipping to the same number
independently, since the handler sees `q` before control characters are
stripped and a clip there is not the same cut.

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

  **Found in review:** FTS5 does not choke on one. A term ending mid-rune
  goes through `SearchBooks`, `CountSearchBooks` and `MatchedSearchFields`
  with a nil error and matches nothing, which was then measured directly
  rather than argued about. The boundary is still right, for two reasons
  the plan does not give: the final term would search a token ending in a
  byte no title contains, and the same string is rendered into the input
  and the paging URLs, where half a character is a U+FFFD. The tests no
  longer route the rune-boundary cases through FTS5, since that assertion
  cannot fail either way and reads as a guarantee that does not exist.
- A complete ISBN with surrounding whitespace still takes the ISBN path
  after the cut, since it is far under the cap.

  **Found in review:** written that way the test never reaches the cap at
  all — 23 bytes of input, so the clip is a no-op and the assertion
  duplicates a case the sanitizer's own table already covers. The padding
  has to exceed `MaxSearchBytes` for the clip to fire, and then the side
  it is on matters: trailing padding is harmless, while leading padding
  past the cap cuts the ISBN away entirely and the query becomes no search
  at all. Both are asserted now.
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

**Found in review:** the first bullet would have failed as specified, on
both the htmx and the no-JS paths, for the reason appended to Decision 2 —
nothing the handler does can shorten a URL the browser has already
requested. It holds now because `maxlength` keeps the box itself from ever
holding more than the cap. The second bullet was run as a throwaway test
rather than by hand: eight concurrent 100,000-token searches against a
3,000-book library finish in 58ms, with a library page rendering in 20ms
alongside them.
