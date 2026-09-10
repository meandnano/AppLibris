# Step: the first sweep against a real NAS library

## Position in the sequence

Independent of the enrichment-hardening and library-on-disk plans written
beside it. `cmd/server`, `internal/fb2`, `internal/epub`, `internal/storage`,
`internal/scanner`, with small follow-on edits in `internal/service`,
`internal/enrich`, `internal/openlibrary` and `internal/googlebooks`.

This plan replaces four backlog items, deleted in the same change:
`2026090722-container-uid-volume-permissions`,
`2026090713-fb2-declared-charset-is-ignored`,
`2026090719-epub-isbn-identifier-unvalidated` and
`2026090720-embedded-metadata-bypasses-length-caps`. Their re-validation
against the code is in Context.

> **Correction, found while implementing.** The four files were already
> deleted, in the same change that wrote this plan and the two beside it,
> so there is nothing to delete here.

## Context

Everything here is what a person meets on the first run against a library
they did not build for this project. Four things go wrong, in the order
they are met.

**The container cannot start.** `run` resolves `LIBRARY_DIR`, `COVERS_DIR`
and `DB_PATH`'s directory through one helper (`cmd/server/main.go:109`,
`:113`, `:122`), and `resolveDir` (`main.go:363`) calls
`os.MkdirAll(dir, 0o755)` on each. The image runs as uid 65532 with
`WORKDIR /`; a NAS bind mount is owned by the share's user, Unraid's by
`nobody`, a fresh named volume by root. The failure is
`create covers directory /data/covers: mkdir /data/covers: permission denied`
and nothing names the uid on either side. The library directory is the odd
one out: the scanner only reads it, so creating it is the one call that
turns a legitimately read-only mount into a startup failure when the path
does not yet exist.

**Every cp1251 FB2 lands under its filename.** `readMetadata`
(`internal/fb2/fb2.go:250`) installs a `CharsetReader` that returns the
input unchanged for any label. `encoding/xml` rejects bytes that are not
valid UTF-8 regardless of `Strict`, so an honestly labelled
`encoding="windows-1251"` document fails with `XML syntax error on line 1:
invalid UTF-8`: no title, no author, no cover, and a parse error is one the
scanner never re-asks. Legacy Russian FB2 collections are predominantly
cp1251. The pass-through is right only for a UTF-8 file that lies about its
label, which the tests at `fb2_test.go:130` and `:142` pin.

**A publisher's ISBN string is stored as written.** `findISBN`
(`internal/epub/epub.go:251`) checks the shape only on its third, bare
branch; the `opf:scheme="ISBN"` and `urn:isbn:` branches return whatever
`normalizeISBN` leaves, so `ISBN 978-0-00-000000-0 (ebook)` stores as
`ISBN9780000000000(ebook)` and `Not available` as `Notavailable`.
`internal/fb2` stores `<isbn>` after a `TrimSpace` and nothing else
(`fb2.go:315`). Both providers' `ByISBN` normalise with their own copies
(`googlebooks.go:624`, `openlibrary.go:472`), each commented as mirroring
`internal/epub`'s. The value is the lookup key both providers are asked
with, is shown on the detail page, and is never reconsidered because the
field is filled.

**Embedded metadata has no length cap.** `internal/service` caps a
person's edit (`metadata.go:76-80`: 1024 bytes for a title and an author
name, 4096 for other scalars, 64 KiB for a description, 100 authors) and
`internal/enrich` restates the same numbers for a provider's answer
(`resolver.go:99-103`). The scanner's `createBook` (`scanner.go:701`)
stores parser output as it comes. A value the app stores but
`normalizeField` rejects is a field that can no longer be edited: opening
the editor and pressing Save unchanged fails on a value nobody typed.

One claim from the backlog items did not survive re-validation.
`2026090719` counts `internal/storage` among the ISBN normaliser's copies.
`ftsquery.go` has `normalizeIfISBNShaped` and two shape predicates for the
search box, which strip hyphens and spaces for a different purpose and do
not share the trailing-`X` rule; the true copies are `internal/epub`,
`internal/googlebooks` and `internal/openlibrary`. The search helpers are
left alone.

## Scope

In scope: the four fixes above, each landing where the value enters the
system rather than where it is later read.

Out of scope, with reasons:

- **A Dockerfile `HEALTHCHECK`.** `distroless/static` ships no shell and
  no `curl`, so there is nothing in the image to run one with; a probe of
  `/healthz` belongs at the compose or orchestrator level, and the README
  says so.
- **Validating an ISBN's check digit.** A malformed ISBN in a file is
  still the best identifier it offers, and a wrong check digit still keys
  a provider lookup that answers no-match cleanly.
- **Re-parsing books already indexed.** A cp1251 file indexed before this
  lands is a filename-titled book with an unchanged path, size and mtime,
  so the cheap check skips it forever. The remedy is the same one-time
  reset every other pre-deployment change documents: delete the database
  and let the next sweep rescan. The service is not deployed.
- **Transcoding on the way out.** Once decoded, the value is UTF-8 like
  every other; nothing downstream changes.

## Decision 1: `LIBRARY_DIR` is never created, and a permission failure names both uids

`resolveDir` splits into two entry points over one body. The library goes
through a variant that stats instead of creating: a missing directory
fails startup with a message naming `LIBRARY_DIR` and the path, since a
library that does not exist is a misconfiguration rather than something to
create empty. `filepath.EvalSymlinks` already refuses to resolve a path
that is not there, so the stat is the failure the helper half has; the
change is that it happens before `MkdirAll` would have papered over it.
`brokenLink`'s pre-check stays in front of both variants, since a dangling
link is a distinct message.

A read-only library then works end to end. The watcher's delivery probe
already treats an unwritable root as an Info-level skip.

`COVERS_DIR` and `DB_PATH`'s directory are still created. When `MkdirAll`
fails with `fs.ErrPermission`, the error is wrapped with the running uid
and the owner uid of the nearest existing ancestor:

```
create covers directory /data/covers: mkdir /data/covers: permission denied
(running as uid 65532; /data is owned by uid 0)
```

The owner comes from `os.Stat` plus `syscall.Stat_t`, behind a
`//go:build linux` file with a fallback that reports only the running uid
elsewhere. The nearest existing ancestor rather than the target, because
the target is what `MkdirAll` could not make.

> **Correction, found while implementing.** The build tag is `unix`, not
> `linux`. `TestMkdirPermissionErrorNamesBothUIDs`, which this plan asks
> for two sections below, asserts the message contains `owned by uid`; the
> development machine and every non-Linux CI runner would take the fallback
> and fail it. `syscall.Stat_t` carries `Uid` on every unix, so the
> narrower tag buys nothing and costs the test. The files are
> `cmd/server/owner_unix.go` and `cmd/server/owner_other.go`.

`storage.Open`'s own `MkdirAll` (`db.go:46`) is reached only after
`resolveDir` has created the directory, so it needs no change.

Rejected: creating the library and warning. Creating it hides the
misconfiguration under an empty grid, which is what `LIBRARY_DIR` pointing
at the wrong volume already looks like, and the log is then the only
place to see it.

## Decision 2: an FB2's declared charset is honoured

`CharsetReader` maps the label through
`golang.org/x/text/encoding/htmlindex.Get` and wraps the input in that
encoding's decoder (`transform.NewReader`). A label `htmlindex` does not
know passes through unchanged, as today. `golang.org/x/text` is already a
direct dependency (`go.mod:8`, for `internal/enrich/match.go`), so this
adds no module.

The consequence for a UTF-8 file that declares `windows-1251` is that its
title becomes mojibake rather than the file failing to parse. That is the
best-effort parse the current comment promises and does not deliver, and
mojibake is one edit away from right where a parse failure is a book
nobody finds. The cost falls on a file that lies about itself; the benefit
falls on every file that does not.

The `.fb2.zip` `cappedReader` (`fb2.go:210`) sits beneath the decoder
unchanged: the cap bounds the bytes read from the archive, and a decoder
over a capped reader stops where the reader does.

The fixture must be a real cp1251 file, not a string transcoded in the
test. `iconv -f UTF-8 -t CP1251` over a UTF-8 source produces the bytes,
and the XML declaration's `encoding` attribute must then be rewritten to
`windows-1251` by hand, since `iconv` transcodes content and leaves the
declaration saying `utf-8`. A hand-made fixture goes wrong at exactly
those two points, the declaration and a stray BOM, which is why the plan
names them. Commit it as `internal/fb2/testdata/cp1251.fb2` with a note at
the top of the test file saying how it was produced, per the convention
the provider packages already follow for fixture provenance.

Rejected: a byte-content sniff that ignores the label. Two encodings that
are both plausible for a run of high bytes cannot be told apart by
sniffing, and the label is the one piece of evidence the file offers.

## Decision 3: one ISBN normaliser, in `internal/storage`

A single exported function beside `MetadataField` and `SortTitle` in
`internal/storage/metadata.go`:

```go
// NormalizeISBN returns the first ISBN-shaped run in raw as bare digits
// with an upper-cased check digit, or "" when raw holds none
func NormalizeISBN(raw string) string
```

It accepts an optional `ISBN` or `urn:isbn:` prefix, hyphens and spaces
between groups, ten or thirteen digits, and a trailing `X` only in the
tenth position of an ISBN-10. Anything before or after the run is ignored,
so `ISBN 978-0-00-000000-0 (ebook)` yields `9780000000000` rather than
being discarded, and `Not available` yields nothing.

> **Correction, found while implementing.** "Anything before or after the
> run is ignored" cannot hold together with the Tests section's case
> "a ten-digit LCCN inside prose → empty": with surrounding text ignored,
> `Library of Congress 2005012345 catalogue` yields a maximal run of
> exactly ten digits and is accepted. Both were tried; the second is the
> one worth keeping, because a bare ten-digit number in a sentence really
> is as likely a control number as an ISBN.
>
> What shipped: a run standing in surrounding text must identify itself —
> thirteen digits, a grouped run (`0-306-40615-2 (pbk.)`), or one behind an
> `ISBN`/`urn:isbn:` marker. Only a bare ten-digit run needs to be the
> whole value. Every example this plan names still holds, the LCCN case
> included. The marker therefore carries evidence rather than being merely
> stripped, which is also what admits `ISBN 0306406152 (ebook)`.
>
> The run is matched maximally over digits, separators and `X` and then
> validated, rather than parsed as it is scanned: that is what makes
> `030640615X7` one eleven-character run that is refused, instead of a
> valid ISBN-10 with a stray digit after it.

It is placed the way `SortTitle` is, because it now has five callers that
must agree: `internal/epub`'s three branches, `internal/fb2`'s `<isbn>`,
and both providers' `ByISBN` and `bestISBN`. The import direction is
already right: `internal/epub` and `internal/fb2` may import `storage`
(the scanner imports all three), and both provider packages import
`internal/enrich`, which imports `storage`. The three unexported copies
are deleted. `isBareISBN` collapses into "the whole trimmed identifier is
the run `NormalizeISBN` found", so the bare branch keeps refusing a
ten-digit LCCN with surrounding text while still accepting a bare
hyphenated one.

An `opf:scheme="ISBN"` identifier whose value yields nothing falls through
to the next identifier rather than ending the search, so a publisher who
writes `Not available` under the ISBN scheme and the real number under
`urn:isbn:` still gets the real number.

Rejected: a fourth private copy in `internal/fb2`. The comments on the
existing copies argue the rule is small and stable; it is also now wrong
in three of four places at once, which is what "stable" looks like when
nobody owns it.

## Decision 4: embedded metadata is capped where it is extracted

The five length constants move to `internal/storage`, exported, and the
two current owners refer to them:

| constant | value | today |
|---|---|---|
| `storage.MaxTitleBytes` | 1024 | `service.maxTitleBytes`, `enrich.maxEnrichedTitleBytes` |
| `storage.MaxAuthorNameBytes` | 1024 | `service.maxAuthorNameBytes`, `enrich.maxEnrichedAuthorNameBytes` |
| `storage.MaxScalarBytes` | 4096 | `service.maxScalarBytes`, `enrich.maxEnrichedScalarBytes` |
| `storage.MaxDescriptionBytes` | 64 KiB | `service.MaxDescriptionBytes`, `enrich.maxEnrichedDescriptionBytes` |
| `storage.MaxAuthors` | 100 | `service.maxAuthors`, `enrich.maxEnrichedAuthors` |

`service.MaxDescriptionBytes` and `service.MaxMetadataValueBytes` stay
exported where they are, defined from the storage constants, so
`internal/web`'s body-cap derivation does not move. `internal/enrich`'s
reason for restating rather than importing was that `internal/service`
sits above it; `internal/storage` sits below both, so the reason is gone.

The scanner's `createBook` applies the caps to `bookMeta` before building
the `storage.Book`: scalars truncated on a UTF-8 boundary
(`strings.ToValidUTF8` over the byte prefix, as `sanitizeValue` does),
the author list cut at `MaxAuthors`. Truncation logs once per book at
Info naming the path and field, since a 10 MB `<dc:description>` is worth
knowing about and is not an error. A truncated field is still `embedded`
in `field_sources`; provenance describes where the value came from, not
whether it was cut.

Rejected: rejecting the file. A book whose description is too long is
still a book, and the filename-titled fallback is worse than a truncated
description.

## Changes

- `cmd/server/main.go`: `resolveDir` becomes `resolveDir(label, dir,
  create bool)` or two thin wrappers over one body; the library call site
  passes the non-creating one; `MkdirAll`'s `fs.ErrPermission` is wrapped
  with `os.Getuid()` and the ancestor owner.
- `cmd/server/owner_linux.go`, `cmd/server/owner_other.go`: `ownerUID(path
  string) (int, bool)` via `syscall.Stat_t`, and the no-op fallback.
- `Dockerfile`: the comment drops `LIBRARY_DIR` from the "must be writable"
  list.
- `internal/fb2/fb2.go`: `CharsetReader` via `htmlindex`; `<isbn>` through
  `storage.NormalizeISBN`.
- `internal/fb2/testdata/cp1251.fb2`: the transcoded fixture.
- `internal/storage/metadata.go`: `NormalizeISBN` and the five `Max*`
  constants.
- `internal/epub/epub.go`: `findISBN` calls `storage.NormalizeISBN` on all
  three branches; `isBareISBN` and `normalizeISBN` deleted.
- `internal/googlebooks/googlebooks.go`, `internal/openlibrary/openlibrary.go`:
  local `normalizeISBN` deleted, callers use `storage.NormalizeISBN`.
- `internal/service/metadata.go`: constants defined from `storage`'s.
- `internal/enrich/resolver.go`: `maxEnriched*` deleted, `sanitizeValue`
  and `metadataValues` use `storage`'s.
- `internal/scanner/scanner.go`: `capMetadata(path string, m bookMeta)
  bookMeta` applied in `createBook`.
- `README.md`: the Running section gains the ownership instruction; the
  `LIBRARY_DIR` row says the directory must exist and may be read-only.

## Tests

`cmd/server`:

- `TestResolveDir` gains cases: the library variant fails on an absent
  directory with a message containing `LIBRARY_DIR`; the creating variant
  still creates; both still report a dangling link first.
- `TestRunWithAReadOnlyLibrary`: `LIBRARY_DIR` at mode `0o555` (skipped
  under root, where the mode is not enforced) starts, sweeps and indexes a
  book.
- `TestMkdirPermissionErrorNamesBothUIDs`: an unwritable parent (same root
  skip) produces an error containing `running as uid` and `owned by uid`.

`internal/fb2`:

- `TestReadMetadataDecodesWindows1251` reads `testdata/cp1251.fb2` and
  asserts the Cyrillic title and author exactly.
- `TestReadMetadataDeclaredWindows1251ParsesAsUTF8` is rewritten as
  `TestReadMetadataMislabelledUTF8DegradesToMojibake`: the same UTF-8 body
  declared `windows-1251` parses without error and its title is not the
  original string, pinning that a lying label costs the title and not the
  book.
- `TestReadMetadataUnknownEncodingLabelDoesNotError` stands.
- `TestReadMetadataZipCapStillAppliesUnderADecoder`: a cp1251 `.fb2.zip`
  past `maxZipDocumentBytes` fails with `errDocumentTooLarge`.
- `TestReadMetadataNormalisesISBN`: `<isbn>978-5-17-118366-1</isbn>`
  stores `9785171183661`.

`internal/storage`:

- `TestNormalizeISBN`, a table: bare 10 and 13, hyphenated, spaced,
  `urn:isbn:` and `ISBN ` prefixes, lower-case `x`, `ISBN 978-0-00-000000-0
  (ebook)` → the digits, `Not available` → empty, a UUID → empty, a
  ten-digit LCCN inside prose → empty, fourteen digits → empty, an `X` in
  any position but the tenth → empty.

`internal/epub`:

- `TestReadMetadataSchemeISBNWithSurroundingText` recovers the number from
  `ISBN 978-… (ebook)`.
- `TestReadMetadataSchemeISBNNotAvailableFallsThrough`: `Not available`
  under the ISBN scheme, then a `urn:isbn:` identifier, yields the URN's.
- The five existing ISBN tests stand and pass against the shared function.

`internal/googlebooks`, `internal/openlibrary`: existing `ByISBN` tests
stand; one new case each asserts a hyphenated argument reaches the request
URL as bare digits, which is what the deleted copy guaranteed.

`internal/service`, `internal/enrich`: no behaviour change; the existing
limit tests stand and now pin that the two packages read one constant. A
compile failure is the guard if a constant is renamed in one place only.

`internal/scanner`:

- `TestCreateBookCapsEmbeddedMetadata`: an EPUB with a description over
  `MaxDescriptionBytes` (built in-test, since `internal/epub` already
  bounds the OPF at 4 MiB the description has to be under that and over
  64 KiB), a 2000-byte title, and 150 `<dc:creator>` elements indexes with
  the description and title cut on a rune boundary, exactly `MaxAuthors`
  authors in source order, and every field `embedded` in `field_sources`.
- `TestCreateBookCappedValuesAreEditable`: the same book's title and
  description round-trip through `service.UpdateBookMetadata` unchanged,
  which is the property the caps exist for.

## docs/notes/formats.md

The **ISBN** paragraph under EPUB names `storage.NormalizeISBN` as the one
derivation every reader of an ISBN calls, and says an `opf:scheme="ISBN"`
value that holds no ISBN-shaped run falls through. The FB2 section's
**Declared encoding is ignored** paragraph is replaced by one describing
the `htmlindex` mapping, the pass-through for an unknown label, and why a
mislabelled UTF-8 file degrading to mojibake is the right trade. Drop the
backlog citation.

## docs/notes/scanner.md

**Sweep and identity** gains a short paragraph: embedded metadata is capped
at extraction to the same limits an edit and a provider answer meet,
truncated on a UTF-8 boundary and logged, so every value in `books` is one
the editor accepts. **Paths and symlinks** gains the two resolution rules
for configured directories: the library must exist and may be read-only;
the covers and database directories are created, and a permission failure
names both uids.

## docs/notes/enrichment.md

The **Sanitising values** paragraph stops restating the numbers and points
at `internal/storage`'s constants as the single definition all three
writers share.

## CLAUDE.md

The Enrichment invariant "`sanitizeValue`'s limits equal
`internal/service`'s" becomes "all three writers cap through
`storage.Max*`; never restate a number". The Formats invariants gain
"every ISBN reader calls `storage.NormalizeISBN`" and "FB2's declared
charset is decoded through `htmlindex`, passing through only an unknown
label".

## README.md

Running: after the `docker run` block, one paragraph saying the container
runs as uid 65532, the data volume must be owned by it or the container
run with `--user` matching the volume's owner, and the library may be
read-only. Configuration: the `LIBRARY_DIR` row says the directory must
exist. A sentence in Running points health probing at `/healthz` from the
orchestrator.

## Verification

- `docker run` with `/data` as a fresh root-owned named volume fails
  naming both uids; with `--user 65532:65532` and a chowned host directory
  it starts.
- `docker run` with the library mounted `:ro` starts and indexes it.
- A real cp1251 FB2 from a legacy collection shows its Cyrillic title and
  author in the grid after one sweep, with no `invalid UTF-8` Warn in the
  log.
- An EPUB carrying `ISBN 978-… (ebook)` shows the bare digits on the detail
  page and Fetch metadata asks Open Library by that number (Debug log).
- An EPUB with a multi-megabyte description opens its detail page, and
  Save unchanged in the description editor succeeds.
