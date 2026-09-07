# Step: a cover that can never decode is not retried every sweep

## Position in the sequence

Independent, with one adjacency: `2026090704-untrusted-ebook-memory-caps`
also touches `internal/cover.Store`'s error paths. Either order works;
land them with awareness of each other.

## Context

Found in the 2026-09-07 review. In `createBook`, any `cover.Store` error
sets `coverRetry = true`:

```go
p, err := cover.Store(coversDir, hash, meta.Cover)
if err != nil {
	slog.Warn("store cover failed", "path", path, "error", err)
	coverRetry = true
}
```

`cover_retry` was designed for a transient store failure (disk full, a
permissions hiccup). But `Store` also fails permanently: `internal/cover`
registers only GIF, PNG and JPEG, so a WebP cover (an EPUB 3.3 core media
type and increasingly common), an SVG or a BMP fails `image.DecodeConfig`
every time, and so does a corrupt image.

`maybeRegenerateCover` then skips the stat check while `CoverRetry` is
set, re-opens and fully parses the book on every sweep, fails again and
Warns again. Every fifteen minutes and on every watcher poke, for every
such book, forever. On a NAS that is sustained disk I/O and a log that
fills with the same line.

## Scope

In scope: distinguishing a decode failure from an I/O failure in `Store`,
and recording the former as "no usable embedded cover" rather than "retry
later"; adding WebP decoding, since it is the format most likely to
trigger this and the decoder is already in the module graph.

Out of scope, with reasons:

- **SVG covers.** No decoder in the standard library or `x/image`, and
  rasterising SVG is a dependency this project does not want for a
  thumbnail. An SVG cover is a permanent no-cover, honestly recorded.
- **Re-trying permanent failures when the decoder set grows.** A book
  recorded as having no usable cover would need a re-parse to discover a
  newly supported format. That is what deleting `COVERS_DIR` plus a
  one-time reset already provides, and CLAUDE.md documents that reset
  for `books_fts`. Not worth a mechanism.

## Decision 1: `Store` returns `ErrUnsupportedCover` for a decode failure

A sentinel wrapped into the error from both `DecodeConfig` and `Decode`
failing, and from the pixel and byte caps. Everything else (`MkdirAll`,
`CreateTemp`, `Encode`, `Rename`) stays an unwrapped I/O error. The
distinction is what the caller needs and only `Store` knows which
statement failed.

## Decision 2: the scanner records a permanent failure as an empty cover with no retry

In `createBook`, `errors.Is(err, cover.ErrUnsupportedCover)` leaves
`coverPath` empty and `coverRetry` false, logged at Info with the format
name from `DecodeConfig` when available. That is the same state a book
with no embedded cover reaches, and `maybeRegenerateCover`'s first guard
returns immediately for it on every later sweep. An I/O error keeps
today's `coverRetry = true`.

`maybeRegenerateCover`'s own `Store` call gets the same split: on
`ErrUnsupportedCover` it writes `cover_path = ""` with `cover_retry = 0`
through a storage method (`UpdateBookCoverPath` with an empty path is
the existing shape for this; confirm it clears the retry flag and the
provider row, or add a sibling) and logs at Info. Otherwise the Warn
stands.

## Decision 3: WebP is registered

`golang.org/x/image/webp` is in the same module `internal/cover` already
imports for `draw`, so registering it adds no dependency. It turns the
commonest instance of this bug into a working cover. Its `DecodeConfig`
reads the header only, so the `maxPixels` guard covers it the same way.

## Changes

- `internal/cover/cover.go`: `ErrUnsupportedCover`; wrapped at the decode
  and cap failures; `_ "golang.org/x/image/webp"` import.
- `internal/scanner/scanner.go`: `createBook` and `maybeRegenerateCover`
  split on the sentinel.
- `internal/storage`: confirm or add the "record no usable cover" write
  that clears `cover_retry` and the provider `field_sources` row together.

## Tests

- `internal/cover`: a WebP cover stores; a BMP header returns
  `ErrUnsupportedCover`; a cover over `maxPixels` returns it too; a
  write to an unwritable directory does not.
- `internal/scanner`: an EPUB with a BMP cover is indexed with
  `cover_path = ""` and `cover_retry = 0`; a second sweep does not
  re-parse it (assert via the log, or a counter on a test hook). An
  EPUB whose store fails on I/O (unwritable `COVERS_DIR`) still sets
  `cover_retry` and is retried once the directory is writable.

## CLAUDE.md

`internal/cover`'s paragraph gains the sentinel and WebP. The
`internal/scanner` cover paragraph's sentence "a separate `cover_retry`
marker records a transient initial store failure" gains "and only a
transient one: a decode failure is recorded as no cover."

## Verification

- Drop an EPUB with a WebP cover and one with an SVG cover into the
  library. After the first sweep the WebP book has a thumbnail; the SVG
  book shows "no cover" and the second and third sweeps log nothing
  about it.
