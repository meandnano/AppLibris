package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// watchMaxDelay bounds how long an unbroken stream of events can hold
	// the debounce open. Copying several hundred books in at once produces
	// events continuously, and a settle timer that resets on every one of
	// them would scan nothing until the whole import finished.
	watchMaxDelay = 60 * time.Second

	// watchRecheckInterval is how often Run looks for having gone deaf.
	// Losing every watch is the one failure the sweep-driven Refresh cannot
	// recover from on its own — see Run.
	watchRecheckInterval = 30 * time.Second

	// watchProbeTimeout is how long the startup delivery probe waits for
	// the event its own file should produce. Generous for an inotify
	// round trip, which is sub-millisecond; the point is to distinguish
	// "slow" from "never", and never is what a FUSE or network mount
	// returns.
	watchProbeTimeout = 2 * time.Second
)

// Watcher turns filesystem activity under the library directory into pokes
// on a trigger channel. It is emphatically not a second way to index a
// book: it never reads, hashes or parses the file an event names. DESIGN.md
// makes the periodic rescan the mechanism and the watcher an optimisation,
// so the only thing a watcher failure can cost is latency — a book waits
// for the next sweep instead of appearing within seconds.
//
// That framing is what makes the debounce safe. An event says something
// changed, not that it finished changing: a copy in progress fires CREATE
// long before its last byte lands. Scanning on the event itself would hash
// a partial file. Scanning after a quiet window usually avoids that, and
// when it doesn't the index still converges — the completing write changes
// size and mtime, the next sweep re-hashes, and CreateBookWithFile's
// orphan-pruning replaces the partial book in the same transaction.
type Watcher struct {
	fsw        *fsnotify.Watcher
	libraryDir string
	settle     time.Duration
	// maxDelay is watchMaxDelay, and recheck is watchRecheckInterval, as
	// fields so tests can shorten them rather than waiting out the real
	// ones.
	maxDelay time.Duration
	recheck  time.Duration
	trigger  chan<- struct{}
	// probeName is set by the delivery probe, on Run's goroutine, before any
	// event is read; only qualifies reads it, from that same goroutine.
	probeName string

	// mu guards registered, which Refresh mutates from the scan goroutine
	// and from Run's recheck.
	mu sync.Mutex
	// registered records which directory each watch is actually attached to.
	registered map[string]watchKey
}

// watchKey identifies the directory a watch really covers. inotify watches
// an inode, but fsnotify files each watch under the pathname it was
// registered with, and the two stop agreeing the moment a directory moves:
// the watch follows the inode out of the tree while the name it is filed
// under can be taken by a different directory entirely. Comparing identity
// rather than name is what keeps a replacement from being mistaken for the
// original and left unwatched.
type watchKey struct {
	dev uint64
	ino uint64
}

// watchKeyFor reads a directory's identity from an already-stat'd entry.
func watchKeyFor(info fs.FileInfo) (watchKey, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return watchKey{}, false
	}
	return watchKey{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}, true
}

// NewWatcher watches libraryDir and every directory beneath it, poking
// trigger once the tree has been quiet for settle. trigger should be
// buffered with capacity 1: pokes are non-blocking, so a burst collapses
// into one pending wake-up rather than queueing sweeps.
//
// Failing to watch the library root is an error; failing to watch a
// subdirectory under it is not (see Refresh).
func NewWatcher(libraryDir string, settle time.Duration, trigger chan<- struct{}) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}
	if err := fsw.Add(libraryDir); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watch %s: %w", libraryDir, err)
	}

	w := &Watcher{
		fsw:        fsw,
		libraryDir: libraryDir,
		settle:     settle,
		maxDelay:   watchMaxDelay,
		recheck:    watchRecheckInterval,
		trigger:    trigger,
		registered: make(map[string]watchKey),
	}
	w.logMount()
	w.Refresh()
	return w, nil
}

// Refresh brings the watch set back in line with the directory tree. Call
// it after each sweep.
//
// It compares each directory's identity, not just its name, against what is
// registered. Four failure modes measured on real filesystems need that:
//
//   - inotify is not recursive, so a subdirectory created since the last
//     sweep has no watch of its own;
//   - a watch is dropped silently when its directory is deleted, and must
//     be restored if the directory comes back;
//   - an unmount/remount drops every watch without reporting an error;
//   - and a directory that *moves out* of the library takes its watch with
//     it, leaving the watch filed under a name that something else can then
//     occupy. Trusting the name alone would skip the replacement for good —
//     books written there produce no events, refresh after refresh — while
//     the departed inode keeps delivering events under a path it no longer
//     owns.
//
// Both tests are needed, because each covers the other's blind spot.
// Identity alone is not enough: a filesystem is free to hand a recreated
// directory the inode number the deleted one had (ext4 does, reproducibly),
// which reads as "unchanged" for a watch the kernel has already dropped.
// The name alone is not enough either — that is the moved-directory case
// above. Together they are exact, because an inode number is only reusable
// once its inode is freed, and freeing it drops the watch filed under that
// name: a live watch plus a matching identity cannot be a coincidence.
func (w *Watcher) Refresh() {
	w.mu.Lock()
	defer w.mu.Unlock()

	// What fsnotify still holds a watch for, by the name it was registered
	// under — the authority on whether a watch exists at all.
	watched := make(map[string]bool, len(w.registered))
	for _, path := range w.fsw.WatchList() {
		watched[path] = true
	}

	seen := make(map[string]bool, len(w.registered))
	err := filepath.WalkDir(w.libraryDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree costs its own watches and nothing
			// else, the same way it costs its own files in a sweep.
			slog.Debug("watch refresh skipped a path", "path", path, "error", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		seen[path] = true

		info, err := d.Info()
		if err != nil {
			// Vanished between the readdir and here; the next refresh has it.
			return nil
		}
		key, haveKey := watchKeyFor(info)
		switch {
		case !watched[path]:
			// Nothing is filed under this name: a directory created since
			// the last refresh, or one whose watch went with it when it was
			// deleted and needs restoring now that it is back.
		case !haveKey:
			// No identity to compare on this platform, so the name is all
			// there is to go on.
			return nil
		case w.registered[path] == key:
			return nil // same directory, still watched
		default:
			// The name now belongs to a different directory. Release what is
			// filed under it *before* re-adding, while the descriptor is
			// still reachable: adding a path whose inode has changed drops
			// the superseded watch from fsnotify's own map — enough to stop
			// its events being delivered — but never calls inotify_rm_watch,
			// so the kernel goes on holding it and no later Remove or
			// WatchList can find it again. Each replacement would otherwise
			// spend another watch from a per-user budget a large library is
			// already sized against.
			//
			// Forget the stale identity now rather than after a successful
			// Add: if the Add fails, a registration that still matches would
			// make the next refresh think this name was fine.
			w.fsw.Remove(path)
			delete(w.registered, path)
		}
		if err := w.fsw.Add(path); err != nil {
			if errors.Is(err, fsnotify.ErrClosed) {
				// Shutdown cancels the watcher and the scan loop together,
				// and Run closes the handle on its way out — so a sweep
				// still finishing can land here with nothing left to
				// register, and would otherwise warn once per directory.
				return fs.SkipAll
			}
			// Most plausibly the inotify watch limit on a deep tree. A
			// missing watch costs latency in that subtree, not correctness,
			// so keep going.
			slog.Warn("could not watch directory", "path", path, "error", err)
			return nil
		}
		if haveKey {
			w.registered[path] = key
		}
		return nil
	})
	if err != nil {
		slog.Warn("watch refresh", "library_dir", w.libraryDir, "error", err)
	}

	// Directories that have left the tree: drop their watches so a departed
	// inode cannot keep poking sweeps from outside the library.
	for path := range w.registered {
		if !seen[path] {
			w.fsw.Remove(path)
			delete(w.registered, path)
		}
	}
}

// Run delivers pokes until ctx is cancelled, then closes the underlying
// watcher. It blocks, so callers run it on its own goroutine.
func (w *Watcher) Run(ctx context.Context) {
	defer w.fsw.Close()

	// The probe consumes events while it waits, so it reports whether it
	// swallowed a real one — that must still start the debounce rather
	// than being lost to the startup check.
	pending := w.probe(ctx)

	var (
		timer  *time.Timer
		timerC <-chan time.Time
		first  time.Time
	)
	arm := func() {
		if first.IsZero() {
			first = time.Now()
		}
		if timer == nil {
			timer = time.NewTimer(w.settle)
		} else {
			timer.Stop()
			timer.Reset(w.settle)
		}
		timerC = timer.C
	}
	fire := func() {
		first = time.Time{}
		timerC = nil
		if timer != nil {
			timer.Stop()
		}
		w.poke()
	}

	// Re-registering after each sweep covers a subdirectory appearing or a
	// single watch being dropped, because a sweep still gets poked. It
	// cannot cover losing *every* watch: the library directory being
	// replaced — an unmount and remount, or a share coming back on a
	// different inode — leaves nothing able to poke the sweep that would
	// have called Refresh, so the watcher stays deaf until the periodic
	// rescan happens along, which may be a quarter of an hour of silently
	// missing every change. Measured, not theorised: deleting and
	// recreating the library directory produced exactly that.
	recheck := time.NewTicker(w.recheck)
	defer recheck.Stop()

	if pending {
		arm()
	}
	for {
		select {
		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if !w.qualifies(event) {
				continue
			}
			// One timer covers both bounds. The settle window handles a
			// burst that ends; this handles one that doesn't, poking on
			// the first event past the cap rather than waiting for a gap
			// that may never come. They cannot both be needed: events are
			// either still arriving or they are not.
			if !first.IsZero() && time.Since(first) >= w.maxDelay {
				fire()
				continue
			}
			arm()
		case <-timerC:
			fire()
		case <-recheck.C:
			// Cheap while healthy: a length check under the watcher's own
			// lock, no walk. Only once there is nothing left to lose is it
			// worth trying to rebuild the set.
			if len(w.fsw.WatchList()) > 0 {
				continue
			}
			w.Refresh()
			if len(w.fsw.WatchList()) > 0 {
				slog.Info("library directory is watchable again", "library_dir", w.libraryDir)
				// Whatever changed while we could not see it is still
				// unindexed, so sweep rather than wait for the next event.
				fire()
			}
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("watcher", "error", err)
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		}
	}
}

// poke wakes the scan loop without ever blocking on it. A poke that finds
// the buffer full is dropped on purpose: a wake-up is already pending, and
// two sweeps would see the same directory.
func (w *Watcher) poke() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// qualifies reports whether an event is worth a sweep.
func (w *Watcher) qualifies(event fsnotify.Event) bool {
	name := filepath.Base(event.Name)
	if w.probeName != "" && name == w.probeName {
		// The probe must not be able to trigger the work it is testing.
		return false
	}
	if matchedSuffix(name) != "" {
		return true
	}
	// A removal or a rename names something that may already be gone, so
	// the suffix is not reliably informative and a book leaving is exactly
	// what missing-file reconciliation wants to hear about promptly.
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		return true
	}
	// A new directory changes the watch set; the sweep that follows is
	// what registers it.
	if event.Has(fsnotify.Create) {
		if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
			return true
		}
	}
	// Everything else — a .part file growing during a download, an
	// editor's swap file — is noise. A download becomes interesting when
	// it is renamed into place, which arrives as a qualifying event.
	return false
}

// probe proves that events actually arrive on this mount, which is not
// something the watcher can otherwise find out: adding a watch to a
// filesystem that will never deliver an event succeeds silently, so a dead
// watcher is indistinguishable from an idle one. Verified against a FUSE
// passthrough, where Add returned no error and no event ever came.
//
// It reports whether it consumed a qualifying event while waiting.
func (w *Watcher) probe(ctx context.Context) (pending bool) {
	// Created exclusively, under a name nothing else can already hold.
	// os.Create would truncate whatever is at the path and follow a symlink
	// through to its target — and in a container the process is PID 1, so a
	// name built from the pid is guessable. A library file must not be
	// collateral damage of a diagnostic; DESIGN.md's rule for this directory
	// is that writes only ever create new paths.
	f, err := os.CreateTemp(w.libraryDir, ".watch-probe-*")
	if err != nil {
		// A read-only library mount is a legitimate deployment, not a
		// broken watcher: say so quietly and carry on.
		if errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission) {
			slog.Info("watch delivery probe skipped: library directory is not writable",
				"library_dir", w.libraryDir)
		} else {
			slog.Warn("watch delivery probe could not create its file",
				"library_dir", w.libraryDir, "error", err)
		}
		return false
	}
	path := f.Name()
	f.Close()
	// Set before any event is read, so the file this probe just made cannot
	// be mistaken for a book and poke the very sweep it is testing.
	w.probeName = filepath.Base(path)
	// Unconditional, including on the timeout path: a restart loop must not
	// litter the library. Only ever the file this probe created, which is
	// what exclusive creation buys.
	defer func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("watch delivery probe could not remove its file", "path", path, "error", err)
		}
	}()

	deadline := time.NewTimer(watchProbeTimeout)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-w.fsw.Events:
			if !ok {
				return pending
			}
			if filepath.Base(event.Name) == w.probeName {
				slog.Info("watching library for changes", "library_dir", w.libraryDir)
				return pending
			}
			if w.qualifies(event) {
				pending = true
			}
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return pending
			}
			slog.Warn("watcher", "error", err)
		case <-deadline.C:
			slog.Warn("live updates are not arriving on this mount; new books will appear at the next periodic rescan instead",
				"library_dir", w.libraryDir)
			return pending
		case <-ctx.Done():
			return pending
		}
	}
}

// logMount says which filesystem the library is on, and warns when it is
// one where changes routinely happen behind the mount rather than through
// it — the case that decides whether this watcher can see anything.
func (w *Watcher) logMount() {
	mountPoint, fsType, err := mountFor(w.libraryDir)
	if err != nil {
		// No /proc/self/mountinfo: a non-Linux development machine. Not
		// worth a warning.
		slog.Debug("could not determine the library's filesystem", "error", err)
		return
	}
	if !mountHidesChanges(fsType) {
		slog.Info("library filesystem", "mount", mountPoint, "type", fsType)
		return
	}
	slog.Warn("library is on a filesystem where changes made behind the mount are invisible to the watcher: "+
		"writes through this path are seen, writes straight to the underlying disk or share are not, "+
		"and only the periodic rescan catches those. Bind-mounting the underlying disk path instead "+
		"(an Unraid /mnt/cache/... or /mnt/diskN/... rather than /mnt/user/...) makes every change local",
		"mount", mountPoint, "type", fsType)
}
