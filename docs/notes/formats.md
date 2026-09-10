# Book formats and covers

Rationale for `internal/epub`, `internal/fb2` and `internal/cover`. The
package map and the invariants that must hold are in CLAUDE.md.

Both format packages expose the same `Metadata` field set (title, authors,
language, ISBN, description, publisher, publication date) and return the
cover as raw bytes, so the scanner fills a book from either with one code
shape. Neither package decides what a field means downstream; they only
read what the file says, and leave a field empty rather than substitute a
value that answers a different question.

## EPUB

Metadata comes from the OPF package document named by
`META-INF/container.xml`.

**Publication date.** Among repeated `dc:date` elements the one tagged
`opf:event="publication"` wins, falling back to the first event-less one
(EPUB 3's form). `creation` and `modification` dates are never used:
`books.published_date` means when this edition was published, and a file's
creation date is not that.

**ISBN.** Recognised as `opf:scheme="ISBN"`, a `urn:isbn:` identifier, or a
bare ISBN-shaped identifier, in that order, and returned normalised (hyphens
and spaces stripped, prefix stripped, a trailing check digit upper-cased).
Normalising on the way in is what lets the value round-trip as the lookup
key the provider chain uses; `internal/storage` and the provider clients
apply the same normalisation to their own inputs.

**Cover.** EPUB 3's `properties="cover-image"` manifest item, falling back
to EPUB 2's `<meta name="cover">`. The href is percent-decoded and any
fragment stripped before the zip lookup, since a manifest href is a URI
reference and the zip entry name is not.

## FB2

FB2 is read for the same field set, from `<description>`, for plain `.fb2`
files and for `.fb2.zip` archives holding exactly one `.fb2` entry. Zero or
more than one entry is an error rather than a guess at which book the
archive contains. Both record `format` as `fb2`: how a book is packaged on
disk is not something the format badge should surface.

**Publication date** prefers `publish-info/year` over `title-info/date`.
The FB2 specification makes the first the edition's publication and the
second the date the work was written, and only the first is what
`books.published_date` means. `date`'s `value` attribute is preferred over
its text.

**Authors** arrive as structured `first-name`/`middle-name`/`last-name`
elements and are joined with single spaces into the one display name the
`authors` table stores. This is the one place FB2 offers more structure than
the schema keeps.

**Cover** is whichever `<binary>` the coverpage's namespaced `l:href="#id"`
points at, base64-decoded.

**Declared encoding is ignored.** `ReadMetadata` installs an
`xml.Decoder.CharsetReader` that passes every charset through unchanged.
The library's FB2 files are UTF-8 regardless of what they declare, and a
bare decoder fails outright on any declared encoding it does not recognise.
A file that really is in the encoding it declares fails to parse at all;
`docs/plans/2026091003-first-sweep-fidelity.md` changes this.

**Only the description is decoded through a struct.** `<binary>` elements
are walked token by token and every one but the coverpage's target is
`Skip`ped, because a struct field holding every illustration in the book
costs the whole document's binaries to find one cover. The walk stops as
soon as nothing more is wanted: after `<description>` when it names no
cover, after the cover's binary otherwise. This relies on FB2's element
order (`<description>` first, `<binary>` last); a binary placed before the
description is never matched, which is the same no-cover outcome a dangling
coverpage id gets. The upside is that a document malformed past the point
of interest still yields its metadata.

## Cover storage

`internal/cover.Store` turns raw cover bytes into the stored thumbnail:
resized to about 400px on the long edge (never upscaled), JPEG, written
into a derived directory at a name keyed by the book's content hash. A
thumbnail is 30–60 KB where an original is often 500 KB–2 MB, and a
full-resolution cover can always be extracted from the source file again,
so nothing is lost by not storing originals. The file's name being the
book's hash rather than a hash of the thumbnail bytes is why `internal/web`
does not serve covers as immutable: a changed resize pipeline can write
different bytes at the same path.

Writes go through a same-directory temporary file (`os.CreateTemp`) and an
atomic rename, so a reader never observes a partial cover. The directory is
created on demand.

Decoders are registered for GIF, PNG, JPEG and WebP. WebP is an EPUB 3.3
core media type and the format most likely to arrive embedded and otherwise
be undecodable.

**Two error classes, and the scanner depends on the split.** Every refusal
the bytes themselves decide (no registered decoder, a corrupt image, either
size cap) wraps `ErrUnsupportedCover`. A filesystem failure (`MkdirAll`,
`CreateTemp`, `Encode`, `Rename`) does not. Storing the same bytes again
fails identically for the first class and may succeed for the second, so
the scanner records a decode failure as "no cover" and an I/O failure as
"retry next sweep". Only `Store` knows which statement failed, which is why
the classification lives here.

## Memory caps against untrusted files

A book file is untrusted input: a small archive can inflate to gigabytes,
and an image header of a few dozen bytes can declare dimensions that
allocate hundreds of megabytes before a single pixel is decoded. Each cap
below bounds one of those paths, and each is applied before the bytes it
bounds are held.

**`cover.MaxCoverBytes` (8 MiB)** is the cap on a raw cover. It is exported
because `internal/epub` and `internal/fb2` apply it while *extracting* the
cover, so an oversized image is never read into memory in the first place.
`Store` re-checks it before reading anything. An over-cap cover is dropped
exactly like an unreadable one (a Debug line, text metadata intact).

**`maxPixels` (16 MP)** is checked from the image header alone
(`image.DecodeConfig`) before `image.Decode` allocates. It is sized off the
worst decoder, not off RGBA arithmetic. `image/jpeg`'s progressive path
allocates coefficient blocks for the whole declared image on the scan
header, before any entropy data: a header declaring 7000×7000 costs around
700 MB with three components and 934 MB with four, where 4 bytes × pixels
would predict 200 MB. At 16 MP the same headers cost 228 MB and 305 MB,
survivable once. Do not raise it back on the RGBA reasoning.

**EPUB zip entries** are read through `readEntry` with a cap:
`cover.MaxCoverBytes` for the cover, `maxPackageDocBytes` (4 MiB) for
`container.xml` and the OPF. `archive/zip` bounds only the compressed bytes
it reads, and deflate runs to about 1000:1, so a 521 KB EPUB can carry a
cover entry inflating to 512 MB. The declared `UncompressedSize64` is
checked first. It is attacker-controlled, but `archive/zip` fails with
`ErrFormat` as soon as a read passes the declared size, so a header that
understates the size cannot inflate past its own claim; a test rewrites a
central directory to pin that. The bounded read behind the pre-check is
defence in depth. An over-cap cover is dropped; an over-cap OPF is an error,
since without it there is no metadata to read.

**FB2 cover base64** is accumulated whitespace-stripped, byte by byte, with
`maxCoverBase64Bytes` (4/3 of `cover.MaxCoverBytes`, padded to a whole
quantum so a cover of exactly the cap is admitted as `Store` admits it)
checked as the buffer fills and the decoded length checked exactly
afterwards, since one base64 quantum encodes three byte lengths alike.

**`maxZipDocumentBytes` (128 MiB)** caps the `.fb2` inside a `.fb2.zip`,
through a `cappedReader`. The cover cap above bounds the copy this package
keeps, not the token itself: `encoding/xml` buffers a whole text node inside
the decoder before `Token` returns it, and nothing in the package caps that.
So this cap is in effect the largest single text node an archive may inflate
to; the tokeniser's buffer doubles as it grows, so the transient worst case
is about twice that, in line with the ~300 MB `maxPixels` already accepts.
Where the cap lands decides what survives it: past the description it costs
the cover only (`errDocumentTooLarge` is told apart from a parse failure),
inside the description there is nothing to keep and it is an error. A plain
`.fb2` has no such cap, since it already costs its own size on disk.

**`enrich.MaxCoverBytes` (512 KiB)** is deliberately a separate, smaller
constant rather than an alias of `cover.MaxCoverBytes`. It is a network
bound on a fetched cover, and `internal/googlebooks` chose which cover size
to request by measuring against exactly that figure, so raising it would
silently make that choice wrong. A downloaded cover is under `Store`'s cap
by construction.
