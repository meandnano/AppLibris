# Import

Rules for `internal/importer`, `scanner.IndexFile`, the import surface of `internal/service` and the `/import` routes, plus the parts of `cmd/server` that decide whether importing is offered.

## Where an import lands

- **An import lands in the library root and is a library file from that moment: the scanner owns it.** The library directory is the one source of truth, so a file written anywhere else is a book the directory does not have.
- **The flat root, never a subdirectory.** The library is a flat unorganised pile by design (`docs/notes/design.md`).
- **A read-only `LIBRARY_DIR` is a legitimate configuration.** Importing is offered or explained at startup, never a startup failure.

## Staging

- **A file is staged outside the library, parsed there and shown with its verdict before anything is written into the library.** Only a person with the book in front of them can say whether it is the one they meant.
- **Staged state is in memory and on `os.TempDir()/applibris-imports`. Nothing about a stage is written to the database, and `importer.New` wipes the directory.** A stage nobody confirmed before a restart is a file nobody asked for.
- **The staging directory is separate from `/data`.** It holds nothing worth backing up.
- **Stages expire after `StageTTL` (30 minutes), swept by a janitor and rechecked on confirm. The TTL is not configurable.** Nothing about a deployment changes how long a person takes to press a button.
- **The janitor runs on the scan context and exists only when the importer does.**
- **A swept staged file reads as `ErrExpired`, never as a raw `fs.ErrNotExist`.** Expiry is checked under the lock and the file opened after it is dropped.
- **The stage id is 128 random bits.** It keeps two people's stages apart; it is not a credential, since every state-changing route is same-site-only.

## What may be staged at once

- **Staging is bounded in total bytes, `stagingBudgetFactor` times the import cap, never in stages; past it an upload is refused with `ErrStagingFull`.** `os.TempDir()` is tmpfs in a container, so what is held is RAM.
- **The reservation is taken at the cap before the copy and corrected afterwards to what the stage retains.** A check made after the copy admits everything and reports later.
- **The charge is the file, the cover kept for the preview and the metadata, never the file alone.** A small archive can hold a `cover.MaxCoverBytes` cover.
- **`Stage` cuts the offered name to `maxOfferedNameBytes` on a rune boundary before anything else.** The name is charged only after the reservation, and a `Content-Disposition` may run to the transport's 10 MiB header limit, which would overshoot the budget and lock imports out until the stage expired.
- **The reservation is released by every path that drops a staged file: discard, expiry, confirm, and the duplicate verdict.** The duplicate releases the file's share and keeps charging the cover its page still renders.

## Format by content

- **`detectSuffix` decides what a file is from its bytes. The client's filename and `Content-Type` never choose the parser or the suffix written.** Browsers send `application/octet-stream` for an FB2, and people rename files.
- **A zip with `META-INF/container.xml` is an EPUB; one with exactly one `.fb2` entry and no container is an `.fb2.zip`. `archive/zip` inflates nothing.**
- **A plain file is an `.fb2` only when it opens with `<?xml` or `<FictionBook` and `<FictionBook` also appears in the 4 KiB sniff window, searched, not prefix-matched.** A declaration says a file is XML and not which XML, so the opening alone writes any XML document into the library as a `.fb2`; a real FB2 carries a declaration and sometimes a DOCTYPE ahead of the root.
- **The staged file is named `<id><suffix>`, and the library name takes the same suffix.** The format readers are picked by suffix, and the staged file must parse as the library file will.
- **The preview parse is the scanner's own, `ExtractMetadata`, with the scanner's caps.** What is stored comes from indexing; the preview only has something to show.
- **The preview is capped through `storage.CapField` (`capMetadata`) before it is built.** It shows what would be stored, and the title-match verdict compares a capped title against the capped `sort_title` the scanner derived.

## A cover is not an image until something says so

- **The preview serves its cover from the stage. A staged book acquires no entry in `COVERS_DIR` before anybody says to keep it.**
- **A staged cover is served with the media type `cover.ContentType` decided from its header, never `http.DetectContentType`. A cover that fails is dropped, so `HasCover` means "a cover this app would keep".** No format reader checks that what a file calls a cover is an image, so sniffing lets an upload choose the type served from this origin.
- **Every route serving bytes rather than a rendered template sets `X-Content-Type-Options: nosniff`: `/import/{id}/cover`, `/covers/` and `/static/`.** One rule about media types is easier to hold than a per-route judgment.

## The three verdicts

- **`Stage` looks the content hash up and the preview carries one of three verdicts.** `new`: Import is offered. `exists`, byte-identical content the library holds: only Discard and a link to the book; the staged file is deleted at once and the record survives so the page renders. `title-match`, different content under a `sort_title` the library holds: Import under a warning, since a second edition is a legitimate thing to own.
- **Confirm does not re-check a `new` or `title-match` verdict: it copies, and `IndexFile`'s answer is the truth.** A sweep can index the same bytes between preview and confirm, so landing on a known book is a second location, not an error. `exists` alone decides anything at confirm, because reaching it deleted the staged file.
- **Nothing beyond a title match is compared.** Near-duplicate guessing belongs to the suggestion feature `docs/notes/design.md` defers.

## Confirm writes in an order that cannot half-fail

- **Name.** The offered base name is stripped of control characters, `/`, `\`, `:`, the six characters Windows refuses and leading dots, collapsed onto single spaces and cut to `maxStemBytes` on a rune boundary; the sniffed suffix replaces the extension, the staged title and then the stage id stand in when nothing survives, and a taken name gets ` (2)`, ` (3)` before the suffix. A mounted library is read from Windows often enough not to create a name it cannot open.
- **Copy.** The bytes go to `<name>.part`, opened `O_CREATE|O_EXCL`, then `Sync`. Nothing creates a supported suffix in the library directly, and a sweep ignores `.part`, so a half-written file is never indexed. A copy and not a rename, because `/tmp` and `/library` are different filesystems.
- **Publish with `os.Link` onto the first free name, then unlink the part. Never swap it back to a rename.** The link fails with `EEXIST` where `os.Rename` silently replaces whatever a person dropped at that name during the copy. A taken name costs one more link from the same part, so the part name and the published name are chosen separately.
- **A filesystem without hard links falls back to `Lstat` then `Rename`, logged once, and the window stays open there.** Refusing to import would break a working deployment over a race that opens only when something else writes the same name mid-copy.
- **Index through `scanner.IndexFile` in the confirming request.** The response redirects to the book.
- **The index write runs on `context.WithoutCancel`.** The library owns the bytes before it starts, the same rule `internal/sender` applies once Resend has accepted a message.
- **A failed `IndexFile` leaves the library file in place.** Deleting it would discard bytes over a database error; the next sweep is the recovery.
- **Clean up removes the staged file and its cover; the record stays, carrying the book id, until it expires.** A repeated confirm answers the first call's book id rather than copying the file in again.
- **A `.part` left in the library by a crash is not removed at startup.** `.part` is the suffix downloaders use, and startup cannot know the file is this app's.

## Indexing through the scanner

- **`IndexFile` is `scanFile` with a fresh `Result`, returning the book id. There is no second way into the index.** Every guard a new book needs lives in `createBook`, and a second entry point is a second place for each to drift.
- **Indexing in the request is safe because `scanFile` is idempotent and keys on the content hash.** A sweep and a confirm that race converge on one book with one location.
- **`internal/importer` imports `internal/scanner`, so `internal/scanner`'s in-package tests cannot import `internal/service`.** `capmetadata_test.go` is `package scanner_test` over `export_test.go` for that reason.

## Writability is probed once, at startup

- **`probeWritable` removes `LIBRARY_DIR/.applibris-write-probe` before creating it, and the create keeps `O_EXCL`.** The create refuses a taken name, so a probe left by a crash would otherwise disable importing for good; `O_EXCL` keeps it from following a symlink left at that name.
- **A probe, not a look at the mode bits.** A read-only mount, an ACL and a uid mismatch all fail at the same call and none shows in the mode.
- **Once at startup, never per request.** A per-request check would differ only when confirm is about to report its own error.
- **Failure logs at Warn naming the uid the process runs as and the uid that owns the directory, the same two facts `mkdirError` names.**
- **The probe name begins with a dot and carries no supported suffix.** A sweep walks past it.
- **A `Stager` exists exactly when importing is available; there is no disabled `Stager` and never a second disabled state inside the importer.** `newStager` builds one only when the probe and the staging directory both succeed; either failing is a Warn, never a startup failure, since a readable library is still worth serving. `internal/service` answers a nil one with `ErrImportDisabled`, the convention `Notify` follows.
- **A staging directory that cannot be created disables importing at Warn naming the directory.** A container run `--read-only` with no tmpfs at `/tmp` has none to give.
- **Whether the nav offers Import comes from `svc.ImportEnabled()`, not a flag beside `sendEnabled` and `enrichEnabled`.** Those two are configuration the service never sees; the importer is its own.
- **The routes stay registered when importing is off; only the nav link is withheld.** A tab open since before a restart gets an explanation rather than a 404.
- **`cmd/server` parses `MAX_IMPORT_SIZE` through `parseByteSize`. Zero and negative are refused, never read as "no limit".** A deployment that means to disable importing takes away write access, which is what the app checks.

## The upload route

- **The upload handler extends its own deadlines through `http.NewResponseController`, sized from the cap at a floor of `uploadRate` (1 MiB/s); every other route keeps the server's 30-second `ReadTimeout`.** A large upload routinely outlasts the global timeout, and a stalled one still ends. A server without the control is left alone.
- **The body is wrapped in `http.MaxBytesReader` at the cap plus multipart overhead, before the read-only check.** That refusal answers a request whose body is still arriving, and the drain that lets the answer be read needs a limit. The importer's own count of the part is the number a refusal names.
- **The body streams through `r.MultipartReader`, never `ParseMultipartForm`.** The latter spools the whole file to a second temporary copy first.
- **Every refused upload drains what is left of the body before rendering.** It is hygiene rather than a cure: the copy stops at the cap plus one byte, so a body far past the limit is the one case whose refusal can be lost.

## Dropping a file on the library page

- **A drop is the upload form's own post: `drop.js` fills the hidden `drop__form` and calls `form.submit()`. Never a fetch or an htmx request, and the form never gains `hx-post`.** The 303 to the preview and the 422 page are what the drop relies on.
- **The script refuses several files, a folder, and a file over the cap before a byte is sent. The server's count remains the check; the filename is not examined.** A stage holds one book, an oversize body's refusal can be lost in flight, and `detectSuffix` already says what a file is not.
- **Every sentence the drop shows, and the cap the script checks against, come from the page; the too-large sentence is composed through `importFailureLine`.** A drop stopped early reads exactly as one the importer stopped.
- **`import-drop` is included only from `library.html`, never from `book-grid`, and only when a `Stager` exists.** A search swaps `book-grid` in, and on a book's own page a drop would read as replacing that book's file.
- **A refusal is pinned to the viewport where the overlay was, and the live region is the `drop__errors` wrapper, which is never hidden.** No page arrives to carry the refusal, and a live region toggled from `hidden` is not reliably announced where a change inside a standing one is.
- **`uploading` is cleared by a bfcache `pageshow` and by Escape.** A cancelled submit aborts the navigation without unloading the document, so no `pageshow` follows it.

## Importing from a link

- **`POST /import/url` downloads inside the request and answers a 303 to the preview or the upload's 422 page; from the preview on, a link is an upload.** It needs no stage state of its own: from the 303 on, the upload's preview, confirm and discard apply unchanged.
- **The body is one more reader for `Stage`, counted once by its copy at the cap plus one byte; `Fetch` refuses only a status other than 200 and a declared length past the cap, before any body byte is read.** One count is one cap, and the transport decompresses gzip before it, so a compressed bomb meets the same limit.
- **Only public addresses are fetched: `importer.Fetcher` dials through `netguard.DialContext` with `netguard.RefusePrivateAddress`, on every hop, and there is no allowlist.** The app has no login, so anything that reaches it could otherwise make it fetch a Docker neighbour, a tailnet peer or a metadata endpoint; a book on the LAN is dropped or uploaded instead.
- **The transport's `Proxy` is nil and `DialTLSContext` stays unset.** Through a proxy the guard checks the proxy's address, and a TLS dialer of its own bypasses `DialContext`.
- **A redirect is followed to any host for at most five hops, `http` or `https` only, with `Referer` deleted from each hop.** Download links bounce to a CDN and the guard judges every hop's address anyway; the previous URL can carry a signed token.
- **`service.StageURL` refuses a link past 2 KiB, one not `http` or `https`, one with no host and one carrying userinfo as `ErrUnsupportedLink`, before anything is fetched, and the fetcher keeps no cookie jar.** A link that needs a session is not a direct link to a file.
- **`Download.Name` is `Content-Disposition`'s filename, else the final URL's last path segment, else empty, unsanitised.** `Stage`'s name derivation is the one sanitiser.
- **`internal/service` holds a `LinkFetcher` interface, `*importer.Fetcher` its one implementation, set by `WithFetcher`; a missing `Stager` or fetcher answers `ErrImportDisabled`.** The guarded transport can be swapped only inside `internal/importer`, so the service and web tests stand in a fetcher on the in-memory network.
- **One download runs at a time: a one-slot channel taken without waiting, a second link refused with `ErrDownloadBusy`.** The staging budget bounds bytes at rest, not outbound connections held open, and a queued request would wait behind a download of unknown length.
- **The handler's window is `downloadWindow`: `importer.FetchStartTimeout` plus `uploadWindow` of the cap, carried by the request's context.** `FetchStartTimeout` is the fetcher's dial, TLS and header timeouts summed, so the handler never abandons a download the fetcher is still allowed to be starting.
- **The link handler extends only its write deadline, after the form is read.** A form trickled in gets no longer than any other request's body, and Go's server clears the read deadline itself once the body is read.
- **A failure to fetch or to read the body wraps `service.ErrDownloadFailed` with the fetch error as text only, plus the context's cause when the request's context ended.** A transport's dial, TLS and header timeouts match `context.DeadlineExceeded`, and would otherwise read as the handler's window running out.
- **`Stage` runs on `context.WithoutCancel` of the request's context; the body alone is bound to the deadline, through the request that fetched it.** The window is sized for the download, and a deadline passing during the parse or the duplicate lookup must not discard a book that arrived whole.
- **A `Stage` failure after the context ended is reported as a download failure carrying the context's cause.** A body cut short by the deadline can read as a clean end, and the truncated bytes would otherwise be blamed as not a book.
- **A refused content whose response was `text/html` wraps `ErrWebPage`.** Pasting the page a download button sits on is the usual mistake, and the line says so.
- **Every named cause has its own line through `importFailureLine`; `ErrDownloadFailed`, a refused private address included, is "Couldn't download that link."; any other error is a 500 logged at Error, as an upload's is.** A line of its own for the refusal would confirm that an internal hostname resolves.
- **A non-200 answer names its status code.** Only public hosts are ever reached, so the code reveals nothing internal.
- **A failure is logged at Info with the link's scheme, host and path only, and `ErrDownloadFailed` keeps a `*url.Error` as its `Op` and inner error with every quoted string blanked.** The query and fragment routinely carry a signed token and the userinfo a password, net/http quotes the full URL and any unparseable `Location` a host sent into its errors, and the log outlives both.
- **The Import page's link form is a plain post with no htmx setting, and a refused link is rendered back into it with no `maxlength`.** Only a whole-page navigation puts the preview's URL in the address bar, and a truncated link fetches the wrong file.

## Pasting on the library page

- **A paste whose target is an input, textarea or contenteditable element is left alone.** The search box pastes normally.
- **A pasted file goes to the drop's own `submitFiles`, exposed on `window.importDrop`, with no dialog.** Both gestures keep one set of refusals and sentences, and the preview is the confirmation.
- **A pasted file is handed on unless its type is `image/*`; an image paste is read as text.** A copied image rides on the clipboard as a file and must not navigate to a refusal, while `detectSuffix` already decides what any other file is, an FB2 archive named `.zip` included.
- **Pasted text opens the `paste-link` dialog only when, trimmed, it is one token `new URL()` parses as `http:` or `https:`; anything else is ignored silently.** A stray paste on the page must not become a question.
- **The dialog holds a real form posted natively to `/import/url`, never a fetch or htmx.** The redirect and the 422 page are the link form's own.
- **The Paste button reads text only, through `navigator.clipboard.readText()`; denied access or no link opens the dialog empty. It renders `hidden` and the script reveals it.** The clipboard API never reads a file, and with JavaScript off the Import page's link form is the way in.
- **While a link hands off, `paste-link.js` holds the drop's `uploading` flag, so drops and pastes are refused; a bfcache `pageshow`, the dialog's `cancel` event and its Cancel button release it.** Two submits race to navigate the tab.
- **The Cancel button and the dialog's `cancel` event share one abort that calls `window.stop()` while the dialog shows a download, tested by its own downloading line rather than `drop.busy()`.** Dismissing the dialog must not leave a navigation that lands on the preview, and drop.js lets go of its hold on the Escape keydown before `cancel` fires.
- **The button shows busy for a link handoff only.** A file, pasted or dropped, is the drop's upload, and its overlay says so.
- **`paste-link` is included only from `library.html` beside `import-drop`; `paste-link.js` loads from `site-scripts` after `drop.js` and returns at once without the dialog or `window.importDrop`.** It hands files to the drop's script and has nothing to hand them to elsewhere.

## Deliberately absent

- **A programmatic API.** Every state-changing route refuses a request carrying no `Sec-Fetch-Site`, which a non-browser client never sends; see `docs/notes/design.md`.
- **Deleting a book from the UI.** Discard removes a staged file only.
- **Editing metadata in the preview.** The detail page does that, and the redirect lands there.
