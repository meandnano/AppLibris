package scanner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testSettle is short enough to keep the suite quick and long enough that
// the events of one burst land inside a single window.
const testSettle = 80 * time.Millisecond

// startWatcher runs a watcher over dir and returns its trigger channel.
func startWatcher(t *testing.T, dir string, settle time.Duration) chan struct{} {
	t.Helper()

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, settle, trigger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("watcher did not return after its context was cancelled")
		}
	})

	// Run's first act is the delivery probe, which consumes events for up
	// to watchProbeTimeout. Wait for it to finish so a test's own writes
	// aren't attributed to it.
	waitForPokeToSettle(t, trigger)
	return trigger
}

// waitForPokeToSettle waits out the startup probe and drains any poke it
// left behind, so each test starts from a quiet watcher.
func waitForPokeToSettle(t *testing.T, trigger chan struct{}) {
	t.Helper()
	// The probe confirms delivery as soon as its own event arrives, which
	// on a local filesystem is immediate.
	time.Sleep(150 * time.Millisecond)
	select {
	case <-trigger:
	default:
	}
}

// awaitPoke waits for one poke, failing if none arrives.
func awaitPoke(t *testing.T, trigger chan struct{}, why string) {
	t.Helper()
	select {
	case <-trigger:
	case <-time.After(5 * time.Second):
		t.Fatalf("no sweep was triggered: %s", why)
	}
}

// expectNoPoke asserts nothing pokes within quiet, which callers size
// against whatever settle window their watcher is using.
func expectNoPoke(t *testing.T, trigger chan struct{}, quiet time.Duration, why string) {
	t.Helper()
	select {
	case <-trigger:
		t.Fatalf("a sweep was triggered but should not have been: %s", why)
	case <-time.After(quiet):
	}
}

// A copy produces CREATE then one or more WRITEs, and a burst produces one
// pair per file. The whole point of the debounce is that the scan loop is
// woken once for the lot, not once per event.
//
// Counting requires draining continuously rather than reading the trigger
// once: the channel holds a single poke and drops the rest, so an
// undebounced watcher looks identical to a debounced one from the outside
// unless something is emptying it. Draining also makes the count
// independent of how fast the writes land — twenty pokes are twenty pokes
// whether the burst takes a millisecond or a second — which matters
// because CI runs this under -race on four shared cores.
func TestWatcherPokesOnceForABurst(t *testing.T) {
	// Comfortably longer than twenty small writes need even under
	// contention, so a straggling burst can't debounce twice for a
	// legitimate reason and fail a test that isn't about that.
	const burstSettle = 500 * time.Millisecond

	dir := t.TempDir()
	trigger := startWatcher(t, dir, burstSettle)

	var pokes atomic.Int64
	stop, drained := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-trigger:
				pokes.Add(1)
			case <-stop:
				return
			}
		}
	}()

	for i := range 20 {
		path := filepath.Join(dir, fmt.Sprintf("book-%02d.epub", i))
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	time.Sleep(burstSettle * 3)
	close(stop)
	<-drained

	if got := pokes.Load(); got != 1 {
		t.Errorf("a burst of twenty books triggered %d sweeps, want exactly 1", got)
	}
}

// A stream of events that never pauses would hold a naive debounce open
// forever. The cap pokes anyway.
func TestWatcherPokesWhileEventsKeepArriving(t *testing.T) {
	dir := t.TempDir()

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, time.Hour, trigger) // a settle window that will never elapse
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.maxDelay = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForPokeToSettle(t, trigger)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("bulk-%03d.epub", i)), []byte("x"), 0o644)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	awaitPoke(t, trigger, "events kept arriving past the maximum delay")
}

// A download in progress is not a book. It becomes one when it is renamed
// into place, and that is the event worth a sweep.
func TestWatcherIgnoresPartialDownloadsUntilRenamed(t *testing.T) {
	dir := t.TempDir()
	trigger := startWatcher(t, dir, testSettle)

	partial := filepath.Join(dir, "book.epub.part")
	if err := os.WriteFile(partial, []byte("not finished"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	expectNoPoke(t, trigger, testSettle*4, "a .part file is not a book yet")

	if err := os.Rename(partial, filepath.Join(dir, "book.epub")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	awaitPoke(t, trigger, "the download was renamed into place")
}

// A book leaving is what missing-file reconciliation reacts to, so it is
// worth waking the sweep for.
func TestWatcherPokesOnRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.epub")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	trigger := startWatcher(t, dir, testSettle)
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	awaitPoke(t, trigger, "a book was removed")
}

// The probe writes into the library to prove events arrive. It must not be
// able to trigger the very work it is testing.
func TestWatcherProbeDoesNotPoke(t *testing.T) {
	dir := t.TempDir()

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, testSettle, trigger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	expectNoPoke(t, trigger, testSettle*4, "the delivery probe's own file must be inert")

	// And it cleans up after itself, so a restart loop can't litter.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".watch-probe-") {
			t.Errorf("probe file %q was left behind", e.Name())
		}
	}
}

// inotify is not recursive: a watch on the parent says nothing about files
// created inside a new subdirectory. Refresh is what closes that gap, and
// the scan loop calls it after every sweep.
func TestWatcherRefreshPicksUpNewSubdirectories(t *testing.T) {
	dir := t.TempDir()

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, testSettle, trigger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForPokeToSettle(t, trigger)

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// Creating the directory is itself a qualifying event; drain it, since
	// what this test is about is the file inside.
	awaitPoke(t, trigger, "a subdirectory was created")

	w.Refresh()

	if err := os.WriteFile(filepath.Join(sub, "nested.epub"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	awaitPoke(t, trigger, "a book was created inside a newly watched subdirectory")
}

// A deleted directory has its watch dropped silently — no error, just
// silence from then on. Refresh restores it when the directory returns,
// which is what keeps an unmount/remount cycle from deafening the watcher.
func TestWatcherRefreshRestoresAWatchAfterTheDirectoryReturns(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, testSettle, trigger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForPokeToSettle(t, trigger)

	if err := os.RemoveAll(sub); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	awaitPoke(t, trigger, "a watched subdirectory was removed")

	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	awaitPoke(t, trigger, "the subdirectory came back")
	w.Refresh()

	if err := os.WriteFile(filepath.Join(sub, "back.epub"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	awaitPoke(t, trigger, "the restored subdirectory is watched again")
}

// The failure the sweep-driven Refresh cannot reach on its own: replacing
// the library directory drops every watch, and with nothing left to poke a
// sweep, nothing would ever call Refresh again. Found by deleting and
// recreating the directory under a running server, which went silent until
// the periodic rescan.
//
// Note that no Refresh is called here — recovering without one is the whole
// point.
func TestWatcherRecoversAfterTheLibraryDirectoryIsReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "library")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, testSettle, trigger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.recheck = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForPokeToSettle(t, trigger)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	awaitPoke(t, trigger, "the library directory was removed")

	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// Coming back is itself worth a sweep: whatever changed while the
	// watcher was deaf is still unindexed.
	awaitPoke(t, trigger, "the library directory came back")

	if err := os.WriteFile(filepath.Join(dir, "after.epub"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	awaitPoke(t, trigger, "a book arrived after the directory was replaced")
}

// The scan loop's trigger has capacity 1 and the watcher never blocks on
// it: a poke arriving while one is already pending is dropped, because the
// sweep it would have asked for is the sweep already about to run.
func TestWatcherPokeNeverBlocks(t *testing.T) {
	trigger := make(chan struct{}, 1)
	w := &Watcher{trigger: trigger}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 {
			w.poke()
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poke blocked when the trigger was already full")
	}
	if len(trigger) != 1 {
		t.Errorf("trigger holds %d pokes, want exactly 1", len(trigger))
	}
}

func TestNewWatcherRejectsAMissingDirectory(t *testing.T) {
	if _, err := NewWatcher(filepath.Join(t.TempDir(), "nope"), testSettle, make(chan struct{}, 1)); err == nil {
		t.Error("NewWatcher on a missing directory returned no error")
	}
}

// Shutdown cancels the watcher and the scan loop together. Run closes the
// fsnotify handle on its way out, but a sweep that was still finishing then
// calls Refresh — and a closed watcher reports an empty WatchList, so a
// naive Refresh tries to re-add every directory in the tree and warns once
// per failure. Shutdown should be quiet.
func TestWatcherRefreshIsQuietOnceTheWatcherIsClosed(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one", "two", "three"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
	}

	w, err := NewWatcher(dir, testSettle, make(chan struct{}, 1))
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	// What Run's deferred Close does when its context ends.
	if err := w.fsw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	logs := captureLogs(t)
	w.Refresh()

	if got := logs.String(); strings.Contains(got, "level=WARN") {
		t.Errorf("Refresh on a closed watcher logged warnings:\n%s", got)
	}
}

// captureLogs redirects the default logger for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// The probe writes into the live library, so it must only ever create a new
// path — os.Create truncates whatever is already at the name and follows
// symlinks through to the target. In a container the process is PID 1, so a
// fixed name is guessable: a symlink at that name pointing at a book would
// leave the book empty.
func TestWatcherProbeDoesNotDisturbAnExistingPath(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.epub")
	const content = "the whole book"
	if err := os.WriteFile(book, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	decoy := filepath.Join(dir, fmt.Sprintf(".watch-probe-%d", os.Getpid()))
	if err := os.Symlink(book, decoy); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, testSettle, trigger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForPokeToSettle(t, trigger)

	got, err := os.ReadFile(book)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != content {
		t.Errorf("the probe modified an existing book: content = %q, want %q", got, content)
	}
	if _, err := os.Lstat(decoy); err != nil {
		t.Errorf("the probe removed a path it did not create: %v", err)
	}
}

// A watch is attached to an inode, but fsnotify files it under the pathname
// used to register it, and the two stop agreeing the moment a directory
// moves: the watch follows the inode out of the library while the name it is
// filed under can be taken by a different directory. Trusting the name alone
// then skips the replacement for good — books written there produce no
// events, refresh after refresh.
func TestWatcherRefreshRewatchesADirectoryReplacedAfterAMove(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	series := filepath.Join(dir, "author", "series")
	if err := os.MkdirAll(series, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	trigger := make(chan struct{}, 1)
	w, err := NewWatcher(dir, testSettle, trigger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForPokeToSettle(t, trigger)

	// The parent leaves the library, taking the descendant watch's inode
	// with it, and a fresh tree takes the same names.
	if err := os.Rename(filepath.Join(dir, "author"), filepath.Join(outside, "author")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.MkdirAll(series, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	awaitPoke(t, trigger, "the tree changed under the library")
	w.Refresh()

	if err := os.WriteFile(filepath.Join(series, "new.epub"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	awaitPoke(t, trigger, "a book arrived in the directory that replaced the moved one")
}

// The other half of a moved directory: its own watch is dropped by the
// move, but a watch on a directory *beneath* it is not — that inode never
// moved relative to its own watch. It stays live, still filed under the
// name it had inside the library, and goes on reporting files created
// outside the library entirely as though they had arrived in it.
//
// Both endings need covering because different code handles them: a name
// something else now occupies is re-registered, while a name that has left
// the tree is only unwatched by the sweep for departed registrations.
func TestWatcherRefreshStopsWatchingADirectoryThatLeftTheLibrary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
	}{
		{"the names are taken by a fresh tree", true},
		{"the names are simply gone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			series := filepath.Join(dir, "author", "series")
			if err := os.MkdirAll(series, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			trigger := make(chan struct{}, 1)
			w, err := NewWatcher(dir, testSettle, trigger)
			if err != nil {
				t.Fatalf("NewWatcher: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go w.Run(ctx)
			waitForPokeToSettle(t, trigger)

			if err := os.Rename(filepath.Join(dir, "author"), filepath.Join(outside, "author")); err != nil {
				t.Fatalf("Rename: %v", err)
			}
			if tc.replace {
				if err := os.MkdirAll(series, 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
			}
			awaitPoke(t, trigger, "the tree changed under the library")
			w.Refresh()
			waitForPokeToSettle(t, trigger)

			moved := filepath.Join(outside, "author", "series")
			if err := os.WriteFile(filepath.Join(moved, "gone.epub"), []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			expectNoPoke(t, trigger, 2*testSettle, "a book arrived in a directory that is no longer in the library")

			if !tc.replace {
				return
			}
			// The replacement is watched, so this is not simply a watcher
			// that has stopped reporting anything.
			if err := os.WriteFile(filepath.Join(series, "here.epub"), []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			awaitPoke(t, trigger, "a book arrived in the directory now at that name")
		})
	}
}

// inotifyWatchCount reports how many inotify watches this process holds in
// the kernel, across every inotify descriptor it has open.
//
// /proc is the only place a leaked watch is visible. fsnotify's WatchList
// reports its own bookkeeping, and a watch that has fallen out of that map
// while staying alive in the kernel is exactly the failure being tested —
// so asking the library would answer with the assumption under test.
func inotifyWatchCount(t *testing.T) int {
	t.Helper()
	const fdinfo = "/proc/self/fdinfo"
	entries, err := os.ReadDir(fdinfo)
	if err != nil {
		t.Skipf("no %s on this platform: %v", fdinfo, err)
	}
	total := 0
	for _, e := range entries {
		// A descriptor can close under us — including the one this read
		// opens — so a vanished entry is ordinary, not a failure.
		b, err := os.ReadFile(filepath.Join(fdinfo, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "inotify ") {
				total++
			}
		}
	}
	return total
}

// Refresh must release the watches it stops using, in the kernel and not
// merely in fsnotify's map.
//
// fsnotify's updatePath drops a superseded descriptor from its own map
// without calling inotify_rm_watch, so re-adding a path whose inode changed
// leaves the previous watch alive and no longer reachable through Remove or
// WatchList. Events from it are discarded, which is why the delivery
// assertions elsewhere pass either way — but each round spends another
// watch from a per-user budget a library of any size is already sized
// against.
func TestWatcherRefreshReleasesTheKernelWatchesItStopsUsing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
	}{
		{"a name taken by a fresh tree", true},
		{"a name that leaves the tree", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			series := filepath.Join(dir, "author", "series")
			if err := os.MkdirAll(series, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			trigger := make(chan struct{}, 1)
			w, err := NewWatcher(dir, testSettle, trigger)
			if err != nil {
				t.Fatalf("NewWatcher: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go w.Run(ctx)
			waitForPokeToSettle(t, trigger)

			baseline := inotifyWatchCount(t)

			// The departed trees are kept, so nothing is released by their
			// inodes being freed: only an explicit removal can account for
			// them.
			const rounds = 8
			for i := range rounds {
				author := filepath.Join(dir, "author")
				if !tc.replace {
					// Nothing takes the name back, so each round needs a
					// tree of its own to watch and then lose.
					author = filepath.Join(dir, fmt.Sprintf("author%d", i))
					if err := os.MkdirAll(filepath.Join(author, "series"), 0o755); err != nil {
						t.Fatalf("MkdirAll: %v", err)
					}
					w.Refresh()
				}
				if err := os.Rename(author, filepath.Join(outside, fmt.Sprintf("author%d", i))); err != nil {
					t.Fatalf("Rename: %v", err)
				}
				if tc.replace {
					if err := os.MkdirAll(series, 0o755); err != nil {
						t.Fatalf("MkdirAll: %v", err)
					}
				}
				w.Refresh()
				// The moved directory's own watch is released by fsnotify
				// from its event loop, on IN_MOVE_SELF, so let that land
				// before counting.
				waitForPokeToSettle(t, trigger)
			}

			after := inotifyWatchCount(t)
			// The library is the shape it started as, so the count should be
			// too. A couple of watches of slack keeps this about a per-round
			// leak rather than the exact bookkeeping of one refresh.
			if after > baseline+2 {
				t.Errorf("inotify watches grew from %d to %d over %d rounds: "+
					"the watches Refresh stopped using were not released", baseline, after, rounds)
			}
		})
	}
}
