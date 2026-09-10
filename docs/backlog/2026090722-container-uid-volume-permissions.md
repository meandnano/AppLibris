# Backlog: a volume the container's uid cannot write to fails startup with a bare permission error

## Problem

The image runs as `nonroot` (uid 65532) with `WORKDIR /`, and once its
configuration is parsed `run` creates all three configured directories,
calling `resolveDir` for `LIBRARY_DIR`, for `COVERS_DIR` and for
`DB_PATH`'s directory. Each one reaches:

```go
if err := os.MkdirAll(dir, 0o755); err != nil {
	return "", fmt.Errorf("create %s %s: %w", label, dir, err)
}
```

`storage.Open` has its own `MkdirAll` for `DB_PATH`'s directory, but
`resolveDir` has already created it by then, so the failure surfaces from
`cmd/server`. On a NAS the
mounted paths are almost never owned by uid 65532: Synology bind mounts
are owned by the share's user, Unraid's by `nobody` (99), a fresh Docker
named volume by root. `/data` does not exist in the image, so a named
volume mounted there is root-owned `755`. The result is:

```
run error="create covers directory /data/covers: mkdir /data/covers: permission denied"
```

and the container exits. The Dockerfile comment says the volumes "must
be writable by uid 65532", which is true and is where nobody reading a
compose file looks.

The library directory is the odd one out: the scanner only reads it, and
a read-only library is an Info-level skip for the delivery probe, so
creating it is the one call that turns a legitimately read-only
mount into a startup failure when the directory already exists but is
not writable. `MkdirAll` on an existing directory succeeds regardless of
its permissions, so this only bites when the path does not exist, but
that is the first run, which is when it matters most.

## Why this is backlog, not a plan

It is a deployment ergonomics gap, not a defect in the running service:
once the volumes are owned correctly, or the container runs with
`--user` matching them, everything works. The README is being
regenerated and is the right place for the instruction; what this item
records is the small code change that would make the failure explain
itself, and the point that `LIBRARY_DIR` should not need to be writable
at all.

## Re-validate before acting

- Whether `run` still creates `LIBRARY_DIR` (today via `resolveDir`).
- Whether the Dockerfile still sets `WORKDIR /` and the `nonroot` user.

## Sketch

- Do not create `LIBRARY_DIR`. Stat it; if it is missing, fail with a
  message naming the variable, since a library that does not exist is a
  misconfiguration rather than something to create empty. A read-only
  library then works. `resolveDir` needs the directory to exist anyway —
  `filepath.EvalSymlinks` resolves nothing that is not there — so "stat
  instead of create" is a failure path it already half has, in its
  broken-link pre-check.
- All three directories now go through that one helper, so this is one
  place to change rather than three call sites; `LIBRARY_DIR` wants a
  variant that refuses rather than creates.
- For `DB_PATH` and `COVERS_DIR`, wrap the permission error with the
  running uid and the directory's owner (`os.Getuid`, `os.Stat` plus
  `syscall.Stat_t` on Linux) so the log line reads "running as uid 65532,
  /data is owned by uid 0" and the fix is obvious.
- Consider `HEALTHCHECK` in the Dockerfile against `/healthz`, so an
  orchestrator sees a container that started but cannot write, once the
  above stops that being a startup failure.
