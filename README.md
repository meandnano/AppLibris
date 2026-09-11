# AppLibris

A self-hosted ebook library server. Point it at a directory of EPUB and FB2
files and it gives you a cover grid to browse, a search box, a detail page
per book, and a button that sends the book to a Kindle by email.

It is built for one person on a home network: a single Go binary, a single
container, an embedded SQLite database, and no other services to run. The
web UI is server-rendered HTML with a little [htmx](https://htmx.org); there
is no JavaScript build step, and every page still works with JavaScript off.

## Features

- **Reads the library you already have.** EPUB and FB2 (including
  `.fb2.zip`) files anywhere under one directory, with no folder
  conventions to follow. Title, authors, language, ISBN, publisher,
  publication date, description and the cover are read out of each file.
- **Keeps up with the directory.** New, changed and removed files are picked
  up by a filesystem watch within seconds, and by a periodic rescan
  regardless. A file that disappears is marked missing and only forgotten
  after a grace period, so an unmounted disk does not delete your edits; a
  path that is gone for good — after renaming a folder, say — can be
  forgotten from the book's own page. Rewriting a file in place, as
  Calibre and friends do, keeps the edits you made to it.
- **Merges byte-identical duplicates.** The same file at two paths is one
  book with two known locations, flagged on the grid and listed on the
  detail page.
- **Search as you type** over title, authors, description and ISBN. Word
  prefixes match in any order, diacritics are ignored in both directions,
  and an ISBN matches however it is punctuated. Press `/` to jump to the
  search box.
- **A paged cover grid** that loads 48 books at a time and appends more as
  you scroll. Every page and every search has its own shareable URL.
- **Inline metadata editing** on the detail page, one field at a time. A
  value you edit is never overwritten by anything automatic.
- **Metadata enrichment on request.** A "Fetch metadata" button fills in
  the fields a file did not provide, from Open Library and Google Books, and
  fetches a cover when the book has none. Embedded and hand-edited values
  are never touched, a provider-supplied value is marked with its source,
  and an answer that does not plausibly match the book is discarded rather
  than written.
- **Send to Kindle** through [Resend](https://resend.com), with a saved
  list of recipient addresses, a live status on the book page, retry, and a
  history page answering "did I already send this?".

## Running it

Every release publishes a linux/amd64 and linux/arm64 image to
`ghcr.io/meandnano/applibris`, tagged by version. Nothing floats, so name the
version you want and run it with two mounts:

```sh
docker run -d \
  -p 127.0.0.1:8080:8080 \
  --user "$(id -u):$(id -g)" \
  -v /path/to/books:/library:ro \
  -v /path/to/data:/data \
  -e RESEND_API_KEY=re_... \
  -e RESEND_FROM=library@yourdomain.example \
  ghcr.io/meandnano/applibris:0.1.0
```

`/library` holds your books, `/data` holds the database and the cover
thumbnails. The covers directory is disposable: delete it and the next scan
rebuilds what it can and forgets the rest.

### Which user to run as

Give `--user` the uid and gid that already own your files. The image has
nothing to set up for it — the binary looks no account up, so any uid works
and none has to exist on the host or in the image. `$(id -u):$(id -g)` is
right when the files are yours; on a NAS, use the uid and gid of the share's
owner.

That user needs:

- **write** access to `/data`, which holds the database and the covers the
  app writes;
- **read** access to `/library`, which the app never writes to — mount it
  `:ro`, as above, and a filesystem exported read-only works too.

Getting it wrong is the first thing that goes wrong, because a NAS bind
mount is owned by the share's user, an Unraid one by `nobody`, and a fresh
named volume by root. Startup says so rather than half-working, naming both
the uid it is running as and the uid that owns the directory it could not
write:

```
create covers directory /data/covers: mkdir /data/covers: permission denied
(running as uid 1000; /data is owned by uid 0)
```

`/library` must also exist: a `LIBRARY_DIR` that is not there fails startup
rather than being created, so a volume that did not mount shows up
immediately instead of as an empty grid.

There is no `HEALTHCHECK` in the image: `distroless/static` ships no shell
and no `curl`, so probe `/healthz` from your compose file or orchestrator
instead.

To run a revision that has no release, build the image from the repository
with `docker build -t applibris .` and use `applibris` in place of the
`ghcr.io` reference. Without Docker, `make run` starts the server against
`./library` and `./data`.

### Put HTTPS in front of it

There is no login. The one thing stopping a web page you happen to visit
from posting to your server is a header browsers send only over HTTPS (or
to `localhost`). So run the server behind an HTTPS gateway such as Tailscale
Serve, Caddy or the reverse proxy your NAS already has, and make sure the
plain listener is reachable only by that gateway: bind it to `127.0.0.1` as
above when the proxy runs on the same host, or leave the port unpublished on
a shared Docker network when the proxy is a sidecar.

By default the server refuses any state-changing request that arrives
without that header, so a listener accidentally exposed over plain HTTP shows
up the first time you press Save, Send or Fetch metadata, with a warning in
the log naming `REQUIRE_FETCH_METADATA`.

### Sending to a Kindle

Resend must be able to send from `RESEND_FROM`, and that address must be on
your Amazon account's Approved Personal Document E-mail List. Resend caps a
message at 40 MB including the base64-encoded attachment, so books over
about 28 MB are refused before a send is attempted. Only EPUB is offered for
sending: Amazon accepts FB2 attachments and then silently drops them.

### On a NAS

A filesystem watch only sees changes made through the mount it is watching.
On an Unraid user share, an NFS export or an SMB mount, files moved behind
the share (Unraid's mover, for example) appear at the next rescan rather
than within seconds. For instant updates, bind-mount the underlying disk
path instead of the share. The server logs which filesystem it found at
startup, warns for the types above, and checks that events actually arrive.

## Configuration

Everything is an environment variable. Relative paths resolve from the
working directory, which is `/` in the container.

| Variable | Default | Meaning |
|---|---|---|
| `ADDR` | `:8080` | Address the HTTP server listens on. |
| `LIBRARY_DIR` | `/library` | Directory holding the books. Only ever read, so it may be read-only, but it must already exist — it is never created. A symlink is followed; a dangling one fails startup. |
| `COVERS_DIR` | `/data/covers` | Where cover thumbnails are written. Created on first run, so its parent must be writable. Safe to delete. |
| `DB_PATH` | `/data/library.db` | SQLite database file. Created on first run, so its directory must be writable. |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` or `ERROR`. Logs go to stderr. |
| `SCAN_INTERVAL` | `1h` | How often the library is rescanned regardless of filesystem events. |
| `MISSING_GRACE` | `24h` | How long a file must stay missing before its record is removed. Must not be negative. |
| `WATCH_ENABLED` | `true` | Watch the library directory for changes. `false` relies on the rescan alone. |
| `WATCH_SETTLE` | `5s` | How long the directory must be quiet after a change before a rescan runs. |
| `REQUIRE_FETCH_METADATA` | `true` | Refuse state-changing requests that carry no `Sec-Fetch-Site` header. `false` admits them and logs a warning instead. |
| `RESEND_API_KEY` | unset | Resend API key. Sending is disabled while unset. |
| `RESEND_FROM` | unset | Sender address for Kindle mail. Sending is disabled while unset. |
| `METADATA_PROVIDERS` | `openlibrary,googlebooks` | Providers to ask, in order. An unknown name fails startup. Set it empty (`METADATA_PROVIDERS=`) to disable enrichment and make no outbound requests. |
| `GOOGLE_BOOKS_API_KEY` | unset | API key for Google Books. Without one the shared anonymous quota is used, which is routinely exhausted, so expect that provider to answer nothing. |

## Development

```sh
make test     # go test ./...
make build    # bin/server
make run      # go run ./cmd/server
```

Design notes live under `docs/notes/`, implementation plans under
`docs/plans/`, and deferred work under `docs/backlog/`. `CLAUDE.md` maps
the code for anyone working on it.
