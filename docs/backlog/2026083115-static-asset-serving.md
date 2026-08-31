# Backlog: static and cover asset serving

## Problem

`internal/web`'s `Routes` mounts both asset trees with stock file servers:

```go
mux.Handle("GET /static/", http.FileServerFS(staticFS))
mux.Handle("GET /covers/", http.StripPrefix("/covers/", http.FileServer(http.Dir(coversDir))))
```

Two consequences, both verified against the real handler:

**Directory listings are enabled.** `GET /covers/` returns a browsable HTML
index of every cover in the library, one entry per content hash; `GET
/static/` does the same for the embedded assets. `http.FileServer` generates
these automatically for any directory with no `index.html`.

**No cache headers on covers.** Cover files are named by content hash and
are therefore immutable — the ideal candidate for a far-future
`Cache-Control` — but nothing sets one. `http.FileServer` does set
`Last-Modified` and honours `If-Modified-Since`, so a browser revalidates
rather than re-downloading, but that is still one conditional request per
cover per page load. A grid of several hundred books means several hundred
round trips on every visit to the library page.

## Why this is backlog, not a plan

Neither is a correctness bug. The listing is not an information leak in any
meaningful sense: DESIGN.md's Authentication section is "None. Internal
network only. Bind it and trust the network," so anyone who can reach
`/covers/` can already reach every cover by URL and the whole library page
that names them. It is untidy, not unsafe.

The caching cost is real but bounded — conditional requests on a LAN, for
one user. It becomes worth fixing when the grid is genuinely in the
hundreds-to-low-thousands UI.md designs for, and that is easier to judge
against a real library than in the abstract.

## Sketch

Disabling listings needs a wrapper, since `http.FileServer` has no option
for it — the usual shape is an `http.FileSystem` whose `Open` returns
`fs.ErrNotExist` for directories, or simply checking for a trailing `/` in a
small handler and 404ing. Either is a dozen lines.

For caching, wrap the covers handler to set
`Cache-Control: public, max-age=31536000, immutable` before delegating.
This is only safe *because* the filename is a content hash — if cover
naming ever stops being content-addressed, this header becomes a bug that
serves stale thumbnails for a year. Tie the two together in a comment.

`/static/` assets are **not** content-addressed (`app.css`, `theme.js`), so
they need a shorter max-age, or a cache-busting query string derived from
the embedded file's hash at startup. The second is nicer and is the point
at which this stops being a ten-minute change — worth doing at the same
time as any build-adjacent work, not on its own.

## Validate before planning

- Confirm the listing behaviour still exists — `2026083112-web-transport-correctness`
  changes routing in this file and may have altered the mounts.
- Measure before optimising the caching: load the library page against a
  realistically-sized library with devtools open and count the actual
  conditional requests. If the grid ends up lazy-loading covers (the
  template already sets `loading="lazy"`), the real number may be small
  enough that this stays backlog.
