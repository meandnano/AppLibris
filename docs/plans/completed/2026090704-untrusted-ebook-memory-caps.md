# Step: bound what a single book file can make the process allocate

## Position in the sequence

Independent of the other plans, with one adjacency: it changes
`internal/cover.Store`'s limits, and `2026090711-permanent-cover-decode-
failures` changes what `Store`'s errors mean to the scanner. Either order
works, but whoever does the second should read the first.

## Context

Found in the 2026-09-07 review and reproduced with throwaway files. Three
independent paths let one file in the library allocate far more than the
file's size, none of them bounded:

| Input | Where | Allocation measured |
|---|---|---|
| 521 KB EPUB whose `cover-image` entry inflates to 512 MB of zeros | `internal/epub/epub.go`, `io.ReadAll(f)` on the cover entry | 1.2 GB |
| 1.4 MB `.fb2.zip` whose single entry carries 400 one-megabyte `<binary>` elements | `internal/fb2/fb2.go`, `Binary []binaryElement` decoded for the whole document | 800 MB |
| 126-byte progressive JPEG header declaring 7000×7000 | `internal/cover/cover.go`, under the 50 MP `maxPixels` | 700 MB |

The third is the interesting one because a guard exists and is sized
wrong. `maxPixels` assumes four bytes per pixel. Go's progressive JPEG
decoder allocates `mxx*myy*h*v` coefficient blocks of 256 bytes per
component on reading the scan header, before any entropy data arrives, so
a 49 MP declaration costs 700 MB at three components and about 1 GB at
four. A 16-bit RGBA PNG at 49 MP is 400 MB.

The failure compounds: a book whose cover fails to store never gets a
`cover_path`, so `maybeRegenerateCover` re-parses it on every sweep. In a
container with a memory limit that is an OOM kill every fifteen minutes,
and the restart policy makes it permanent. Nothing in the library has to
be hostile for the FB2 case: a heavily illustrated 300 MB FB2 is a
legitimate file that costs 600 MB of heap to read a title out of.

`archive/zip` bounds only the compressed section it reads. Deflate's
ratio is about 1000:1, so a byte cap on the archive is no cap on the
entry.

## Scope

In scope: a decompressed-size cap on every zip entry the EPUB reader
opens; decoding only the FB2 `<binary>` the coverpage names, with a cap;
a document-size cap on the `.fb2` inside a `.fb2.zip`; a `maxPixels`
sized off the worst decoder rather than the best.

Out of scope, with reasons:

- **A byte cap on the book file itself.** The scanner hashes files by
  streaming and never reads one whole; the parsers are the only place
  bytes accumulate. Capping the file would refuse a legitimate 500 MB FB2
  that these changes make cheap to read.
- **Running parsing in a subprocess or with a memory-limited goroutine.**
  Go offers neither cheaply, and the caps below make it unnecessary.
- **Registering WebP or other decoders.** A separate question; see
  `2026090711`.

## Decision 1: one exported constant for the cover byte cap, shared with `enrich`

`internal/enrich.MaxCoverBytes` (512 KiB) already bounds a provider's
cover before decoding, chosen because a cover past that size has no
thumbnail value. An embedded cover is the same object with the same use.
But `internal/cover` cannot import `internal/enrich` (the dependency runs
the other way), so the constant moves: `cover.MaxCoverBytes`, with
`enrich` referring to it. Its value can be more generous than 512 KiB for
embedded covers, since publishers embed 2 MB covers routinely and refusing
them costs a real cover. 8 MiB is the proposal: still two orders of
magnitude under the reproductions above, and above any cover a
publisher ships.

If raising the provider-side cap from 512 KiB is not wanted, keep two
constants with the reasoning for each; do not silently change the
provider one.

## Decision 2: EPUB entries are read through `io.LimitReader(f, cap+1)` and refused past it

For the cover entry the cap is `cover.MaxCoverBytes`. For `container.xml`
and the OPF a separate, smaller cap (4 MiB is far above any real package
document) applies the same way. Reading `cap+1` and checking the length
distinguishes "exactly at the cap" from "over it" without a second read.

Additionally check `File.UncompressedSize64` against the cap before
opening: it is attacker-controlled and so cannot replace the read-side
cap, but when it is honest it saves decompressing anything at all.

An over-cap cover is not an error for the book: `readCover` returns nil
with a Debug line, the same treatment an unreadable declared cover gets
today. An over-cap OPF is an error, since without it there is no metadata
to read.

## Decision 3: FB2 decodes only the binary the coverpage names

`fictionBook.Binary []binaryElement` goes. `readMetadata` decodes the
`<description>` element via the struct as today, then walks the remaining
token stream: for each `<binary>`, read its `id` attribute; if it is the
one the coverpage's `l:href` names, accumulate its character data through
a cap of `4/3 × cover.MaxCoverBytes` bytes of base64 and stop; otherwise
`decoder.Skip()`. Every other illustration in the book costs the tokeniser
a pass and no heap.

The coverpage href is known before the binaries because FB2 places
`<description>` first and `<binary>` last. A document that puts them the
other way round is invalid FB2 and gets no cover, the same as today's
behaviour for a coverpage pointing at a missing id.

For `.fb2.zip`, wrap the entry reader in a `LimitReader` with a generous
document cap (256 MiB) as a second line of defence. A real FB2 with
hundreds of megabytes of illustrations still parses, because Decision 3
means the tokeniser streams past them.

## Decision 4: `maxPixels` is sized off the progressive JPEG allocation

The bound that matters is `256 bytes × blocks × components` at scan-header
time, not `4 × pixels`. At 16 MP (a 4000×4000 scan, larger than any cover)
the worst case is a four-component progressive JPEG at about 330 MB,
which is survivable once and, with Decision 2's byte cap, cannot come from
a file over 8 MiB. Set `maxPixels` to 16 MP and rewrite its comment to
name the decoder that sets it, with the measured figures, so the next
person does not raise it back on the RGBA arithmetic.

## Changes

- `internal/cover/cover.go`: `MaxCoverBytes` exported; `Store` refuses
  `len(raw) > MaxCoverBytes` before `DecodeConfig`; `maxPixels` lowered
  and its comment rewritten.
- `internal/enrich`: `MaxCoverBytes` becomes a reference to
  `cover.MaxCoverBytes` (or stays a separate constant per Decision 1's
  fallback), with `FetchCover` unchanged in behaviour.
- `internal/epub/epub.go`: `LimitReader` on every `zr.Open` result;
  `UncompressedSize64` pre-check; over-cap cover returns nil.
- `internal/fb2/fb2.go`: `Binary` field removed; token walk for the named
  binary with a cap; `LimitReader` on the `.fb2.zip` entry.

## Tests

- `internal/epub`: an EPUB whose cover entry inflates past the cap
  returns metadata with `Cover == nil` and no error, and the test asserts
  peak allocation stays bounded (`testing.AllocsPerRun` is the wrong
  tool; read `runtime.MemStats` before and after, or simply assert the
  returned cover is nil and rely on the cap being read from the same
  constant). An OPF past its cap is an error.
- `internal/fb2`: a document with a large non-cover `<binary>` before the
  cover one parses with the right cover and `runtime.MemStats.TotalAlloc`
  growth under a small multiple of the cover's size. A cover binary past
  the cap yields `Cover == nil` and intact text metadata. The existing
  cover tests keep passing, which pins that the token walk finds the same
  binary the struct did.
- `internal/cover`: a JPEG header declaring dimensions over the new
  `maxPixels` is refused by `DecodeConfig`; a `raw` over `MaxCoverBytes`
  is refused before it. Both errors carry the limit in their text.
- `internal/enrich`: `FetchCover`'s existing over-cap test still passes
  against the shared constant.

## CLAUDE.md

`internal/epub` and `internal/fb2` paragraphs each gain a sentence on the
caps and the FB2 single-binary walk. `internal/cover`'s paragraph replaces
the "50 MP" figure and the RGBA reasoning with the progressive-JPEG
reasoning. `internal/enrich`'s `MaxCoverBytes` mention points at where the
constant now lives.

## Verification

- Build the three reproduction files (a zip-bomb cover EPUB, a
  many-binary `.fb2.zip`, a 126-byte progressive JPEG header wrapped in
  an EPUB) in a scratch library. Run the server under `GOMEMLIMIT` or a
  Docker `--memory=512m` limit. The sweep completes, each book is indexed
  with text metadata and no cover, and `docker stats` never shows the
  container approaching the limit.
- A real illustrated FB2 (tens of megabytes of `<binary>`) indexes with
  its cover and the process RSS after the sweep is close to its idle
  figure.
