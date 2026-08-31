# Step: Make covers actually regenerable

## Context

DESIGN.md states the covers directory is "fully disposable" because "a
missing or corrupt cover is regenerable from the source file". It is not,
and never has been.

Cover extraction lives only on `createBook`'s path in the scanner, which
runs exactly once per book — the first time its content hash is seen.
Verified: delete the covers directory and rescan, and the sweep reports
`Unchanged: 1`, re-extracts nothing, and does not even recreate the
directory. **Deleting `COVERS_DIR` permanently loses every cover in the
library.** The property DESIGN.md relies on to call the directory
disposable is exactly the property that is missing, and the plan for the
original covers step (`2026083001-covers.md`) said as much out loud —
"DESIGN.md's 'regenerable from the source file' is what makes the derived
directory disposable, not a feature this step has to build". This is the
step that builds it.

Two smaller defects in the same area, worth fixing in the same pass:

**`cover.Store` writes non-atomically.** `os.Create` truncates the final
path and JPEG-encodes straight into it. A crash, a full disk, or a killed
container mid-encode leaves a truncated file at the canonical path — and
because of the bug above, it is never regenerated. The two defects compound:
a partial write becomes permanent corruption.

**A missing covers directory fails silently.** `Store` requires `dir` to
exist and says so, but the scanner passes `coversDir` straight through
without checking. Verified: with a nonexistent covers directory a scan
reports `Errors: 0` and creates books with no covers, the failure visible
only as a `slog.Warn` per file. `cmd/server` does create the directory at
startup, so this only bites when something removes it later — which is
precisely the disposable-directory scenario this step is about.

## Scope

In scope: re-extracting a cover when the stored one is missing or unusable,
atomic writes, and the directory check. Out of scope: FB2 covers (see
`docs/backlog/2026083119-fb2-metadata.md`), a manual "refresh cover" UI
action, and pruning covers no book references — all separate concerns.

## Detecting a cover that needs regenerating

During a sweep, for a file whose content is already known (the branch that
currently just refreshes stat or attaches a location), check the book's
cover and re-extract if needed. The check must be cheap — it runs per file,
per sweep, over the whole library:

- `book.CoverPath == ""` → the book has never had a cover. **Do not
  re-extract.** An empty `cover_path` is the recorded result of "this book
  has no embedded cover", and retrying it every sweep means re-opening and
  re-parsing every coverless EPUB in the library forever. Books whose cover
  extraction genuinely failed once are the acceptable cost; a manual
  refresh action is the right fix for those, later.
- `book.CoverPath != ""` and `os.Stat` fails → regenerate.
- `book.CoverPath != ""` and the file is zero bytes → regenerate. Catches
  the truncation-at-offset-0 case, which is the common shape of an
  interrupted `os.Create`.

Anything more thorough (decoding the JPEG to prove it is intact) is too
expensive per sweep. Atomic writes below are what actually prevent
corruption; the zero-byte check just cleans up after the ones already on
disk.

Regeneration re-reads the source file, so it needs the absolute path —
`filepath.Join(libraryDir, relPath)` once `2026083109-scanner-sweep-resilience`
has landed.

On success, `UPDATE books SET cover_path = ?`. The path is derived from the
content hash and so will be unchanged in the normal case, but writing it
keeps the code honest if the naming scheme ever changes, and it is one
statement.

## Atomic writes in `internal/cover`

Replace `os.Create` + encode with encode-to-temp + rename:

1. `os.CreateTemp(dir, contentHash+".jpg.tmp*")` — same directory, so the
   rename is on one filesystem and therefore atomic.
2. Encode into it, `Close`.
3. `os.Rename(tmp, final)` — atomic on POSIX; a reader either sees the old
   file or the complete new one, never a partial.
4. `defer os.Remove(tmp)` so an error path doesn't litter. Ignore the error
   from that remove — after a successful rename the temp name is gone and
   `Remove` returning `ENOENT` is expected, not a problem.

Also have `Store` create `dir` itself (`os.MkdirAll(dir, 0o755)`) rather
than documenting that the caller must. It is one line, it makes the
function total, and it removes the silent-failure mode above — the covers
directory being disposable means "recreate it when you need it" is the
correct behaviour, not a courtesy. Update the doc comment, which currently
promises the opposite.

## `internal/scanner`

- `Result` gains `CoversRegenerated int`; `runScan` adds it to the summary.
- The regeneration path logs at Info when it rewrites a cover — it is I/O
  the user didn't ask for and should be able to see, especially the first
  sweep after a covers directory is wiped, which will regenerate the whole
  library.
- A regeneration failure is a Warn and does not fail the file: the book row
  is fine, it just has no thumbnail this time round. Same posture as the
  existing cover-store failure.

## Tests

`internal/cover`:

- `Store` creates a missing directory rather than erroring.
- After `Store`, no `*.tmp*` files remain in the directory.
- `Store` over an existing cover replaces it (the rename path), and the
  result decodes.

`internal/scanner`:

- The regression this step exists for: scan an EPUB with a cover, delete
  the covers directory, scan again, and assert the cover file exists and
  `CoversRegenerated == 1`. Today the second scan reports `Unchanged` and
  creates nothing.
- A zero-byte cover file is replaced with a valid one.
- A book with `cover_path = ''` does **not** cause a re-parse on every
  sweep — assert `CoversRegenerated == 0` over two sweeps of a coverless
  EPUB. This pins the deliberate decision above; without the test, a later
  "improvement" to retry empty cover paths would quietly make every sweep
  O(library) in EPUB parses.

## CLAUDE.md

Update the `internal/cover` and `internal/scanner` bullets: covers are
written atomically (temp + rename) into a directory `Store` creates on
demand, and a sweep re-extracts a cover whose stored file has gone missing
or is empty — so `COVERS_DIR` really is disposable, as DESIGN.md assumes.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual, the DESIGN.md claim end to end: with the server running against a
  populated library, `rm -rf` the covers directory, wait for a sweep, and
  confirm the grid renders every cover again and the log reports the
  regeneration count.
