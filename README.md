# AppLibris

A self-hosted ebook library server. Point it at a directory of EPUB and FB2
files and it gives you a cover grid, a search box, a detail page per book,
and a button that sends a book to a Kindle by email.

Built for one person on a home network: one Go binary, one container, an
embedded SQLite database, no other services. The web UI is server-rendered
HTML with a little [htmx](https://htmx.org); there is no JavaScript build
step, and every page works with JavaScript off.

## Features

- **Reads the library you already have.** EPUB and FB2 (including
  `.fb2.zip`) anywhere under one directory, no folder conventions. Title,
  authors, language, ISBN, publisher, publication date, description and
  cover are read from each file.
- **Keeps up with the directory.** New, changed and removed files are
  picked up by a filesystem watch within seconds, and by a periodic rescan
  regardless. A missing file is marked and only forgotten after a grace
  period, so an unmounted disk does not delete your edits; a path gone for
  good can be forgotten from the book's page. Rewriting a file in place, as
  Calibre does, keeps your edits.
- **Merges byte-identical duplicates.** The same file at two paths is one
  book with two locations, flagged on the grid and listed on the detail
  page.
- **Search as you type** over title, authors, description and ISBN. Word
  prefixes match in any order, diacritics are ignored, and an ISBN matches
  however it is punctuated. Press `/` to focus the search box.
- **A paged cover grid** loading 48 books at a time and appending more as
  you scroll. Every page and search has its own URL.
- **Inline metadata editing** on the detail page, one field at a time. An
  edited value is never overwritten by anything automatic.
- **Import from the browser.** The Import page takes an EPUB or FB2, or a
  link to one, shows its cover, metadata and whether the library already
  has it, and on confirmation copies it into the library directory and
  opens its page.
  Needs write access to the library; without it the page says so.
- **Metadata enrichment on request.** "Fetch metadata" fills fields the
  file did not provide from Open Library and Google Books, and fetches a
  cover when the book has none. Embedded and hand-edited values are never
  touched, a provider value is marked with its source, and an implausible
  answer is discarded.
- **Send to Kindle** through [Resend](https://resend.com), with saved
  recipient addresses, live status on the book page, retry, and a history
  page.

## Running it

Every release publishes a linux/amd64 and linux/arm64 image to
`ghcr.io/meandnano/applibris`, tagged by version. Nothing floats, so name
the version and run it with two mounts:

```sh
docker run -d \
  -p 127.0.0.1:8080:8080 \
  --user "$(id -u):$(id -g)" \
  -v /path/to/books:/library \
  -v /path/to/data:/data \
  -e RESEND_API_KEY=re_... \
  -e RESEND_FROM=library@yourdomain.example \
  ghcr.io/meandnano/applibris:0.1
```

`/library` holds your books, `/data` the database and cover thumbnails. The
covers directory is disposable: delete it and the next scan rebuilds it.

### Which user to run as

Give `--user` the uid and gid that own your files. The binary looks no
account up, so any uid works and none has to exist on the host or in the
image. `$(id -u):$(id -g)` is right when the files are yours; on a NAS, use
the share owner's uid and gid.

That user needs:

- **write** access to `/data`, for the database and covers;
- **read** access to `/library`, which only importing writes to. Mount it
  `:ro` and everything works except the Import page, which says so at
  startup and offers no nav link. Leave it writable to import from the
  browser.

Ownership is the usual first failure: a NAS bind mount is owned by the
share's user, an Unraid one by `nobody`, a fresh named volume by root.
Startup refuses rather than half-working, naming the uid it runs as and the
uid owning the directory it could not write:

```
create covers directory /data/covers: mkdir /data/covers: permission denied
(running as uid 1000; /data is owned by uid 0)
```

`/library` must exist: a missing `LIBRARY_DIR` fails startup instead of
being created, so an unmounted volume shows up immediately rather than as
an empty grid.

The image has no `HEALTHCHECK`: `distroless/static` ships no shell and no
`curl`, so probe `/healthz` from your compose file or orchestrator.

To run an unreleased revision, `docker build -t applibris .` and use
`applibris` in place of the `ghcr.io` reference. Without Docker, `make run`
starts the server against `./library` and `./data`.

### Put HTTPS in front of it

There is no login. The only thing stopping a web page you visit from
posting to your server is a header browsers send only over HTTPS (or to
`localhost`). Run the server behind an HTTPS gateway such as Tailscale
Serve, Caddy or your NAS's reverse proxy, and make the plain listener
reachable only by that gateway: bind it to `127.0.0.1` as above when the
proxy is on the same host, or leave the port unpublished on a shared Docker
network when the proxy is a sidecar.

By default the server refuses any state-changing request without that
header, so an accidentally exposed plain listener shows up the first time
you press Save, Send or Fetch metadata, with a log warning naming
`REQUIRE_FETCH_METADATA`.

### Importing a book

The Import page uploads one EPUB or FB2 at a time; dropping a file onto the
library page, or pasting a copied one there with Cmd/Ctrl+V, does the same.
A link to a book file works too: paste it on the library page, use its
Paste button, or type it into the Import page's link field, and the server
downloads it to the same preview. Only publicly reachable addresses are
downloaded, so a book on your own network is dropped or uploaded instead.

The file is held in `applibris-imports` under `TMPDIR` (`/tmp` in the
container) while you look at the preview, and is copied into the root of
`/library` under its own name only when you press Import. An unconfirmed
upload is discarded after thirty minutes, and the directory is emptied on
every restart. Several uploads may wait at once, so size `TMPDIR` for four
times `MAX_IMPORT_SIZE` (256 MiB at the default); `/tmp` in a container is
often a tmpfs in RAM. A `TMPDIR` the server cannot write disables importing
with a startup warning and changes nothing else, so a `--read-only`
container needs a tmpfs at `/tmp` or another `TMPDIR`.

The file's content decides how it is saved: an FB2 named `.epub` is saved
as `.fb2`, and the preview shows the name it will get. A file the library
already holds byte for byte is refused with a link to the existing book; a
different file with a title you already own is offered under a warning,
since a second edition is legitimate.

`MAX_IMPORT_SIZE` caps an upload or a download at 64 MiB by default.

### Sending to a Kindle

Resend must be able to send from `RESEND_FROM`, and that address must be on
your Amazon account's Approved Personal Document E-mail List. Resend caps a
message at 40 MB including the base64-encoded attachment, so books over
about 28 MB are refused before a send is attempted. Only EPUB is offered:
Amazon accepts FB2 attachments and silently drops them.

### On a NAS

A filesystem watch only sees changes made through the mount it watches. On
an Unraid user share, an NFS export or an SMB mount, files moved behind the
share (Unraid's mover, say) appear at the next rescan rather than within
seconds. For instant updates, bind-mount the underlying disk path instead
of the share. The server logs which filesystem it found at startup, warns
for the types above, and checks that events actually arrive.

## Configuration

Everything is an environment variable. Relative paths resolve from the
working directory, which is `/` in the container.

| Variable | Default | Meaning |
|---|---|---|
| `ADDR` | `:8080` | Address the HTTP server listens on. |
| `LIBRARY_DIR` | `/library` | Directory holding the books. Written only by the Import page, so it may be read-only if you do not want that, but it must already exist — it is never created. A symlink is followed; a dangling one fails startup. |
| `COVERS_DIR` | `/data/covers` | Where cover thumbnails are written. Created on first run, so its parent must be writable. Safe to delete. |
| `DB_PATH` | `/data/library.db` | SQLite database file. Created on first run, so its directory must be writable. |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` or `ERROR`. Logs go to stderr. |
| `SCAN_INTERVAL` | `1h` | How often the library is rescanned regardless of filesystem events. |
| `MISSING_GRACE` | `24h` | How long a file must stay missing before its record is removed. Must not be negative. |
| `MAX_IMPORT_SIZE` | `64MiB` | Largest file an import accepts, uploaded or downloaded from a link. A byte count, optionally suffixed `B`, `K`/`M`/`G` or `KB`/`MB`/`GB` (powers of ten), or `Ki`/`Mi`/`Gi` or `KiB`/`MiB`/`GiB` (powers of two), in any case. Must be positive. |
| `WATCH_ENABLED` | `true` | Watch the library directory for changes. `false` relies on the rescan alone. |
| `WATCH_SETTLE` | `5s` | How long the directory must be quiet after a change before a rescan runs. |
| `REQUIRE_FETCH_METADATA` | `true` | Refuse state-changing requests that carry no `Sec-Fetch-Site` header. `false` admits them and logs a warning instead. |
| `RESEND_API_KEY` | unset | Resend API key. Sending is disabled while unset. |
| `RESEND_FROM` | unset | Sender address for Kindle mail. Sending is disabled while unset. |
| `METADATA_PROVIDERS` | `openlibrary,googlebooks` | Providers to ask, in order. An unknown name fails startup. Set it empty (`METADATA_PROVIDERS=`) to disable enrichment; the server then makes no outbound request except to download a link you paste to import. |
| `GOOGLE_BOOKS_API_KEY` | unset | API key for Google Books. Without one the shared anonymous quota is used, which is routinely exhausted, so expect that provider to answer nothing. |

## Development

```sh
make test     # go test ./...
make build    # bin/server
make run      # go run ./cmd/server
```

Design notes live under `docs/notes/`, implementation plans under
`docs/plans/`, deferred work under `docs/backlog/`. `CLAUDE.md` maps the
code for anyone working on it.
