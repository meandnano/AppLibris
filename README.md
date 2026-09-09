# library

A self-hosted ebook library server: browse a book collection and send titles
to a Kindle by email. See [DESIGN.md](https://github.com/meandnano/AppLibris/blob/init/DESIGN.md)
on the `init` branch for the full design, and [CLAUDE.md](CLAUDE.md) for
what's actually built so far.

## Search

The search box on the library page filters the grid as you type — there's
no separate results page. It searches title, authors, description and
ISBN, matching on word prefixes (typing "har pot" finds "Harry Potter"
while you're still typing, in either word order) and ignoring diacritics
in both directions (searching "tokarczuk" finds a stored "Tokarczuk", and
vice versa). Typing an ISBN finds the book regardless of whether it's
punctuated with hyphens or not.

Press `/` anywhere on the page to jump to the search box, and `clear ×`
inside it goes back to the unfiltered library. When a search matches, a
line above the grid says how many books of how many, and which fields
matched — so a hit on a description or an ISBN isn't a mystery.

Filtering is live via [htmx](https://htmx.org): each keystroke fires a
debounced request that swaps in just the matching grid, so the page never
does a full reload while you type. With JavaScript disabled, the same
search box still works as a plain form — submitting it reloads the page
with the results already filtered server-side, using the exact same
`?q=` URL the live version keeps in the address bar. That URL is
shareable and bookmarkable either way.

## Paging

The library grid loads 48 books at a time and appends the next batch as
you scroll. The count under the grid says how many are left.

With JavaScript disabled the same element is an ordinary link: following
it loads the next page as a whole page, and a "first page" link appears
beside the search box to get back. Each page's URL carries its own
position, so it can be bookmarked or shared like any other.

## Deployment requirement: an HTTPS front

There is no login. What keeps a page in your browser from sending your
books to an address of its choosing is a header browsers attach to every
request, `Sec-Fetch-Site`, which says whether the request came from this
site or another one. Browsers attach it **only when the page is served
over HTTPS** (or from `localhost`). Over plain `http://` on a LAN or
tailnet address it is simply absent, and the server cannot tell a
same-site request from a cross-site one.

So the service has to be run in two parts, and both matter:

1. **An HTTPS gateway in front.** Tailscale Serve, Caddy, or whatever
   reverse proxy your NAS already has. The server's own redirects are
   relative, so it does not care what scheme it is fronted by.
2. **The plain listener reachable only by that gateway.** With the proxy on
   the same host, `ADDR=127.0.0.1:8080`. With the proxy in a sidecar
   container, put both on a shared Docker network and do not publish the
   server's port. A port published on the LAN beside an HTTPS front is the
   requirement half-met, which is the same as unmet.

By default (`REQUIRE_FETCH_METADATA=true`) the server refuses any
state-changing request that arrives without the header, so a listener that
is accidentally reachable over plain HTTP shows up the first time someone
presses Save, Send or Fetch metadata: a red "Refused" line appears above
the control, and the log carries a warning naming this setting. Set
`REQUIRE_FETCH_METADATA=false` to admit such requests anyway; the server
then logs one warning at startup and one on the first such request, and
cross-site protection for them rests entirely on part 2 above.

## Live updates

A book dropped into the library directory normally appears within a few
seconds: the server watches the directory and rescans shortly after things
go quiet. A periodic rescan (`SCAN_INTERVAL`, default 15 minutes) runs
regardless, so nothing depends on the watch working — at worst a new book
takes that long to show up.

That distinction matters on a NAS. On an Unraid **user share**
(`/mnt/user/...`), an NFS export or an SMB mount, the filesystem is a view
over storage that other things can write to directly, and only changes
made *through* the share generate events. Copying a book to the share over
SMB is seen; Unraid's mover shuffling files between the cache pool and the
array is not, because it works on `/mnt/cache` and `/mnt/diskN` behind the
share's back. Those changes still appear at the next rescan.

For instant updates either way, bind-mount the underlying disk path
(`/mnt/cache/books`, `/mnt/diskN/books`) rather than the user share. The
server logs which filesystem it found at startup and warns when it is one
of the types above; it then creates a short-lived probe file to check
whether events actually arrive, and says so if they don't. Set
`WATCH_ENABLED=false` to turn the watch off and rely on the rescan alone,
or `WATCH_SETTLE` (default `5s`) to change how long the directory must be
quiet before a rescan runs.

## Metadata enrichment

The server can fill in missing book metadata and fetch a cover, from Open
Library and Google Books, for whichever fields a book's own file didn't
provide — never overwriting an embedded value or one edited by hand.
Each book's detail page has a "Fetch metadata" button that queues the
work and reports what it filled in; a field a provider supplied is marked
with its source, so you can tell a guess from what the file itself said.
Nothing is enriched automatically — a run is always something you asked
for, on a book you chose. A run is reported as failed rather than as
nothing found when no provider could answer, and when the only thing it
found was a cover it could not save — a throttled API, a broken image and
an unknown book are three different answers, and only the last one means
there is nothing there.

A book with no ISBN is looked up by title and author instead, and an
answer that doesn't plausibly match the book is discarded rather than
written — so a file named `01 - Fellowship` reports "nothing to add"
instead of acquiring some other book's publisher and cover. That is the
common outcome for files whose titles came from their filenames, and it is
the intended one: an empty field can still be filled by hand or by a later
run, where a wrong one is recorded as though it were known.

Covers fetched this way live in `COVERS_DIR` alongside the ones read out
of book files, and that directory stays safe to delete: the next scan
rebuilds a cover it can re-extract from the book itself, and simply forgets
one a provider supplied — the book shows an empty cover again, and "Fetch
metadata" puts it back.

`METADATA_PROVIDERS` (default `openlibrary,googlebooks`) lists which
providers to use and in what order. Set it to an empty value
(`METADATA_PROVIDERS=`) to disable enrichment outright and make no
outbound requests at all — the setting for a deployment with no internet
access (it does not change the HTTPS requirement above, which is about how
*you* reach the server, not how it reaches anything else). Google Books
nominally works anonymously at a low
quota, but in practice expect to need `GOOGLE_BOOKS_API_KEY`: unauthenticated
requests share one Google-wide project whose daily quota was found exhausted
on every attempt, days apart, answering `429` rather than results. Enrichment
degrades quietly when that happens — the provider is skipped and the other
one still answers — so a keyless setup looks like it works and simply finds
less.
