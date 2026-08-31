# Step: Harden cover regeneration after review

## Context

Review of the cover-regeneration implementation found three gaps: an initial
cover-store failure was indistinguishable from an EPUB with no embedded cover,
the unchanged-file path added a second database query per file, and every cover
stat error triggered expensive regeneration rather than only a confirmed
missing or zero-byte file.

## Changes

- Add `books.cover_retry` as a persistent marker for a cover that exists in the
  source but could not be stored during initial indexing
- Clear that marker when regeneration stores the cover and updates its path
- Join the owning book's cover fields into `FindFileByPath` so the unchanged
  path keeps one database round-trip
- Regenerate on `fs.ErrNotExist` or a zero-byte file and warn without parsing
  the source for other stat failures
- Cover transient initial-store recovery and non-`ENOENT` stat errors with
  scanner regressions

## Verification

- `go test ./...`
- `go vet ./...`
- `go build ./...`
