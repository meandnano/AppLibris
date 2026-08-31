# Step: Web transport correctness

## Context

Two defects in `internal/web`, both of which get worse the moment a second
page exists.

**1. Every unknown path renders the library page with HTTP 200.**
`mux.HandleFunc("GET /", libraryHandler(svc))` registers a catch-all: Go's
`ServeMux` treats a pattern ending in `/` as a subtree match, and `/` is the
subtree containing everything. Verified — `/nope` and `/books/12345` both
return the full library HTML with a 200.

Today that is merely wrong. It becomes actively confusing with the book
detail page, which is the next UI step: a mistyped or stale `/books/{id}`
will render the entire library instead of a 404, and the user has no way to
tell "that book is gone" from "that URL doesn't mean what I thought". It
also means no crawler, script, or `curl` against this server can ever detect
a bad URL.

**2. Templates render straight into the `ResponseWriter`.** `render`
executes into `w`, so the first byte of output commits a 200 and starts
streaming. A template error partway through — a nil dereference in a field
access, a range over a bad type — leaves the client holding a truncated page
that looks like a network glitch, and the handler can only log after the
fact, which is exactly what `libraryHandler` does today. The error path is
unreachable-by-construction on the current single template with a
fully-populated view model; it stops being so as soon as templates take
optional metadata (the detail page: no ISBN, no description, no cover) and
htmx partials multiply the number of render paths.

## Scope

In scope: routing precision and buffered rendering. Out of scope: the book
detail page itself, htmx, and access-log middleware — this step makes the
transport correct for the pages that exist so the next one can add a route
without inheriting the bugs.

Also out of scope, deliberately: directory listings and cache headers on
`/covers/` and `/static/`. Both are real but neither is a correctness bug,
so they are in `docs/backlog/2026083115-static-asset-serving.md`.

## Routing

Go 1.22+ `ServeMux` patterns distinguish the two cases directly, so this
needs no path comparison inside the handler:

```go
mux.HandleFunc("GET /{$}", libraryHandler(svc))
```

`{$}` matches **only** the exact path `/`, rather than the subtree. With no
other pattern matching `/nope`, the mux's own 404 handles it — no
hand-rolled `if r.URL.Path != "/"` guard, and no risk of that guard drifting
out of sync when routes are added.

Note the interaction with `cmd/server`, which mounts this handler with
`mux.Handle("/", web.Routes(...))` on its own outer mux. That outer
catch-all is what makes `/healthz` work alongside the UI, and it is fine —
the inner mux still 404s paths it has no pattern for. Worth a comment in
`Routes` saying the inner mux owns 404s for everything below `/`, so nobody
later "fixes" the outer mount by narrowing it.

Add `GET /healthz` awareness nowhere: it stays in `cmd/server`, which is the
right place for a liveness endpoint that must answer even if the UI is
broken.

## Buffered rendering

```go
func render(w http.ResponseWriter, name string, data any) error {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}
```

Now a template error happens before any byte reaches the client, so the
handler can turn it into a real 500 instead of logging a truncated page.
Update `libraryHandler` accordingly:

```go
if err := render(w, "library.html", page); err != nil {
	slog.Error("render template failed", "template", "library.html", "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
```

Setting `Content-Type` explicitly rather than relying on `http.DetectContentType`
sniffing the first 512 bytes: sniffing gets it right today, but it guesses
from content, and an htmx partial that begins with a bare `<li>` is exactly
the kind of fragment sniffing does not reliably call HTML.

The buffer is a whole page in memory per request. At this library's scale
(a grid of a few thousand cards is on the order of a megabyte) against a
single user on a LAN, that is not worth streaming to avoid — and a future
`/api/v1` will buffer JSON the same way for the same reason.

Keep `render`'s signature returning an error rather than having it write the
500 itself. Handlers decide status codes; that is the layering DESIGN.md
asks for, and an htmx partial handler will want a different failure response
from a full-page one.

## Tests

`internal/web`:

- `GET /` → 200 and the library markup (existing tests cover this; they
  must keep passing).
- `GET /nope` → **404**. Add `/books/1` explicitly too: it is the route the
  next step introduces, and having the test already assert 404 means the
  detail page's own tests start from a known state rather than from an
  accidental 200.
- `GET /` sets `Content-Type: text/html; charset=utf-8`.
- A deliberately failing render produces a 500 with **no** partial body.
  Reach this by executing a template name that doesn't exist through
  `render` directly — `ExecuteTemplate` with an unknown name errors before
  writing, which is the cheapest way to exercise the path without
  introducing a broken template into the embedded set.

## CLAUDE.md

Update the `internal/web` bullet: `GET /{$}` matches only the library page
so unknown paths 404, and templates render into a buffer before anything is
written, so a template failure is a clean 500 rather than a truncated page.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: `curl -i localhost:8080/nope` returns 404, `curl -i
  localhost:8080/` returns 200 with the grid, and `curl -i
  localhost:8080/healthz` still returns `ok` — that last one confirms the
  outer mount in `cmd/server` wasn't broken by the inner pattern change.
