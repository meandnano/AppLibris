# Step: close the import review findings

## Context

A review of the import step (`2026091104-import-upload.md`) found six
things. Four are fixed here; two are recorded below as deliberately left.
The review also found three places where the completed plan and the code
disagree. That plan is immutable, so the record of those disagreements lives
in this one.

## Findings fixed

### An unwritable staging directory ends the run

`importer.New` wipes and creates `os.TempDir()/applibris-imports`, and
`cmd/server` treats its error as fatal, closing the database and returning.
A container run `--read-only` with no tmpfs at `/tmp` has no such directory
to give, so a deployment that ran before the import step crashes on it.

The write probe already answers an unwritable library with "import
disabled" at Warn, because a library that can be read is still worth
serving. The staging directory is the second precondition and gets the same
answer. `newStager` in `cmd/server` holds both: it runs the probe, then
`importer.New`, and returns nil on either failure with a Warn naming the
directory so `TMPDIR` is the obvious remedy. Nothing about the run changes
otherwise. `TestNewStagerDisablesImportWhenStagingCannotBeCreated` and
`TestNewStagerBuildsAnImporterWhenBothDirectoriesAreWritable` pin both
directions.

### README understates `MAX_IMPORT_SIZE`

The configuration row listed `K`/`M`/`G` and `Ki`/`Mi`/`Gi` while the default
in the same row is `64MiB`, a spelling the row did not admit; `parseByteSize`
also takes `KB`/`MB`/`GB`, `KiB`/`MiB`/`GiB` and `B`, case-insensitively. The
row now lists all of them. The "Importing a book" section said 256 MB for
four times 64 MiB; it says 256 MiB.

### Library names keep characters Windows refuses

`sanitizeStem` dropped `:` and a trailing dot or space so a mounted library
reads from Windows, but kept `<`, `>`, `"`, `|`, `?` and `*`, which Windows
refuses in a filename outright. An upload called `Dune?.epub` produced a
library file no Windows client could open. The six are dropped alongside the
colon, for the same reason. Reserved device names (`CON`, `NUL`, `COM1`)
stay: the finding was about characters, and a stem equal to one is not a
name anybody uploads.

### The completed import plan and the code disagree in three places

Recorded here because `docs/plans/completed/` is immutable. In each case the
code and `docs/notes/import.md` are right.

- **Publish.** The plan's Decision 5 step 2 ends the copy with `Rename` to
  `<name>`. The code publishes with `os.Link` onto the first free name and
  unlinks the `.part`, falling back to `Lstat` then `Rename` only on a
  filesystem without hard links, logged once. A link fails `EEXIST` rather
  than replacing, where a rename silently destroys whatever a person put at
  that name during the seconds the copy took, and the library-directory rule
  is that writes only ever create new paths.
- **Staging budget.** The plan bounds one upload at the cap and says nothing
  about how many may wait at once. The code bounds staging in total bytes
  at `stagingBudgetFactor` times the cap, reserves at the cap before the
  copy and corrects to what the stage retains afterwards, and refuses past
  the budget with `ErrStagingFull`. Staging is tmpfs in a container, so an
  unbounded number of forgotten tabs is RAM.
- **Confirm on an `exists` verdict.** The plan says confirm never trusts the
  verdict and always copies. The code answers the existing book id without
  copying for `exists`, because reaching that verdict deleted the staged file
  at once — there is nothing left to copy, and the bytes are in the library
  and indexed, which is what the caller asked for. `new` and `title-match`
  are still not trusted; `IndexFile`'s answer is the truth for those.

## Deliberately left

- **The staging budget can overshoot.** The reservation is corrected from
  the cap to file plus cover plus metadata, which can exceed the cap, so
  several maximum-size uploads with large covers pass a budget of four times
  the cap by up to a cover each. Low impact and no data at risk; worth a
  backlog item if it ever matters.
- **The preview description flattens line breaks.** `.import__description`
  has no `white-space: pre-line`, unlike the detail page's description.
  Cosmetic, and the redirect lands on the page that renders it properly.

## Changes

- `cmd/server/main.go`: `newStager`; `run` calls it and no longer returns
  on a staging-directory error.
- `cmd/server/main_test.go`: the two `newStager` tests and
  `openServerTestDB`.
- `internal/web/import.go` and `templates/partials.html`: the disabled page
  and the refusal line name both causes and point at the log, since the
  service cannot tell which one applied and a page blaming a read-only
  library would be wrong for the other.
- `internal/importer/name.go`: the six characters in `sanitizeStem`, and the
  reason in `libraryStem`'s comment.
- `internal/importer/name_test.go`: two rows in the sanitising table.
- `CLAUDE.md`: the `cmd/server` bullet and the `Stager` invariant name both
  preconditions.
- `docs/notes/import.md`: the staging directory under "Writability is
  probed once, at startup"; the character list in the confirm order.
- `README.md`: the `MAX_IMPORT_SIZE` row, 256 MiB, and the `--read-only`
  sentence under "Importing a book".

## Verification

- `go test ./...`, `go vet ./...`, and `go test -race` over `cmd/server` and
  `internal/importer`.
- `TMPDIR=/nonexistent make run` against a writable `./library` starts,
  logs "importing disabled" naming the directory, `GET /import` explains
  itself and the grid works; with `TMPDIR` restored the Import link is back.
- Upload a file named `What? Is: This*.epub` and read "Will be saved as
  What Is This.epub" in the preview.
