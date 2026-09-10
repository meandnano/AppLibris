# Step: a symlinked library is a library

## Position in the sequence

Independent of the other plans. `cmd/server` and `internal/scanner`.

## Context

Found in the 2026-09-07 review and confirmed with a throwaway walk.
`Scan` uses `filepath.WalkDir`, which `Lstat`s the root and never follows
links. Two consequences:

- A **symlinked `LIBRARY_DIR`** (`~/Books -> /volume1/books`, the ordinary
  Synology shape) is visited once as a non-directory entry and the walk
  ends. `Scanned == 0`, one Warn ("library appeared empty"), zero books,
  no error. `os.MkdirAll(libraryDir)` in `cmd/server` succeeds through the
  link, so startup gives no hint either. `NewWatcher` and `Refresh` share
  the blindness.
- A **symlinked subdirectory** inside the library is visited as a
  non-directory whose name has no supported suffix, so `d.IsDir() ||
  matchedSuffix(d.Name()) == ""` drops it silently.

The first is a configuration the documentation would accept and the
server rejects without saying so. The second is a choice worth making
deliberately rather than by omission.

## Scope

In scope: resolving the library root once at startup; deciding, and
logging, what happens to a symlinked subdirectory.

Out of scope: following directory symlinks during the walk. See Decision
2.

## Decision 1: `cmd/server` resolves `LIBRARY_DIR` with `filepath.EvalSymlinks` before anything sees it

One call, after `MkdirAll`, before `storage.Open`. The resolved path is
what `Scan`, `NewWatcher`, the sender worker and the enrichment worker all
receive. Relative `file_path` storage is unaffected, since every path is
made relative to whatever root the scanner is given and the root is now
consistent.

A `LIBRARY_DIR` whose resolution fails (a dangling link) is a startup
error naming both the configured and the attempted path. Today it is an
empty library, which is worse.

`COVERS_DIR` and `DB_PATH` get the same treatment for consistency, though
neither is walked; a symlinked covers directory works today because
`os.CreateTemp` and `os.Rename` follow links.

**Correction found while implementing this, recorded here because the
instruction above is wrong as written in two places.**

*One call, after `MkdirAll`* does not produce the dangling-link error this
decision asks for, because `MkdirAll` fails first and describes the wrong
thing: `os.Stat` follows the link and reports the target absent, so
`MkdirAll` goes on to `os.Mkdir` the path itself, which fails `EEXIST` on
the link. The message is `mkdir ./library: file exists` — it names the link
it could not replace and never the target that is missing, which is the
whole question when a volume did not mount, and `EvalSymlinks` is never
reached to say otherwise. So the dangling case is checked *before*
`MkdirAll`, with `os.Readlink` plus `os.Stat`: a path that is a link and
does not stat is the startup error naming both. `EvalSymlinks` still runs
after `MkdirAll` for the ordinary case.

*`DB_PATH` gets the same treatment* cannot be a plain `EvalSymlinks` either:
that call needs every element of the path to exist, and the database file
does not on a first run — every fresh deployment would fail at startup. Only
`filepath.Dir(dbPath)` is resolved (through the same `resolveDir`, which
also creates it, as `storage.Open` would), and the base name is rejoined.
The value is cosmetic in the way the decision already says — the
`listening` line names the file actually opened — so resolving the final
element once it exists buys nothing worth a second code path.

One consequence not anticipated above, kept rather than suppressed:
Decision 2's branch fires on the walk *root* too, since `WalkDir` hands the
root to the callback as an ordinary symlink entry. A symlinked
`LIBRARY_DIR` reaching `Scan` unresolved therefore now warns and counts an
error instead of reading as an empty library. Decision 1 means production
never takes that path; it is a backstop for an embedded caller, and a
scanner test pins it.

## Decision 2: a symlinked subdirectory is skipped with a Warn, not followed

Following directory symlinks means a cycle guard keyed on `(dev, ino)`,
and it means a link pointing outside the library indexes files the
relative-path storage cannot express (a `file_path` would need `..`).
The mover story in CLAUDE.md is built on the index never caring about
inodes. Not following is the safer default; what changes is that it is
said out loud.

In the `WalkDir` callback, an entry with `d.Type()&fs.ModeSymlink != 0`
that resolves to a directory is logged once per sweep at Warn ("symlinked
directory is not followed") and counted in `result.Errors` so the sweep
summary shows it. A symlinked **file** with a supported suffix is followed
as today (`os.Stat`, `os.Open` and the hash all go through the link), and
a dangling one is already a per-file Warn.

The Warn is per sweep and would repeat every fifteen minutes. That is the
intended pressure toward fixing the configuration, the same choice the
delivery probe's silence Warn makes.

## Changes

- `cmd/server/main.go`: `EvalSymlinks` on the three paths after their
  `MkdirAll`; the resolved values are what is logged in the `listening`
  line.
- `internal/scanner/scanner.go`: the symlinked-directory branch in the
  walk callback.

## Tests

- `internal/scanner`: a library root that is a symlink to a real
  directory (tests pass the resolved path, since the resolution lives in
  `cmd/server`; a `cmd/server` test covers `EvalSymlinks` being applied).
  A symlinked subdirectory containing an EPUB: not indexed, one Warn in
  the captured log, `Errors == 1`. A symlinked EPUB file: indexed.
- `cmd/server`: `run` with `LIBRARY_DIR` set to a symlink starts against
  the target; set to a dangling link, it fails with both paths in the
  error.

## CLAUDE.md

The `cmd/server` paragraph gains the resolution step. The
`internal/scanner` paragraph gains the symlinked-directory rule and its
reason.

## Verification

- `ln -s /real/books ./library`, start the server: the first sweep indexes
  the books and the `listening` log line names the resolved path.
- Add `ln -s /elsewhere ./library/more`: the sweep summary shows one error
  and the log explains it.
