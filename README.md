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
There's no UI to trigger it yet, so this only configures what a future
enrichment run is allowed to do.

`METADATA_PROVIDERS` (default `openlibrary,googlebooks`) lists which
providers to use and in what order. Set it to an empty value
(`METADATA_PROVIDERS=`) to disable enrichment outright and make no
outbound requests at all — the setting for a fully offline or LAN-only
deployment. Google Books works anonymously at a low quota;
`GOOGLE_BOOKS_API_KEY` is optional and raises that quota when set.
