# Step: Static and cover asset serving

## Context

Promoted from `docs/backlog/2026083115-static-asset-serving.md`, deleted in
this change. Re-validated against current master: neither #16 nor #18
touched `internal/web`, and both `Routes` mounts are still the stock file
servers the backlog file described.

```go
mux.Handle("GET /static/", http.FileServerFS(staticFS))
mux.Handle("GET /covers/", http.StripPrefix("/covers/", http.FileServer(http.Dir(coversDir))))
```

The backlog file said to measure before planning the caching work. Measured —
and the result **inverts** what that file assumed.

### 1. Directory listings, as described

Verified on master: `GET /covers/`, `GET /static/` and `GET /static/css/`
all return a browsable HTML index. `http.FileServer` generates these for
any directory with no `index.html`.

This is untidy rather than unsafe, exactly as the backlog file argued —
DESIGN.md's Authentication section is "None. Internal network only", so
anyone who can reach `/covers/` can already reach every cover named on the
library page. It is unintended surface, not a leak.

### 2. The embedded static assets cannot be cached *at all*

This is the finding that changes the plan. Measured response headers:

| request | Last-Modified | ETag | Cache-Control |
|---|---|---|---|
| `/static/css/app.css` | *(none)* | *(none)* | *(none)* |
| `/static/js/theme.js` | *(none)* | *(none)* | *(none)* |
| `/covers/<hash>.jpg` | `Tue, 01 Sep 2026 …` | *(none)* | *(none)* |

`embed.FS` reports a **zero `ModTime`**, so `http.FileServer` emits no
`Last-Modified` — and with no validator of any kind, a browser cannot make
a conditional request. Confirmed directly: `app.css` with a far-future
`If-Modified-Since` still returns **200 and the full body**, not 304.

So the stylesheet and the theme script are re-sent in their entirety on
**every page load, forever**. The backlog file had this backwards: it
treated covers as the caching problem and static assets as the secondary,
fiddly part. In fact covers already revalidate correctly via
`Last-Modified`; the embedded assets are the ones with no caching story at
all.

### 3. Covers: one conditional request each, and there are a lot of them

Covers do get `Last-Modified`, so a browser revalidates rather than
re-downloading — but that is still a round trip per cover. Measured on a
500-book library, the rendered page is **182KB with 500 `<img>` tags, all
carrying `loading="lazy"`**.

The lazy attribute is doing real work: the initial load only fetches what
the viewport shows, so this is not 500 requests up front. But a full scroll
through the library is 500 conditional requests, every visit, for files that
**can never change** — their names are content hashes.

## Scope

In scope: suppressing directory listings, giving the embedded assets a
validator so they can be cached, and marking covers immutable. Out of
scope: any change to how covers are named or generated — the immutability
argument below depends on content-hash naming, and this step must not be
the thing that changes it.

No schema change, no migration, no new dependency.

## Directory listings

`http.FileServer` has no option to disable them, so wrap the filesystem so
directories look absent:

```go
// noDirFS hides directories from http.FileServer, which would otherwise
// generate a browsable index for any directory lacking an index.html.
// Serving one is not a leak — DESIGN.md binds this to a trusted network —
// but the covers directory listing every content hash in the library is
// surface nobody asked for.
type noDirFS struct{ fs http.FileSystem }

func (f noDirFS) Open(name string) (http.File, error) {
    file, err := f.fs.Open(name)
    if err != nil {
        return nil, err
    }
    info, err := file.Stat()
    if err != nil {
        file.Close()
        return nil, err
    }
    if info.IsDir() {
        file.Close()
        return nil, fs.ErrNotExist
    }
    return file, nil
}
```

Applies to both mounts. `http.FileServer` turns `fs.ErrNotExist` into a
404, which is the right answer: there is no resource at `/covers/`.

Note this also 404s a bare `/static/` and `/covers/`, which nothing links
to. If a future index page is ever wanted there, it should be a real
handler, not `FileServer`'s generated listing.

## Caching

Two different problems needing two different answers, because the two
mounts differ in one decisive way: **cover filenames are content hashes,
embedded asset filenames are not.**

**Covers — immutable, one year.**

```go
w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
```

Safe *only because* `cover.Store` writes to `dir/<contentHash>.jpg`: the
bytes at a given URL can never change, because different bytes get a
different hash and therefore a different URL. Put that reasoning in a
comment next to the header, tied to `cover.Store` by name. If cover naming
ever stops being content-addressed, this header becomes a bug that serves a
stale thumbnail for a year, and the comment is what makes the next reader
notice.

This turns the 500 conditional requests measured above into zero after the
first visit.

**Embedded assets — a content-derived `ETag`.** They have no validator at
all today, so the first job is giving them one. `embed.FS` has no modtime
to use, but the content is fixed at build time, so hash it once at startup:

```go
// staticETags maps an embedded asset's path to a strong ETag derived from
// its content. embed.FS reports a zero ModTime, so http.FileServer emits
// no Last-Modified and a browser has nothing to revalidate against — the
// stylesheet is re-sent in full on every page load without this.
var staticETags = buildStaticETags() // path -> `"<sha256 prefix>"`
```

Walk `staticFS` once in an `init` or a `sync.OnceValue`, hash each file,
and have the wrapper set `ETag` before delegating to the file server.
`http.ServeContent` handles `If-None-Match` and the 304 automatically once
the header is present.

Pair it with a **short** `max-age` (five minutes is plenty), *not* a long
one: the filenames are stable across releases, so a long max-age would
serve a stale stylesheet after a deploy with no way to bust it. The ETag is
what makes repeat loads cheap; the max-age just avoids a revalidation
round trip within a single browsing session.

A content-hashed filename (`app.<hash>.css`) would allow the immutable
treatment here too, but it means generating the filename and rewriting the
template reference, which is build-step-shaped — precisely what DESIGN.md's
"no build step" constraint rules out. The ETag gets most of the benefit
with none of that.

## `internal/web` changes

One small wrapper handler used by both mounts, parameterised by the headers
it sets, rather than two near-identical ones. `Routes` becomes:

```go
mux.Handle("GET /static/", staticHandler(staticFS))
mux.Handle("GET /covers/", coversHandler(coversDir))
```

with the `noDirFS` wrapping and the header-setting inside each. Keep
`Routes` itself a readable list of routes — the existing comment on it
explains the 404 ownership, and that should stay true.

## Tests

`internal/web`:

- `GET /covers/` and `GET /static/` return **404**, not a listing. Assert on
  the status, and also that the body does not contain `<a href=` — a future
  refactor could plausibly restore a listing while still returning 200 for
  some other reason.
- `GET /static/css/app.css` returns a non-empty `ETag`, and repeating the
  request with that value in `If-None-Match` returns **304 with an empty
  body**. This is the assertion that pins the actual fix; on master the
  same exchange returns 200 with the full stylesheet, so run it before the
  change to confirm it fails.
- `GET /covers/<hash>.jpg` sets `Cache-Control` containing `immutable`.
- A cover and an asset are both still served correctly with their content
  intact — the existing `TestStaticFileServed` and
  `TestCoverServedFromCoversDir` cover this and must keep passing.
- Path traversal: `GET /covers/../secret` still does not escape the covers
  directory. `http.Dir` already refuses this and the wrapper must not
  undo it.

## CLAUDE.md

Update the `internal/web` bullet: `/static/` and `/covers/` serve files
only — directory listings are suppressed — with covers marked immutable
(safe because they are named by content hash) and embedded assets carrying
a content-derived `ETag`, since `embed.FS` provides no modification time to
revalidate against.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual, with devtools open on a populated library: confirm `app.css` and
  `theme.js` return **304** on a second page load rather than 200, and that
  cover requests stop appearing entirely after the first visit. Both are
  visible in the network panel without any special tooling, and both are
  currently 200 on every load.
