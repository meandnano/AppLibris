# Step: Filesystem watcher

## Context

DESIGN.md's scanner section names two entry points sharing one code path —
a full sweep and a filesystem watcher — and only the first is built. Its
status note is blunt about the cost: "a new file takes up to
`SCAN_INTERVAL` (default 15m) to appear."

That section also decides, in advance, what this step is allowed to be:

> The watcher is an optimisation, not the mechanism — it is unreliable
> across Docker volume mounts and network shares. A **periodic rescan** is
> the safety net and is always on.

Every decision below follows from taking that literally. The watcher never
becomes a second way to index a file; it is a **trigger** that makes the
existing sweep run sooner. Nothing about correctness may depend on an
event arriving, because on the target deployment a whole class of changes
provably never generates one.

## The target deployment, measured rather than assumed

The library lives on an Unraid **user share**, bind-mounted into the
container. That is a FUSE filesystem (`shfs`) presenting several physical
disks plus a cache pool as one tree, and it is the single most important
constraint on this step — so it was tested rather than taken on folklore.

Verified here with a go-fuse loopback mount (a FUSE passthrough over a
backing directory, which is structurally what `shfs` is) and
`fsnotify` v1.10.1:

| What happened | Watch on the FUSE mount |
|---|---|
| File created **through** the mount | `CREATE` + `WRITE` — events arrive |
| File created **directly in the backing directory** | **no events at all** |
| That same file, listed through the mount afterwards | visible to `readdir` |

So the rule is not "inotify doesn't work on Unraid shares". It is:

**Events fire for changes made through the mount, and are lost for changes
made behind it.** Copying a book to the share over SMB goes through
`shfs`, so it is seen. Unraid's **mover** relocating a file between the
cache pool and the array operates on `/mnt/cache` and `/mnt/diskN`
directly — behind the share — so it is not. Nor is anything written
straight to a disk path.

Three further measurements, each of which changes the design:

- **`fsnotify.Add()` on the FUSE mount returns no error.** A watch that
  will never deliver an event looks exactly like a healthy one. There is
  nothing to catch, so silence has to be probed for, not waited on.
- **`CREATE` fires before the data is there.** Copying an 8MB file in
  produced `CREATE` immediately, then `WRITE` — reacting to the `CREATE`
  would hash a partial file.
- **inotify is not recursive, and a watch is dropped silently when its
  directory goes away.** Creating a subdirectory produced one `CREATE` for
  the directory and *nothing* for a file written inside it. Deleting a
  watched directory emitted `REMOVE` for it and left `WatchList()` empty —
  no error — so a watcher that never re-registers goes permanently deaf
  after an unmount/remount cycle, which on a NAS is a routine event.

`Add()` on a path that does not exist does error, which is only a note
about ordering: `cmd/server` already `MkdirAll`s `LIBRARY_DIR` before the
scan goroutine starts.

## Scope

In scope: a watcher in `internal/scanner` that triggers the existing sweep;
debouncing; watch re-registration; mount-type reporting and a delivery
self-test; configuration; the Unraid mount guidance in the README.

Out of scope:

- **Per-file scanning.** The watcher does not read, hash, or index the
  file an event names. It notes that *something* changed and lets `Scan`
  do what it already does correctly. Reusing the one code path is
  DESIGN.md's instruction, and it is also what makes the partial-file
  problem above a non-issue.
- **Shortening or replacing the periodic rescan.** It stays on, at its
  current interval, whether or not the watcher works. It is the mechanism.
- **Watching `COVERS_DIR`.** Derived data this app owns; nothing else
  writes there.
- **A UI indicator of watcher health.** It belongs with the scan-status
  surface the empty-library backlog item
  (`docs/backlog/2026090203-empty-library-scan-action.md`) sketches, not
  ahead of it. Startup logging is this step's whole reporting surface.

## Dependency: fsnotify

Add `github.com/fsnotify/fsnotify` (v1.10.1). Verified: it builds and runs
under `CGO_ENABLED=0`, and its only dependency is `golang.org/x/sys`,
which is already in `go.mod` as an indirect dependency at a newer version
(v0.47.0), so nothing is downgraded and no new transitive tree appears.

The alternative — calling `inotify_init1`/`inotify_add_watch` through
`golang.org/x/sys/unix` and adding no module at all — is roughly sixty
lines and was seriously considered, since this project ships a Linux
container and nothing else. It is rejected on one practical ground: the
development machine is macOS, so a Linux-only implementation needs a
build-tagged stub, and `go test ./...` on the dev machine would then
exercise the stub rather than the watcher. fsnotify covers both with one
code path. This is the same "one dependency, clearly justified" bar the
rest of `go.mod` meets.

## Design: a trigger, not a scanner

### One scan goroutine, woken two ways

`cmd/server` already runs exactly one scan goroutine: an initial sweep,
then `periodicScan`'s ticker loop. The watcher does not get its own
goroutine calling `Scan` — that would allow two concurrent sweeps over one
database. Instead `periodicScan` grows a third case:

```go
select {
case <-ticker.C:
        runScan(...)
case <-trigger:          // buffered, capacity 1
        runScan(...)
case <-ctx.Done():
        return
}
```

The watcher owns a goroutine that only ever does a non-blocking send into
`trigger`. Sweeps stay serialized by construction, with no mutex, and a
burst of events collapses into one pending wake-up. This is deliberately
the same poke-plus-ticker shape `docs/plans/2026090201-send-to-kindle.md`
gives the send worker; two background workers in one binary should not
have two different shapes.

### Settling before the poke

The watcher holds the debounce, not the scan loop:

- Any qualifying event resets a `settle` timer (`WATCH_SETTLE`, default
  `5s`). The poke happens when the timer fires — that is, once the
  directory has been quiet for five seconds.
- A **maximum delay** (`watchMaxDelay`, a constant, 60s) bounds the case
  where events never stop: a bulk import of several hundred books would
  otherwise reset the timer indefinitely and scan nothing until it
  finished. The first event in a quiet period starts this cap; whichever
  of the two fires first pokes the scan.

Note what the debounce is and is not for. It is not a correctness
mechanism: if the settle window elapses mid-copy, the sweep hashes a
partial file and indexes it, then the completing write changes size and
mtime, the next sweep re-hashes, and `CreateBookWithFile`'s
orphan-pruning replaces the partial book in the same transaction. The
index converges either way. The debounce exists so that the common case
does one sweep instead of three, and so the library grid does not flicker
a half-written book into view.

### Which events qualify

Reuse `matchedSuffix` — the watcher lives in `internal/scanner`, which is
where DESIGN.md puts it ("two entry points sharing one code path") and
where that helper already is. An event qualifies when:

- its name matches a supported suffix (`.epub`, `.fb2`, `.fb2.zip`), or
- it is a `CREATE` or `REMOVE` of a **directory** (the watch set changed),
  or
- it is a `REMOVE`/`RENAME` of any name (a file leaving is exactly the
  case missing-file reconciliation wants to hear about promptly).

Everything else — a `.part` file growing during a download, an editor's
swap file, `.DS_Store` — is ignored, so a slow download of `book.epub.part`
provokes nothing until it is renamed into place, which arrives as a
qualifying `CREATE`.

The delivery probe's own file (below) is excluded by name.

### The watch set, and keeping it

Watch the library root and every subdirectory beneath it. DESIGN.md calls
the library "a flat, unorganised pile of files", so in practice this is one
watch — but `Scan` walks recursively, and a watcher that silently ignored
a subdirectory would create a latency cliff nobody could see.

Re-register the whole set **after every sweep**, comparing against
`WatchList()` and adding what is missing. One rule covers every failure
mode measured above: a new subdirectory becomes watched, a watch dropped
by a vanished directory is restored when the directory returns, and an
unmount/remount cycle heals at the next tick instead of leaving the
watcher permanently deaf. A sweep already runs on a timer, so this needs
no scheduling of its own.

An `Add` that fails (most plausibly `ENOSPC` from
`fs.inotify.max_user_watches` on a deep tree) logs at Warn and does not
abort the rest — the sweep is the mechanism, and a missing watch costs
latency, nothing more.

## Reporting: say which mount this is, then prove it

Two startup steps, both cheap, both log-only.

**1. Name the filesystem.** Parse `/proc/self/mountinfo` for the mount
backing `LIBRARY_DIR` — the longest mount point that prefixes it — and log
its type at Info. The format is stable: fields up to a `-` separator, then
the fstype. Verified against the loopback mount, which reported
`fuse.rawBridge`; an Unraid user share reports `fuse.shfs`.

Log at Warn, naming the type, when it is one where changes commonly happen
behind the mount: any `fuse.*`, `nfs`/`nfs4`, `cifs`, `9p`. The message
should say what to do, not just what is true — that changes written
through the mount are seen, changes written behind it are not, and that
pointing the bind mount at the disk or cache path underneath the share
(`/mnt/cache/books`, `/mnt/diskN/books`) makes every change local. On a
non-Linux dev machine `/proc/self/mountinfo` is absent; that is a Debug
line, not a warning.

**2. Prove delivery.** Filesystem type is a heuristic — some FUSE
filesystems deliver, and the bind-mount hop into the container is one more
thing that could swallow events. So, once the watches are registered,
create `.watch-probe-<pid>` in the library root, wait up to two seconds
for an event naming it, and remove it. Silence logs one Warn:
live updates are not arriving on this mount, the periodic rescan is
covering it, and new books will take up to `SCAN_INTERVAL` to appear.

Three details this probe has to get right:

- **A read-only library is legitimate.** If the probe file cannot be
  created because the mount is read-only, that is not a watcher failure —
  log at Info that the probe was skipped and leave the watcher running.
  Any other create error is a Warn.
- **Clean up unconditionally**, including on the timeout path, so a
  restart loop cannot litter the library. The name carries the pid so two
  processes cannot collide on it.
- **The dotfile is inert to everything else.** `matchedSuffix` does not
  match it, so a sweep that catches it mid-probe ignores it, and the
  watcher's own filter excludes it by name so the probe cannot poke a
  sweep.

This is the one place the app writes into `LIBRARY_DIR`. DESIGN.md's
filesystem rule — "writes only ever create new paths" — is satisfied
literally, and the file is removed within two seconds.

## What the mover does, and why nothing needs to handle it

Worth stating because a reader will ask. When Unraid's mover relocates a
book from the cache pool to the array, its path through the user share is
unchanged, and size and mtime are preserved. The scanner's cheap
path+size+mtime check therefore skips it, no hash is recomputed, and no
row changes. The file's inode and physical disk both change, and neither
is anything this index stores. The move generates no events (it happens
behind the share), which is equally fine: there is nothing to react to.

The mover is a non-event for this design. That is a property worth keeping
— it is the reason not to add inode tracking or a device-id column later.

## Configuration

Two variables in `cmd/server`, matching the existing `envOrDefault` style:

- `WATCH_ENABLED` (default `true`, parsed with `strconv.ParseBool`) —
  turning it off skips the watcher, its probe and its logging entirely,
  leaving exactly today's behaviour. For a mount where the probe reports
  silence, this stops paying for watches that do nothing.
- `WATCH_SETTLE` (default `5s`) — the debounce. Rejected as negative the
  way `MISSING_GRACE` is; a zero value means "poke on the first event",
  which is legitimate for a local disk.

`SCAN_INTERVAL` keeps its 15m default and its meaning. The watcher does
not change it, because the rescan is not a fallback for the watcher — it
is the mechanism the watcher accelerates.

## Tests

`internal/scanner` (watcher-specific, all against a real temp directory,
which is the only place these semantics are real):

- A file copied in pokes the trigger exactly once after the settle window,
  not once per write.
- A burst of twenty files still pokes once.
- Events continuing past `watchMaxDelay` poke before they stop, so a bulk
  import is not starved.
- A `.part` file being written pokes nothing; renaming it to `.epub` pokes
  once.
- A file removed pokes (missing-file reconciliation depends on it).
- The probe file pokes nothing.
- Re-registration: create a subdirectory, and after the next
  re-registration a file created inside it pokes; delete a watched
  subdirectory and confirm re-registration does not error and the root
  watch survives.
- Cancellation returns from the watcher goroutine and closes the fsnotify
  watcher, with no goroutine left behind (`goleak` is not a dependency
  here — assert the returned channel closes).

Mount reporting (pure unit tests, no mounting): a table of
`mountinfo` lines against an expected `(mountpoint, fstype)`, including a
path under a nested mount, a line with optional fields before the `-`
separator, and a missing file (the macOS case) reporting "unknown"
without an error.

`cmd/server`: `WATCH_ENABLED=false` starts no watcher; a negative
`WATCH_SETTLE` is a startup error, like `MISSING_GRACE`; the watcher joins
the same bounded shutdown wait the scan loop uses (`waitForScan` —
generalised to `waitForBackground` by the send-to-Kindle step if that
lands first; if it has not, this step does not need the rename).

Deliberately not tested in CI: that events arrive over a FUSE or SMB
mount. That is what the probe is for, and it reports at runtime on the
machine that actually has the mount.

## CLAUDE.md

- `internal/scanner`: the watcher as a trigger rather than a second index
  path; the debounce and its max-delay cap; the qualifying-event filter
  and why `.part` files are excluded; re-registration after every sweep as
  the one rule covering new subdirectories, dropped watches and
  remounts; and the rule that the index's correctness never depends on an
  event arriving.
- `cmd/server`: `WATCH_ENABLED`/`WATCH_SETTLE`, the single scan goroutine
  woken by ticker or poke, and the startup mount-type log plus delivery
  probe.
- A sentence on the Unraid finding — through the mount is seen, behind it
  is not — since it is the reason the whole thing is shaped this way and
  is exactly what a future reader will otherwise rediscover.

## README

A short "Live updates" note under the existing sections: books normally
appear within seconds of landing in the library directory, a periodic
rescan catches everything regardless, and on a NAS share (Unraid's
`/mnt/user`, NFS, SMB) changes written behind the share are only caught by
the rescan — with the disk-path bind mount named as the way to get instant
updates. This is the one piece of deployment advice a self-hoster needs
and cannot derive.

## Verification

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...` clean, and
  `CGO_ENABLED=0 go build ./...` specifically, since a new dependency is
  the one thing that could break the static-binary property.
- Locally: start the server, copy an EPUB into `LIBRARY_DIR`, and confirm
  the log shows one scan a few seconds later and the book on the grid
  without waiting for the interval. Copy a 200MB file in over a slow pipe
  and confirm one sweep after it completes, not several during.
- Delete the library directory while running, recreate it, drop a file in,
  and confirm the watch was re-registered and the file is picked up —
  the remount case, which is where a naive watcher goes deaf.
- **On the real Unraid box, both mounts.** With the bind mount on
  `/mnt/user/books`: confirm the startup log names `fuse.shfs`, then check
  whether the probe reports delivery — copying a book to the share over
  SMB should be seen, while `cp` straight into `/mnt/disk1/books` should
  not be, and should still appear at the next rescan. With the bind mount
  moved to `/mnt/cache/books` or `/mnt/diskN/books`: confirm the probe
  passes and both routes are seen. Whichever the probe reports is the
  configuration the README should recommend first — the loopback test
  behind this plan is structurally the same as `shfs` but is not `shfs`,
  and this is the step that settles it.
