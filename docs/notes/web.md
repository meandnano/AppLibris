# Web UI and service layer

Rationale for `internal/web` and `internal/service`. The package map and
the invariants that must hold are in CLAUDE.md.

## Service layer

`internal/service` is the layer beneath the HTTP handlers, so a future
`/api/v1` can be a second thin transport over the same calls. Handlers
parse the request, call one service method and render. Business rules,
validation and normalisation live in the service, where both transports
would need them; presentation stays in the transport as pure functions of
the data it is given. The history window is the worked example: what
"recent" means is decided beside the query, from the service's own clock,
while rendering an instant as "yesterday, 22:41" stays in `internal/web`.

`Service.now` is a private `func() time.Time`, defaulted by `New` and
overridden in tests, and every timestamp the service writes goes through
it. That is what makes `modified_at` propagation and the history window
testable without sleeping.

Reads return shaped types rather than storage rows. `ListBooks` and
`SearchBooks` build a `BookSummary` per book through one shared helper, so
the two grids cannot diverge. `BookSummary.Locations` normalises a book
absent from `CountFilesByBook`'s map to 1: zero and one both mean "no
multi-location badge", and one location is what an absent entry means in
practice.

`SearchBooks` sanitises the query through `storage.SanitizeFTSQuery` and
returns a `SearchResult`: one page of books, whether a search actually ran
(`Searched`), which indexed fields matched, how many books matched in
total (`MatchCount`) and where the next page starts. A query that
sanitises to nothing is treated as `ListBooks`, so an empty search box and
a freshly loaded page are one state. `Searched` exists because "sanitises
to nothing" is wider than "looks blank": control characters are stripped,
so `?q=%00` is a non-blank query that is nonetheless no search, and a
transport deriving its own flag from the raw query would render a result
count over the whole library. `Fields` is only fetched when something
matched; the no-matches state names the searched fields itself.

`GetBook` returns `nil, nil` for an unknown id, the same absent-is-not-an-
error contract the storage finders use, and the handler turns that into a
404. `BookDetail.FileSize` is a book-level field taken from the first
location: every location of one book is byte-identical by construction,
and a size per path would imply a difference that cannot exist.
`HasFileSize` distinguishes a book with no location from one whose file
really is zero bytes, so the page never claims `0 B` for a size it does
not know.

`UpdateBookMetadata` maps the field name onto storage's enum, normalises
the value and returns the reloaded `BookDetail` rather than echoing the
input, so the caller renders canonical data and normalisation is visible.
Title is required; every scalar except description rejects an embedded
line break, since a stored one breaks every single-line rendering
downstream and a future API could submit one even though a browser input
cannot. Authors are split on newlines, trimmed, blanks dropped and a
repeated name kept once, because the textarea is free text and a repeat is
a slip. A rejected value is a `metadataValidationError` wrapping
`ErrInvalidMetadata` and carrying the sentence the field shows, so a bad
value is a field error and never a 500.

`QueueSend` owns the send rules. It validates the address with
`net/mail.ParseAddress` and stores only the mailbox, so a pasted
`"Mike <mike@kindle.com>"` saves the address and not the display name;
`ErrInvalidAddress` queues nothing. `BookDetail.Sendable` and
`SendableNote` say whether Amazon's Send to Kindle accepts the book's
format at all, decided by `sendableFormat` over the formats Amazon lists
that the scanner can index, today only `epub`. Amazon drops an FB2 or ZIP
attachment silently, so a send Resend accepted would read "Delivered" for
a book that never reached the device; until format conversion exists the
honest surface is a control that says why it is not offered. `QueueSend`
does not refuse an unsendable book: the button is not rendered, and a
hand-crafted POST is harmless, where a 4xx would be a second rule to keep
in step with the first for no visible gain.

`Service.Notify` and `Service.NotifyEnrichment` are function fields set by
`cmd/server`, nil in tests and whenever a queue is unconfigured. Function
fields rather than interfaces, because `internal/service` importing
`internal/sender` would be a cycle. Two fields rather than one multiplexed
hook, because poking the wrong worker leaves a job waiting for its poll
tick. `Notify` fires only when `EnqueueSend` reports it actually inserted a
row, so a double submit never wakes the worker twice.

The send and enrichment surfaces are parallel triples (queue, state,
latest) shaped through `sendStateFrom` and `enrichmentStateFrom`, each
collapsing "when did this happen" to one `At` field so a template branches
on one shape. They stay two surfaces on purpose: an abstraction over
exactly two cases has no third instance to test against, and they differ
in precisely the part that would have to be generic, an address and a
failure reason versus a list of written fields.

`SendHistory` covers a trailing 30 days capped at `SendHistoryLimit`
(500). Truncation is detected by asking storage for one row past the
limit rather than a second `COUNT`; `ListBooks` and `SearchBooks` decide
`HasMore` the same way. The cap is exported because `internal/web` spells
the number out in the scope line, and a copy of the literal there would
drift.

## Rendering

`render` executes the template into a buffer before writing anything, so
a template error is a clean 500 rather than a truncated page. Only the
pre-write `ExecuteTemplate` error is returned to the handler. Once the
buffer starts writing, the response is committed, and a write failure
there (almost always the client disconnecting) is logged inside `render`;
a handler answering it with `http.Error` would double-write onto a
committed response. `renderStatus` is the same with an explicit status,
for the one case that needs a body and a non-200 together: a rejected edit
answering 422 with the editor and its message.

Handlers map service types onto small per-page view models so templates
hold no logic. Every label is composed in the handler: the results line,
the "2 paths" badge (set only above one, so the template branches on
presence and cannot render "1 paths"), the send button's label, the
history row's status. The template only ever chooses which block to show.

Static assets are embedded via `go:embed` with no build step. They get a
content-derived `ETag` computed once at startup (`embed.FS` reports a zero
`ModTime`, so `http.FileServer` would otherwise emit no validator) and a
five-minute `max-age`, which bounds how long a stale file survives a
deploy. Covers get a day-long `max-age` and no `immutable`: `cover.Store`
names a file by the book's content hash, not by the bytes served, so a
changed resize pipeline can put different bytes at an unchanged URL. Both
mounts wrap their filesystem in `noDirFS` so a directory 404s instead of
listing every content hash in the library.

The UI is translated from mockups kept on the `init` branch.

## htmx contract and progressive enhancement

htmx is vendored at `internal/web/static/js/htmx.min.js`, version 2.0.10,
pinned in a comment at the top of the file. It is used only where
dynamism is needed: search-as-you-type, the send and enrichment controls
polling their job, inline editing, and the grid appending its next page.

Every dynamic affordance has one markup path that works with and without
JavaScript. A read affordance is an `<a>` carrying both `href` and
`hx-get`; an editor is a `<form>` carrying both `action` and `hx-post`;
the plain-navigation response is a whole page or a `303` back to it, and
the htmx response is a fragment. There is no separate no-JS path to drift.

Whether a request gets a fragment is decided by `isHTMXFragment`:
`HX-Request` present **and** `HX-History-Restore-Request` absent. htmx
sets both on the request it issues when Back lands on a URL that has
fallen out of its history cache (ten entries, and `hx-push-url` pushes one
per keystroke), and swaps that response into the whole document body.
Answering it with a fragment replaces the masthead, search bar and scripts
with a bare grid that can no longer search. Every route serving two bodies
names both headers in `Vary: HX-Request, HX-History-Restore-Request`; a
route serving one body to every caller, such as the send status poll,
sets no `Vary` at all.

The vendored htmx does not swap a 4xx response. Two places depend on that
fact and answer 200 where the status would honestly be 4xx: a rejected
inline edit on the fragment path (below) and a refused fetch-metadata
request from an htmx form (further below). Do not opt 4xx swapping in from
the client through `htmx:beforeSwap`: it makes the whole interaction
depend on one listener still being loaded and still matching, and a
silent no-op Save is the worst failure the page has.

## Search

Search is `GET /{$}` with a `q` parameter, not a separate route, so the
empty box and the unfiltered library are the same page. Each keystroke is
a debounced (`delay:300ms`) request swapping `#book-grid` with
`outerHTML`. The search box lives only in the full page and is never
re-rendered, so a keystroke mid-request is never lost. `hx-push-url` keeps
the URL shareable. With JavaScript off the same `<form method="get">`
submits to the same handler.

Three affordances resolve in the browser because the input is never
re-rendered: the `clear ×` link (a plain `href="/"`, hidden by CSS while
the box shows its placeholder), the `/` shortcut hint (unhidden only after
`search.js` binds the key, so it never advertises a shortcut that is not
bound) and the `filtering …` status line. The status line renders inside
`<main>` sharing the results count's container and margins, because the
two swap places and the grid would otherwise jump on every keystroke.

Two separate things bound an overlong query, and pairing them the obvious
way is backwards. The handler passes `q` through
`storage.NormalizeSearchQuery`, the same call the service makes on the
way to a `MATCH` expression, so every copy the page renders back (the
input's value, the no-results heading, the paging URLs) is the string
that was searched rather than a same-numbered clip made at a different
point. That is all it does; it cannot bound the request, because the
input is outside `#book-grid` and htmx re-sends whatever was pasted on
the next keystroke. What bounds the request is the input's own
`maxlength`, carried into the template as `SearchMaxLength` from
`storage.MaxSearchBytes` so the number has one home. A forgotten field
renders `maxlength="0"` and makes the box untypeable, which is why the
wiring has a test. `maxlength` counts UTF-16 units, so it approximates
the byte cap from above and never cuts what the server would keep.

A query that sanitises to nothing renders the plain grid with no result
count. One that searches renders either the results line (`4 of 1,284 ·
matched title, author`, both counts grouped by thousands as the masthead's
is, since one screen must not show the same number two ways) or a
distinct `search__empty` block, kept separate from the empty-library
block because the two call for different next actions. The empty-library
state dims and disables the search control; with nothing indexed there is
nothing to search.

The search deliberately does not order by relevance: a grid someone is
scanning while they type must not reorder under them. That same property
is what lets one cursor page both the filtered and unfiltered grid.

## Paging

The grid renders `pageSize` (48) books and appends the next batch when
the trigger beneath it is revealed. 48 is the mockup's own figure, and a
number in a mockup is a decision about how much scrolling one reveal buys.
Unpaged, a reference library of 1,284 books was 1,284 cards and 1,284
lazy cover requests in one document; nothing about it was bounded.

One route serves three shapes, one more than the `HX-Request` split can
tell apart, so the third is named in the query: the full page, the whole
`book-grid` fragment a keystroke gets, and with `append=1` just the next
batch of cards. `book-grid-cards` is that batch, and the same template
renders inside the full grid's `<ul>`, so a page of cards looks identical
however it arrived.

The trigger is a single `<li class="grid__more">` inside the cards'
`<ul>`, because it replaces itself (`hx-target="this"`,
`hx-swap="outerHTML"`) with the next batch plus a fresh trigger, and
whatever it swaps in has to be a legal child of that list. It carries both
`href` and `hx-get`. The plain href is a whole page starting at the same
cursor; an unpaged grid works with JavaScript off, so a paged one that
forgets the fallback is strictly worse than no paging. `MoreLabel` empty
is how the last page renders no trigger rather than an offer of zero more
books, and the count in it is the one the reader is looking at: the
library total on an unfiltered grid, `MatchCount` during a search.

A keystroke rebuilds the whole grid including its trigger, so a new
search resets paging by construction; a stale trigger would append page
two of the previous query. That is invisible until it breaks, so a test
pins it.

Paging creates the possibility of being deep in the library with
JavaScript off, and every affordance that would lead home is inert there:
the brand and the current nav item are plain text, and the clear link is
hidden whenever the box is empty, which is exactly a deep unfiltered
page's state. The clear link therefore persists on such a page, reading
"first page" rather than naming a search that is not running.

The cursor itself, and why it is keyset rather than `OFFSET`, is
`internal/storage`'s decision; see `docs/notes/storage.md`.

## Book detail and editing

`GET /books/{id}` parses the id with `strconv.ParseInt`; a non-numeric
and an unknown id both plain 404, indistinguishable on purpose, since
neither is a client error worth its own page. Metadata renders one element
per field. Empty optional fields (publisher, date, language, ISBN, and
file size when the book has no location) render as visible em-dash rows
rather than being dropped: a hidden field cannot be filled in, and sparse
metadata is the common FB2 case. `PublishedDate` renders exactly as
stored; it is free text from embedded metadata, sometimes a year and
sometimes a full date, and parsing it would lie confidently. Locations
reveal through a native `<details>`, since no JavaScript is guaranteed to
have loaded, with a location inside its missing-file grace period
annotated.

Inline editing is `GET`/`POST /books/{id}/metadata/{field}`, one route
per field rather than one form per page, so each field is its own swap
target and a keystroke in one never re-renders another. `makeFieldViews`
builds all seven from one place, so a whole-page render and a single-field
fragment cannot drift. Each view carries `Value` (what the control edits,
authors newline-separated) and `Display` (what the read view shows, "A, B
& C") separately, because the stored and readable forms differ. Every
read affordance carries an `aria-label` naming its field: with an optional
value empty its visible text is only an em dash, so the accessible name is
the only thing distinguishing seven otherwise identical links.

Without htmx the `GET` redirects to `/books/{id}?edit={field}`, which
renders the whole page with that editor open, and the `POST` 303s back to
the book. An unrecognised `?edit=` value opens nothing rather than 400ing;
it names no resource. Both paths load the book before choosing a shape,
so an unknown book is the same plain 404 on each rather than a 303 for a
book that does not exist.

`storage.ParseMetadataField` is the gate on these routes and on `?edit=`,
and `cover` is deliberately absent from it: `cover_path` holds a path
`internal/cover.Store` produced, never text a person types. Admitting the
name would make `POST /books/{id}/metadata/cover` reach storage, come back
with an error that is not `service.ErrInvalidMetadata`, and answer 500,
where a name nobody may edit should simply 404.

**A rejected fragment answers 200; the rejected full page answers 422.**
htmx does not swap a 4xx, so an honest status on the fragment would leave
the editor untouched and make Save look like it did nothing. The
navigation path keeps the 422, where nothing swallows it. This is the one
place the UI trades an accurate status for a working interaction.

**The body cap is derived, not chosen.** `maxMetadataFormBody` is
`3 × service.MaxMetadataValueBytes + 1024`: the service limits decoded
bytes, `MaxBytesReader` bounds the encoded body, and form-urlencoding
triples non-ASCII text. It is sized off the author list rather than the
description, since 100 names of 1 KiB outweigh 64 KiB of prose, and
sizing off the description would reject a valid author list before
`normalizeAuthors` could apply its own limits. Over the cap is still a
field error, not a bare 413.

Provenance markers appear only where they are not obvious.
`providerSourceNote` renders a marker for a provider's name and nothing
for `embedded`, `manual` or an absent source. Every field has a source,
and rendering all seven would double the block's weight to say "embedded"
seven times. The marker is a caveat: a value read out of the file is a
fact about the file, a typed value is the person's own, and a third-party
guess is the only one whose origin changes how much to trust it. `manual`
renders nothing even though it is the source the resolver cares most
about, because the person who typed it does not need telling. The marker
is derived in `makeFieldViews`, and the metadata POST reloads the book
rather than echoing the submitted value, which is what makes saving a
field clear its marker for free. A test pins that, because it is exactly
what a later optimisation removes.

## Send and enrichment controls

The send control mounts above the description, the reason the page gets
opened, via `{{template "send-control" .}}` over `bookDetailPage` itself,
so `POST /books/{id}/send` and `GET /books/{id}/sends/{sendID}` build one
mostly-zero-valued `bookDetailPage` rather than a parallel type. Its
states (idle, sending, delivered, failed, sending unconfigured, format
not accepted) are driven by fields `applySendState` computes once, so the
template branches on which block to show and never on how to phrase it.
`queued` and `sending` are one visual state, "Sending": the UI has no
separate treatment for the gap between enqueue and claim, which the
worker's `Notify` poke keeps short.

The whole control is one swap target (`id="send"`). Form and status share
a region because the states replace each other. The pending status box's
`hx-get`/`hx-trigger="load delay:2s"` targets `#send`, not itself, so the
outer swap replaces the whole control, and a terminal state's block
carries no such attributes, so polling stops by construction. The
`<form>`'s own `hx-post` survives every state, keeping "Send again" and
"Retry" enhanced; a retry is a new row.

Every route that renders the control copies `BookDetail.SendableNote`
onto the page, so a fragment can never offer a button the full page
withholds. The page carries only the note, and the template branches on
it: a separate `Sendable` bool beside it would let a route that copied
neither render a refusal with no reason.

With zero saved recipients the `+ add address` `<details>` renders open
and the `<select>` is omitted. It also renders open after a rejected
address, with the typed values carried back so the fix is an edit rather
than a retype. That path re-reads `LatestSend` rather than rendering a
nil state: nothing was queued, so retracting a Delivered or Failed result
over a typo would make the page contradict itself.

`POST /books/{id}/send` answers a fragment request with the fragment and
everyone else with a `303` back to the book, whose initial render picks
the job up through `LatestSend`. With sending unconfigured it 503s with
the disabled fragment rather than 404ing, so a stale open tab gets an
explanation. `GET /books/{id}/sends/{sendID}` is scoped under the book id
so a mismatched pairing 404s instead of leaking one book's send under
another's page.

Removing a saved recipient is `POST /recipients/remove`, reachable only
from the send control's address list; there is no management screen. An
`<option>` cannot hold a button, so the address list lives in the `+ add
address` `<details>`, one row per address with a "remove" button. That
button cannot be a child of the send form, since submitting it must never
also submit a send and HTML forbids nested forms, so it submits a sibling
`<form id="recipient-form">` through its `form=` attribute, plain HTML
with no duplicated markup. The form carries the book id so the response
re-renders that book's whole send control, since removing an address
changes the picker too. Removing an unknown address 200s like any other:
a double submit is a slip, not an error.

Enrichment reuses the send control's state machine rather than inventing
a second one: `POST /books/{id}/enrich` and
`GET /books/{id}/enrichment/{jobID}`, the same book-id scoping, one swap
region (`#enrich`), the same fragment-or-303 split, polling that stops
because only the pending block carries a trigger. It differs where the
job differs: no recipient picker, and the terminal states are "Added
publisher, description" or "Nothing to add". **"Nothing to add" is a
success.** It is the ordinary outcome for a book whose embedded metadata
is complete and for any book no provider could answer, and rendering it
as a failure would teach people to distrust a working feature.
`EnrichResultOK` carries that, and a mutation test asserts it. The result
names the fields that moved rather than saying "done". With no provider
configured the control renders the disabled treatment the send control
shows without Resend, and the POST 503s with that fragment.

The enrichment surface is exactly three affordances, one per question it
raises: where did this value come from (the provenance marker), can I
fetch metadata now (the trigger), did it do anything (the result). A
library-wide enrich, an enrichment history page and editable provenance
are absent by decision: the first has no honest progress display short of
building one, the second is a page nobody opens because the result is
visible in the fields themselves, and a source is a fact rather than a
setting.

## Cross-site protection and the HTTPS requirement

The send POST, every metadata POST, the enrich POST and recipient removal
are the only state-changing routes, and each is wrapped in
`sameSiteOnly`, which rejects a request whose `Sec-Fetch-Site` is anything
but `same-origin` or `none`. There is no login, so the network position of
the request is the only thing between the collection and everyone else.
Any page in the user's browser can reach a LAN or localhost server its
author cannot, and a form-encoded POST needs no CORS preflight, with the
attachment's destination address in the request body.

Browsers send `Sec-Fetch-Site` only to a potentially trustworthy origin:
HTTPS, or localhost. Over plain HTTP on a LAN or tailnet address it is
absent from every request, cross-site ones included, and `sameSiteOnly`
alone admits everything. The deployment requirement follows, in two parts
that only work together: an HTTPS gateway in front (the app's redirects
are relative paths, so it is agnostic to the scheme), and the plain
listener bound so nothing but that gateway reaches it,
`ADDR=127.0.0.1:8080` with the proxy on the host or an unpublished port on
a shared Docker network with a sidecar. A listener published on the LAN
beside an HTTPS front is the requirement half-met, which is unmet.

Two wrappers in `web.go`, one of which `cmd/server` puts around the whole
handler, make a violated requirement visible. `RequireFetchMetadata`
(`REQUIRE_FETCH_METADATA=true`, the default) refuses any
non-GET/HEAD/OPTIONS request with no `Sec-Fetch-Site` at all, logging each
refusal at Warn: every current browser sends the header over HTTPS, so a
mutation without it is a script or an exposed plain listener, and the
person whose edit was refused needs the log to say so. The refusal has two
shapes, for the reason a rejected edit answers 200: an htmx fragment
request gets a 200 carrying the `fetch-metadata-refused` partial with
`HX-Reswap: afterbegin`, which inserts one line as the first child of
whatever the posting form's `hx-target` names, so the control survives
beneath it and the wrapper never learns which control posted. Every other
client gets the 403. `next` is not called in either shape.
`WarnMissingFetchMetadata` (`REQUIRE_FETCH_METADATA=false`) admits
everything and logs one Warn per process, a tripwire rather than a guard.
`cmd/server` picks between them through a pure function with a table test
pinning both directions, since swapping the branches would invert the
security default with every handler test still green.

`sameSiteOnly` itself passes an empty header through, deliberately: it
answers only the question it can, "the browser said cross-site", and the
opt-out mode depends on that.

The HTTPS requirement also closes DNS rebinding against the unchecked
`Host` header: a rebound hostname fails certificate validation against an
HTTPS origin, and the plain listener is not reachable from a browser at
all. That is why there is no `Host` allowlist.

## History page

`GET /history` lists every send over `service.SendHistory`'s window,
newest first, answering "did I already put this on the Kindle?" across the
library rather than per book. It renders even with sending unconfigured:
it is a log, not an action, and a library that used to send still has
history worth reading. Each row is composed in the handler.
`historyStatus` collapses `queued` and `sending` into one "Sending", the
same collapse the send control makes, since two screens naming one state
differently would be worse than either alone. `BookURL` is empty for a
send whose book has been pruned, rendered unlinked rather than pointing
nowhere; the row's title and address come from `send_log`'s own columns,
which is what lets a pruned book's send still appear.

The scope line reads "last 30 days" ordinarily and names
`SendHistoryLimit` once the cap has truncated the window. The page's two
wrong answers are not symmetric: a false "yes" costs a moment's doubt, a
false "no" causes a duplicate delivery, the failure the send job model is
built to avoid. A fixed "last 30 days" over a silently truncated list
would reintroduce in the UI what the queue prevents.

`relativeTime(t, now)` renders "today, 14:02", "yesterday, 22:41",
"28 Aug, 09:15" as a pure function with `now` passed in, so every case is
a table test. It converts both times to the server's local zone, the only
zone a server-rendered page without JavaScript knows, and compares
calendar dates via `AddDate` rather than a raw `time.Sub`: a send at 23:50
is "yesterday" twenty minutes later at 00:10, which a `< 24h` comparison
gets wrong at exactly that boundary.

The masthead's `site-header` partial takes `Nav` and `HeaderNote`, both
composed by the handler. `navFor(current)` builds every nav entry each
time and marks one current, rendered as plain text rather than a link,
since there is nowhere more useful to send someone already on the page.
The book detail page passes `navFor("library")`, having no entry of its
own. `headerBookCount` composes the "1,284 books" note both book pages
share; the history page puts its scope line in the same slot.

## Styling

There is one button system in `app.css`: `.button` with `--md`/`--lg`
sizes and `--primary`/`--secondary`/`--tertiary` intents, plus `.spinner`
and `.spinner--sm`, shared by the send control, the enrichment control and
the inline editors. Two neighbours stay outside it on purpose:
`.search__spinner` is toggled by `htmx-request` and coloured against the
input, sharing only the keyframes, and `.send__remove` is a borderless
text affordance rather than a button.

`--primary`'s foreground is `var(--bg-raised)` and must stay a token. It
resolves to `#fff` in light theme, which reads as the obvious
simplification, but `--accent` is a light tan in dark theme, where white
on it measures 2.9:1, under even the 3:1 large-text floor, on the primary
action of the whole application. The token holds 5.8:1 in light and 6.1:1
in dark.

Two rules read as tidy-ups and are load-bearing: `--md`/`--lg` reset the
base's `min-height`, without which the enrichment button gains a pixel,
and `.button--tertiary:disabled` beats `.button:disabled` on source order
alone, so grouping the `--tertiary` rules together silently reverts it.
Two tests guard the class names in each direction: no retired name
survives in any template or stylesheet, and every button or spinner class
the markup names has a rule, since a mistyped modifier renders as a bare
`.button` with every handler test still green. Known limits:
`docs/backlog/2026090610-description-paragraphs-do-not-render.md` and
`docs/backlog/2026090702-button-base-carries-the-editors-size.md`.
