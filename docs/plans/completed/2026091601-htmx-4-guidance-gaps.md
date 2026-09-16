# Five htmx 4 guidance gaps

## Context

An audit of every `hx-*` attribute against htmx 4's own guidance and the
vendored `4.0.0` source found no htmx 2 leftovers — attribute names, event
names, config keys, `:inherited` and the `noSwap`/`hx-status` pairing all hold.
It did find five places where this app hand-rolls something htmx 4 has an
attribute for, or sets globally something htmx 4 scopes per element. One is a
live bug.

## The search grid can settle on a stale query

The input carries no `hx-sync`, so htmx defaults to `queue first` per element
(`#syncStrategy` returns it when the attribute is absent, and the queue state
lives on the element, so it spans keystrokes). A queued request's URL is frozen
before it is admitted: the GET's body is folded into `request.action` and only
then does the sync gate run. And the queue holds one entry — `queue first`
returns `dropped` once it is non-empty.

So under latency, request A is in flight, B is queued carrying an older `q`,
and keystrokes C and D are dropped outright. The grid renders B's results while
the box holds D's text, and because the box is never re-rendered, nothing on the
page says so until the next keystroke.

`this:replace` abandons A and runs the newest request, which also cancels the
`MATCH` the server is running for a keystroke nobody is waiting for. It costs a
console error per superseded request: htmx 4.0.0's request `catch` has no
`AbortError` filter and logs every rejected fetch. That is accepted — a
cancelled request is cheaper than a wrong grid. `queue last` was the
alternative: no console noise and a correct final state, but one stale grid
renders in between and the superseded query runs to completion.

## The other four

**A global timeout where htmx 4 scopes one.** `defaultTimeout: 0` on the meta
tag was right about the import routes and wrong about everything else: a hung
search or status poll had no bound at all and would leave its indicator up
forever. htmx 4 added `hx-config` for exactly this, and it is read ahead of
`config.defaultTimeout`, with `0` parsing to a falsy interval so no timer is
installed. The two import forms say it themselves; every other request keeps
htmx's sixty seconds.

**A double-submit guard written in CSS.** `.htmx-request .import__submit`
carried `pointer-events: none`, which a focused button walks straight past on
Enter or Space — and with queue-first the second submit is queued rather than
dropped. `hx-disable` sets the `disabled` attribute for the duration of the
request, and htmx applies it after the body has been read, so it cannot strip
the file part it is guarding.

**A redundant attribute with a comment that is wrong for htmx 4.** htmx reads
`hx-encoding ?? form.enctype`, so `enctype` alone already sends the file. The
comment claimed `hx-encoding` "is what makes htmx send the file rather than a
urlencoded field name" — true in htmx 2.

**An injected stylesheet nothing uses.** htmx adopts rules for
`htmx-indicator` at startup. No element here carries that class; every
indicator is a rule of this app's own keyed on `htmx-request`.

## Steps

1. `internal/web/templates/partials.html`, `search-bar`: the input gains
   `hx-sync="this:replace"`.
2. `document-head`'s meta tag drops `defaultTimeout` and gains
   `"includeIndicatorCSS":false`.
3. `import-form` and `import-preview`'s confirm form gain
   `hx-config="timeout:0"` and `hx-disable="find button"`; the discard form
   gains `hx-disable="find button"`. `import-form` loses `hx-encoding`.
4. `internal/web/static/css/app.css`: `.htmx-request .import__submit` keeps
   `opacity: 0.55` and loses `pointer-events: none`. Not swapped for
   `.button:disabled`, whose `0.85` was chosen for a different control.
5. `internal/web/web_test.go`: `TestSearchBarHTMXWiringContract` gains the
   `hx-sync` attribute; `TestHTMXConfigContract`'s meta tag and its comment follow the
   change.
6. `internal/web/import_test.go`: a new `TestImportFormsBoundTheirOwnRequests`
   asserts each attribute against its own form's opening tag, through an
   `openTag` helper — three forms render into one preview panel and two carry
   `hx-disable`, so a check against the whole body passes on a neighbour's
   attribute. The timeout opt-in is now the only thing between a slow import
   and a browser that abandons it, which is exactly the kind of attribute a
   tidy-up deletes silently.
7. `CLAUDE.md` and `docs/notes/web.md` carry the four rules.

## Not done

The send, enrich, metadata-save and forget controls still have no
`hx-indicator`; their responses render their own pending state, and the gap is
only the window between click and swap.
