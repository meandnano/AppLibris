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

## Live updates

A book dropped into the library directory normally appears within a few
seconds: the server watches the directory and rescans shortly after things
go quiet. It also scans once at startup, and those two together are the
default arrangement — there is no periodic rescan unless you ask for one.

That distinction matters on a NAS. On an Unraid **user share**
(`/mnt/user/...`), an NFS export or an SMB mount, the filesystem is a view
over storage that other things can write to directly, and only changes
made *through* the share generate events. Copying a book to the share over
SMB is seen; Unraid's mover shuffling files between the cache pool and the
array is not, because it works on `/mnt/cache` and `/mnt/diskN` behind the
share's back. Those changes go unnoticed until something else
provokes a scan.

That is what `SCAN_INTERVAL` is for: set it to a duration (`15m`, `1h`) and
the server also sweeps on that timer, so a library the watch cannot see
still catches up on its own. It defaults to `0`, meaning no timer — a
rescan re-walks and re-stats every file to usually find nothing, which on a
NAS keeps disks spun up for no result.

For instant updates either way, bind-mount the underlying disk path
(`/mnt/cache/books`, `/mnt/diskN/books`) rather than the user share. The
server logs which filesystem it found at startup and warns when it is one
of the types above; it then creates a short-lived probe file to check
whether events actually arrive, and says so if they don't. Set
`WATCH_ENABLED=false` to turn the watch off and rely on `SCAN_INTERVAL`
alone — set one if you do, or nothing after the startup scan will ever
notice a change — or `WATCH_SETTLE` (default `5s`) to change how long the
directory must be quiet before a rescan runs.

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
outbound requests at all — the setting for a fully offline or LAN-only
deployment. Google Books works anonymously at a low quota;
`GOOGLE_BOOKS_API_KEY` is optional and raises that quota when set.
