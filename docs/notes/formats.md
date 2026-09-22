# Book formats and covers

Rules for `internal/epub`, `internal/fb2` and `internal/cover`.

## Shared shape

- **Both format packages expose one `Metadata` field set and return the cover as raw bytes.** The scanner fills a book from either with one code shape.
- **A format package reads what the file says and leaves a field empty rather than substitute a value that answers a different question.**

## EPUB

- **Metadata comes from the OPF package document named by `META-INF/container.xml`.**
- **Publication date is the `dc:date` tagged `opf:event="publication"`, then the first event-less one. `creation` and `modification` are never used.** `published_date` means when this edition was published.
- **ISBN is recognised as `opf:scheme="ISBN"`, then `urn:isbn:`, then a bare ISBN-shaped identifier.**
- **Every reader of an ISBN calls `storage.NormalizeISBN`: EPUB's three branches, FB2's `<isbn>`, both providers' `ByISBN` and `bestISBN`. Never a private copy.** The value is the key every provider lookup is asked with, and nothing re-derives it once the field is filled.
- **`NormalizeISBN` returns the first ISBN-shaped run as bare digits with an upper-cased check digit and ignores the text around it.** That is safe only because every caller reads a slot that already claims to hold an ISBN.
- **The run is matched maximally, then validated.** A digit-led word after the number joins the run and the whole is refused: it loses an ISBN, never invents one. `docs/backlog/2026091005-isbn-run-absorbs-a-following-number.md` records the limit.
- **A scheme-marked identifier holding no run falls through to the next identifier.** Publishers write "Not available" under the scheme while the real number sits under `urn:isbn:`.
- **The bare branch carries its own guard, `bareISBN`: the whole identifier must be the run.** An identifier that claims nothing is evidence of nothing, and the shared function does not pay for that rule.
- **The check digit is never validated.** A malformed ISBN is still the best identifier the file offers, and a wrong one keys a lookup that answers no-match cleanly.
- **`dc:description` is flattened through `storage.PlainDescription` before it leaves the package.** It legally holds escaped HTML, and nothing downstream renders a description as markup.
- **`PlainDescription` is also `internal/googlebooks`' flattening, one derivation and never a private copy, and it decodes only terminated character references.** A bare `&` in prose must survive.
- **FB2 reaches plain text structurally, calls no flattening, and `annotationText` ends on `storage.CapBlankLines`, the call `PlainDescription` also ends on.** Its decoder drops inline markup and joins paragraphs, but a `<p>` keeps every source line break. Blank lines are the parser's business in both formats, so `internal/scanner` caps lengths and leaves shape alone.
- **The cover is the `properties="cover-image"` manifest item, falling back to EPUB 2's `<meta name="cover">`. The href is percent-decoded and its fragment stripped before the zip lookup.** A manifest href is a URI reference and a zip entry name is not.

## FB2

- **A `.fb2.zip` must hold exactly one `.fb2` entry.** Anything else is an error rather than a guess at which book the archive contains.
- **Plain and zipped both record `format` as `fb2`.** Packaging is not something the format badge surfaces.
- **Publication date prefers `publish-info/year` over `title-info/date`, and `date`'s `value` attribute over its text.** The specification makes the second the date the work was written.
- **Structured name parts are joined with single spaces into the one display name the `authors` table stores.**
- **The cover is the `<binary>` the coverpage's `l:href="#id"` names, base64-decoded.**
- **The declared encoding is honoured: a `CharsetReader` maps the label through `htmlindex`, and a label it does not know passes through unchanged.** `encoding/xml` refuses bytes that are not valid UTF-8 whatever `Strict` says, so a `windows-1251` document parses only because of this, and a parse error is one nothing re-asks.
- **A UTF-8 file that declares `windows-1251` becomes mojibake, and that is the accepted cost.** Mojibake is one edit from right where a parse failure is a book nobody finds, and a byte sniff cannot tell two plausible encodings apart.
- **Only the description is decoded through a struct. Binaries are walked token by token and every one but the coverpage's target is `Skip`ped.** A struct field holding every illustration costs the whole document's binaries to find one cover.
- **The walk stops after `<description>` when it names no cover, and after the cover's binary otherwise.** This relies on FB2's element order, so a binary before the description is never matched, and a document malformed past the point of interest still yields its metadata.

## Cover storage

- **`Store` resizes to about 400px on the long edge, never upscales, and writes a JPEG. Originals are not kept.** A full-resolution cover can always be extracted from the source file again.
- **The file is named by the book's content hash, not the thumbnail's bytes, so `internal/web` never serves `/covers/` as `immutable`.** A changed resize pipeline writes different bytes at the same path.
- **Writes go through a same-directory `os.CreateTemp` and an atomic rename, and the directory is created on demand.** A reader never observes a partial cover.
- **Decoders are registered for GIF, PNG, JPEG and WebP.** WebP is an EPUB 3.3 core media type.
- **Neither format reader decides that what a file calls a cover is an image. `Store` settles it.** `internal/epub` ignores the manifest item's `media-type` and `internal/fb2` the binary's `content-type`.
- **`ContentType` is the header read `Store` makes, `inspect`, exported. Any caller serving raw cover bytes must ask it.** One read means the two cannot disagree about what counts as an image. `internal/importer` is that caller; see `docs/notes/import.md`.
- **A refusal the bytes decide wraps `ErrUnsupportedCover`. A filesystem failure is unwrapped.** Storing the same bytes again fails identically for the first and may succeed for the second, so the scanner records no cover for one and retry for the other. Only `Store` knows which statement failed.

## Memory caps against untrusted files

- **Every zip entry and FB2 binary is read through a cap before its bytes are held. Never add an uncapped read in `internal/epub` or `internal/fb2`.** A small archive can inflate to gigabytes, and an image header can declare dimensions that allocate hundreds of megabytes before a pixel is decoded.
- **`cover.MaxCoverBytes` (8 MiB) is exported because both format packages apply it while extracting the cover, and `Store` re-checks it.** An oversized image is never read into memory, and is dropped like an unreadable one with the text metadata intact.
- **`maxPixels` (16 MP) is checked from the header alone, through `image.DecodeConfig`, before `image.Decode` allocates. It is sized off `image/jpeg`'s progressive-scan allocation, not pixels times 4. Do not raise it on the RGBA reasoning.** The progressive path allocates coefficient blocks for the whole declared image on the scan header, four to five times what RGBA arithmetic predicts.
- **EPUB entries are read through `readEntry`: `cover.MaxCoverBytes` for the cover, `maxPackageDocBytes` (4 MiB) for `container.xml` and the OPF. The declared uncompressed size is checked first and the bounded read behind it is defence in depth. An over-cap cover is dropped; an over-cap OPF is an error.** `archive/zip` bounds only the compressed bytes and deflate runs to about 1000:1, but it fails with `ErrFormat` as soon as a read passes the declared size, so an understating header cannot inflate past its own claim. Without the OPF there is no metadata to read.
- **FB2 cover base64 is accumulated whitespace-stripped under `maxCoverBase64Bytes`, checked as the buffer fills, with the decoded length checked exactly afterwards.** The constant is 4/3 of `cover.MaxCoverBytes` padded to a whole quantum so a cover of exactly the cap is admitted as `Store` admits it; one quantum encodes three byte lengths alike, so the exact check is separate.
- **`maxZipDocumentBytes` (128 MiB) caps the `.fb2` inside a `.fb2.zip`, on both sides of the charset decoder: `cappedReader` bounds the bytes read from the archive, `budgetReader` what the decoder produces.** `encoding/xml` buffers a whole text node before `Token` returns it, so this is in effect the largest single text node an archive may inflate to. A decoder only grows a byte count, so a cap on the read alone is two to three times looser than the figure it names.
- **The decoded side refuses a crossing read whole rather than trimming it.** A trimmed read cuts the output mid-rune, and `encoding/xml` reports invalid UTF-8 before it asks for the byte that carries the size refusal.
- **A cap reached past the description costs the cover only; inside the description it is an error. A plain `.fb2` has no such cap.** `errDocumentTooLarge` is told apart from a parse failure so the metadata already read survives, and a plain file already costs its own size on disk.
- **`enrich.MaxCoverBytes` (512 KiB) is a separate constant, never an alias of `cover.MaxCoverBytes`.** `internal/googlebooks`' cover-size choice is calibrated against exactly that figure. A downloaded cover is under `Store`'s cap by construction.
