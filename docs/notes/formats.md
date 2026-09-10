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
bare ISBN-shaped identifier, in that order. Every branch derives its value
through `storage.NormalizeISBN`, which is the one derivation every reader of
an ISBN calls — these three branches, `internal/fb2`'s `<isbn>`, and both
providers on the way into a lookup. It lives below all of them because the
value is the lookup key the whole provider chain is asked with, it is what
the detail page shows, and nothing re-derives it once the field is filled:
`ISBN 978-0-00-000000-0 (ebook)` stored as written is a lookup nobody
answers.

It returns the first ISBN-shaped run in its input as bare digits with an
upper-cased check digit. An `ISBN` or `urn:isbn:` marker is stripped first,
groups may be separated by single hyphens or spaces, and a trailing `X`
counts only as an ISBN-10's tenth character. Text around the run is ignored,
so `ISBN 978-0-00-000000-0 (ebook)` and the bare `0306406152 (pbk.)` both
yield their digits.

Ignoring the surroundings is safe because every caller reads a slot that
already claims to hold an ISBN — an `opf:scheme="ISBN"` or `urn:isbn:`
identifier, FB2's `<isbn>`, a provider's ISBN array — so a run found there is
an ISBN by declaration and needs no corroborating shape. A run is matched
maximally over digits, separators and `X` and validated afterwards, which is
what makes `030640615X7` one refused eleven-character run rather than a valid
ISBN-10 with a stray digit after it. A space is one of those run bytes, so a
digit-led word after the number (`978-0-306-40615-7 2nd ed.`) joins the run
and the whole thing is refused: it loses an ISBN, never invents one, and
`docs/backlog/2026091005-isbn-run-absorbs-a-following-number.md` records it.

An `opf:scheme="ISBN"` value holding no ISBN-shaped run at all — "Not
available" is what publishers write — falls through to the next identifier
rather than ending the search, so the real number under a `urn:isbn:`
identifier is still found. The bare branch adds the one rule the shared
function deliberately lacks: a scheme-less identifier must be *wholly* an
ISBN, since an identifier that claims nothing is evidence of nothing and a
ten-digit run in a sentence is as likely an LCCN or a catalogue number.

The check digit is never validated: a malformed ISBN in a file is still the
best identifier it offers, and a wrong check digit still keys a provider
lookup that answers no-match cleanly.

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

**The declared encoding is honoured.** `ReadMetadata` installs an
`xml.Decoder.CharsetReader` that maps the label through
`golang.org/x/text/encoding/htmlindex` and wraps the document in that
encoding's decoder. `encoding/xml` refuses bytes that are not valid UTF-8
whatever `Strict` says, so an honestly labelled `windows-1251` document —
which most of a legacy Russian collection is — parses only because of this,
and would otherwise fail with `invalid UTF-8` and land under its filename.
A parse error is one nothing re-asks.

A label `htmlindex` does not know is passed through unchanged. A UTF-8 file
that declares `windows-1251` is the other cost: its bytes are read through
the cp1251 table and its title becomes mojibake rather than the parse
failing. That is the trade — the cost falls on a file that misdescribes
itself, the benefit on every file that does not, and mojibake is one edit
away from right where a parse failure is a book nobody finds.
A byte-content sniff that ignored the label would not help: two encodings
that are both plausible for a run of high bytes cannot be told apart by
sniffing, and the label is the one piece of evidence the file offers.

The `.fb2.zip` cap is applied on both sides of the decoder, since a decoder
only grows a byte count and the cap has to mean the same number either way.
See **Memory caps against untrusted files** below.

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

**`maxZipDocumentBytes` (128 MiB)** caps the `.fb2` inside a `.fb2.zip`.
The cover cap above bounds the copy this package keeps, not the token
itself: `encoding/xml` buffers a whole text node inside the decoder before
`Token` returns it, and nothing in the package caps that. So this cap is in
effect the largest single text node an archive may inflate to; the
tokeniser's buffer doubles as it grows, so the transient worst case is about
twice that, in line with the ~300 MB `maxPixels` already accepts.

It is applied on **both sides of the charset decoder**, and needs to be. A
`cappedReader` bounds the bytes read out of the archive; a `budgetReader`
bounds what the decoder produces from them. One alone is not the figure the
constant names, because a decoder only ever grows a byte count — every
cp1251 Cyrillic byte becomes two of UTF-8, a legacy CJK encoding's byte can
become three — so an archive capped only on the read would hold two to three
times the budget. The decoded side refuses a crossing read whole rather than
trimming it to what is left: a trimmed read cuts the decoder's output
mid-rune, and `encoding/xml` reports invalid UTF-8 for that before it ever
asks for the byte that would carry the size refusal.
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
