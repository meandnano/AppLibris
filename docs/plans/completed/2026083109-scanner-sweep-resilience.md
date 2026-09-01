# Step: Sweep resilience — partial walks and portable paths

## Context

Two scanner defects that both surface as "books silently missing from the
library", from different directions.

**1. One unreadable directory aborts the entire sweep.** The `WalkDir`
callback in `scanner.go` opens with `if err != nil { return err }`.
`filepath.WalkDir`'s contract is that returning a non-nil error (other than
`fs.SkipDir`/`fs.SkipAll`) stops the walk immediately — so a single
permission-denied subdirectory, a broken symlink target, or a stale NFS
handle means **every file after it in walk order is never scanned**. `Scan`
returns the error, `runScan` logs it at Error, and the index is left
partially populated with no indication of how much was missed.

This is the opposite of how the same function treats per-*file* problems
two lines below, where a failure is logged, counted in `result.Errors`, and
the sweep continues. Directory errors should follow that established
pattern.

**2. Paths are stored exactly as walked, so changing `LIBRARY_DIR`
re-indexes everything.** The scanner stores whatever `WalkDir` yields,
which is `LIBRARY_DIR` plus the relative path. Going from `./library`
(`make run`) to `/library` (the Dockerfile) — the normal path from
development to production — makes every stored `file_path` miss the cheap
check. Every file is re-hashed, found by content hash, and gains a *second*
`book_files` row at the new prefix. The old rows are never removed, so
**every book in the library then reports multiple file locations**, which
is precisely the signal UI.md wants to use for byte-identical duplicates.
The duplicate flag becomes noise on its first day in production.

## Depends on

Nothing hard. Ordering note: landing this before
`2026083110-missing-file-reconciliation` matters, because that step decides
what to prune based on which directories a sweep successfully walked — a
question this step is what makes answerable.

## Directory errors

Change the callback's error branch to distinguish the two cases:

```go
if err != nil {
    // A directory we can't read costs us its subtree, not the sweep.
    // Anything else (including an error on the root itself) is fatal.
    if d != nil && d.IsDir() && path != libraryDir {
        slog.Warn("skipping unreadable directory", "path", path, "error", err)
        result.Errors++
        return fs.SkipDir
    }
    return err
}
```

Three things this gets right that a blanket `return nil` would not:

- **The root still fails hard.** If `LIBRARY_DIR` itself is missing or
  unreadable, that is a configuration error, not a partial result, and
  `Scan` should return it as it does today. `cmd/server` creates the
  directory at startup, so in practice this means "the volume didn't
  mount" — which must not look like an empty library.
- **`d` can be nil.** `WalkDir` passes a nil `DirEntry` when the error comes
  from `os.Lstat` on the root; dereferencing it without the guard turns a
  missing library directory into a panic.
- **It counts.** Reusing `result.Errors` means a sweep that skipped a
  subtree already logs at Warn via the existing `Errors > 0` branch in
  `runScan`, with no new plumbing.

`fs.SkipDir` returned from a directory error skips that directory's
subtree and continues the walk — which is the whole point.

## Storing relative paths

Store every `book_files.file_path` **relative to the library root**, and
join the root back on whenever a path is used to touch the filesystem.

- `Scan` computes `rel, err := filepath.Rel(libraryDir, path)` for each
  file and passes `rel` to `scanFile`, keeping the absolute `path` for
  `os.Stat`, hashing, and metadata extraction. A `Rel` failure (different
  volumes on Windows; not reachable on this project's Linux target but
  cheap to handle) is a per-file error, logged and counted.
- `filepath.ToSlash` the result before storing. Paths are an index key, not
  a display string, and a database that is portable between a Linux
  container and a macOS dev machine should not encode the separator. The
  reverse (`filepath.FromSlash`) happens on the way out.
- Anything that later needs to *open* a book — send-to-Kindle, cover
  regeneration, FB2 parsing — joins `LIBRARY_DIR` back on. There is no such
  caller yet, which is exactly why this is the moment to change the
  convention: today it costs one function, later it costs every reader.

**No migration is needed.** The service has never been deployed and no
database exists, so there are no rows holding absolute paths to convert —
the scanner simply stores relative paths from its first sweep onwards.

> An earlier draft of this plan specified a one-shot
> `RelativiseFilePaths(ctx, root string) (int, error)` storage method run
> at the top of every `Scan`, rewriting absolute or `./`-prefixed rows to
> their form relative to the library root, guarded by a
> `WHERE file_path LIKE '/%' OR file_path LIKE './%'` so it no-opped on
> later sweeps. With no rows to convert that method is dead code on its
> first execution and forever after — a permanent query at the top of every
> sweep to fix a condition that can never arise. Leave it out.
>
> The same reversal was applied to
> `docs/plans/2026083105-storage-schema-hardening.md` and
> `docs/plans/completed/2026083106-sort-title-normalisation.md`; the latter
> records the reasoning, and the limits of it, in full.

Anyone holding a local `data/library.db` from an earlier run should delete
it. Nothing will convert its absolute paths, so every file would miss the
cheap check, get re-hashed, and gain a *second* `book_files` row at the
relative path — which is precisely the duplicate-locations failure this
step exists to prevent.

## Tests

`internal/scanner`:

- A library with an unreadable subdirectory still indexes files in sibling
  directories, returns a nil error, and reports `Errors == 1`. Skip the
  test when running as root (`os.Geteuid() == 0`), where mode bits aren't
  enforced — CI containers commonly run as root and the test would
  otherwise pass vacuously.
- A missing `LIBRARY_DIR` still returns an error rather than an empty
  successful sweep.
- Stored paths are relative: scan a nested file and assert
  `FindFileByPath(ctx, "sub/dir/book.epub")` finds it, and that no stored
  path contains the temp-directory prefix. This is the assertion that
  actually pins the fix — the existing scanner tests all use a flat
  library, so they pass either way.
- Scanning the same library through two different absolute roots (copy the
  fixture, or bind the same directory via a symlink) yields one
  `book_files` row per book, not two.

## CLAUDE.md

Update the `internal/scanner` bullet: `book_files.file_path` is stored
relative to `LIBRARY_DIR` (slash-separated), so the index survives the
library being mounted at a different path; an unreadable directory skips its
subtree and counts an error instead of aborting the sweep.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: scan a library at `./library`, then restart with
  `LIBRARY_DIR=$(pwd)/library` and confirm the second sweep reports every
  file `Unchanged` and no book gains a second `book_files` row —
  `SELECT book_id, COUNT(*) FROM book_files GROUP BY book_id HAVING
  COUNT(*) > 1` should return only genuine duplicates.
- Manual: `chmod 000` a subdirectory, sweep, and confirm the rest of the
  library still indexes with a Warn line naming the skipped directory.
