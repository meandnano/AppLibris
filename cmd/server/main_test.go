package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"library/internal/storage"
)

// A serving failure (an occupied address, here) must still cancel and wait
// for the background scan before closing the database — not race ahead of
// it. There's no way to directly observe "no goroutine leaked" without
// exposing internal state, but a hang in that cleanup would blow the bound
// below: the scan against an empty library finishes near-instantly, so run
// must return promptly, not just eventually within its own 10s budget.
func TestRunReturnsPromptlyOnOccupiedAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer l.Close()

	t.Setenv("ADDR", l.Addr().String())
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "library.db"))
	t.Setenv("LIBRARY_DIR", t.TempDir())
	t.Setenv("COVERS_DIR", t.TempDir())

	done := make(chan error, 1)
	go func() { done <- run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run: want an error from the occupied address, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of an occupied address — scan cleanup may be hanging")
	}
}

// The same promptness bound as above, but with sending configured: the
// worker also runs on scanCtx and must join the same bounded
// waitForBackground wait, or a serving failure would hang shutdown behind
// an idle worker that never notices cancellation.
func TestRunReturnsPromptlyOnOccupiedAddressWithSendingEnabled(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer l.Close()

	t.Setenv("ADDR", l.Addr().String())
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "library.db"))
	t.Setenv("LIBRARY_DIR", t.TempDir())
	t.Setenv("COVERS_DIR", t.TempDir())
	t.Setenv("RESEND_API_KEY", "test-key")
	t.Setenv("RESEND_FROM", "kindle@example.com")

	done := make(chan error, 1)
	go func() { done <- run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run: want an error from the occupied address, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of an occupied address — sender worker cleanup may be hanging")
	}
}

// The same promptness bound again, with the watcher explicitly on: its
// startup delivery probe waits for an event, and a probe that blocked
// rather than timing out would show up here as a stalled shutdown.
//
// Note what this does not pin: that the watcher joins waitForBackground.
// Dropping it from that wait passes this test, because unlike the scan loop
// and the sender the watcher touches no database — it only pokes a channel
// and closes its own handle — so there is no ordering against db.Close to
// observe, and its goroutine exits on the same cancelled context either
// way. It is waited for symmetry and to avoid leaving a goroutine behind in
// an embedded caller, not for a property a test can catch.
func TestRunReturnsPromptlyWithTheWatcherRunning(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer l.Close()

	t.Setenv("ADDR", l.Addr().String())
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "library.db"))
	t.Setenv("LIBRARY_DIR", t.TempDir())
	t.Setenv("COVERS_DIR", t.TempDir())
	t.Setenv("WATCH_ENABLED", "true")

	done := make(chan error, 1)
	go func() { done <- run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run: want an error from the occupied address, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s with the watcher running")
	}
}

// Turning the watcher off leaves exactly the previous behaviour, which is
// the point of the switch: on a mount where the delivery probe reports
// silence, there is no reason to pay for watches that do nothing.
func TestRunWithTheWatcherDisabled(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer l.Close()

	t.Setenv("ADDR", l.Addr().String())
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "library.db"))
	t.Setenv("LIBRARY_DIR", t.TempDir())
	t.Setenv("COVERS_DIR", t.TempDir())
	t.Setenv("WATCH_ENABLED", "false")

	done := make(chan error, 1)
	go func() { done <- run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run: want an error from the occupied address, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s with the watcher disabled")
	}
}

// A negative settle window would poke on an event that hasn't happened yet;
// like MISSING_GRACE, it is rejected at startup rather than quietly
// producing nonsense. A misspelled METADATA_PROVIDERS entry gets the same
// treatment for a different reason: silently running with fewer providers
// than configured is the kind of thing nobody notices for months.
func TestRunRejectsBadWatchConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"negative settle", "WATCH_SETTLE", "-5s"},
		{"unparseable settle", "WATCH_SETTLE", "soon"},
		{"unparseable enabled", "WATCH_ENABLED", "sometimes"},
		{"unparseable fetch metadata requirement", "REQUIRE_FETCH_METADATA", "maybe"},
		{"unknown metadata provider", "METADATA_PROVIDERS", "bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "library.db"))
			t.Setenv("LIBRARY_DIR", t.TempDir())
			t.Setenv("COVERS_DIR", t.TempDir())
			t.Setenv(tc.key, tc.value)

			if err := run(context.Background()); err == nil {
				t.Errorf("run with %s=%q returned no error", tc.key, tc.value)
			}
		})
	}
}

// The watcher's trigger has capacity 1 and holds a poke until the scan loop
// takes it, so at shutdown both that channel and ctx.Done() are ready at
// once — and Go's select picks between ready cases at random. Falling
// through to the sweep on the trigger branch runs Scan against a dead
// context, which fails and logs at ERROR: a clean shutdown that looks like
// a fault, roughly half the time. Before the watcher this was near
// impossible, since only the 15-minute ticker could be ready.
func TestPeriodicScanDoesNotSweepOnACancelledContext(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	libraryDir, coversDir := t.TempDir(), t.TempDir()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// One run is a coin flip; fifty makes missing the bug essentially
	// impossible.
	for range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		trigger := make(chan struct{}, 1)
		trigger <- struct{}{} // a poke still pending, exactly as at shutdown

		periodicScan(ctx, db, libraryDir, coversDir, time.Hour, time.Hour, trigger, nil, nil)
	}

	if got := logs.String(); strings.Contains(got, "level=ERROR") {
		t.Errorf("a cancelled shutdown swept anyway and logged an error:\n%s", got)
	}
}

// METADATA_PROVIDERS= (set, empty) is the documented way to run with no
// outbound requests at all, which only works because it stays distinct from
// the variable being unset. Collapsing the two — the pattern every other
// env var in run() uses — would silently give a deployment that asked for
// no outbound calls the default provider pair.
func TestMetadataProviderNamesDistinguishesEmptyFromUnset(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		want  []string
		isSet bool
	}{
		{name: "unset uses the default pair", want: []string{"openlibrary", "googlebooks"}},
		{name: "set but empty disables enrichment", env: map[string]string{"METADATA_PROVIDERS": ""}, want: nil},
		{name: "one name", env: map[string]string{"METADATA_PROVIDERS": "openlibrary"}, want: []string{"openlibrary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := metadataProviderNames(func(key string) (string, bool) {
				v, ok := tc.env[key]
				return v, ok
			})
			if len(got) != len(tc.want) {
				t.Fatalf("names = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("names[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The setting's whole effect is which wrapper goes around the routes, and
// the two wrappers answer a header-less POST oppositely. Pinning both
// directions here is what stops a swapped branch — the strict default
// silently becoming the permissive one — from passing every other test.
func TestFetchMetadataGuardMapsTheSettingToTheRightWrapper(t *testing.T) {
	for _, tc := range []struct {
		name     string
		require  bool
		wantCode int
		wantNext bool
	}{
		{"required refuses", true, http.StatusForbidden, false},
		{"not required admits", false, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := fetchMetadataGuard(tc.require, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/books/1/enrich", nil))

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if called != tc.wantNext {
				t.Errorf("next called = %v, want %v", called, tc.wantNext)
			}
		})
	}
}

// Every consumer of a configured directory — the walk, the watcher, the
// sender worker — has to be handed the same root, and filepath.WalkDir is
// the one that cannot resolve it for itself. A link resolving nowhere is
// the reason this is a function rather than a bare EvalSymlinks call:
// os.MkdirAll fails on one too, but names only the link it could not
// replace, never the target that is missing
func TestResolveDir(t *testing.T) {
	t.Run("a symlink resolves to its target", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "library")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		got, err := resolveDir("library directory", link)
		if err != nil {
			t.Fatalf("resolveDir: %v", err)
		}
		// the target itself may sit behind a link (macOS /var), so the
		// comparison is against its own resolution rather than the raw path
		want, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("resolveDir = %q, want %q", got, want)
		}
	})

	t.Run("an absent directory is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data", "covers")

		got, err := resolveDir("covers directory", dir)
		if err != nil {
			t.Fatalf("resolveDir: %v", err)
		}
		if info, err := os.Stat(got); err != nil || !info.IsDir() {
			t.Fatalf("Stat %q = %v, %v; want a directory", got, info, err)
		}
	})

	t.Run("a dangling symlink names the link and its target", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "volume1", "books")
		link := filepath.Join(t.TempDir(), "library")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		_, err := resolveDir("library directory", link)
		if err == nil {
			t.Fatal("resolveDir on a dangling link: want an error, got nil")
		}
		if !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), target) {
			t.Errorf("error = %q, want both the link %q and its target %q named", err, link, target)
		}
	})

	// The same failure one level up, and the ordinary NAS shape: it is the
	// mount point that is a link, not the directory configured under it.
	// Checking the final element alone leaves this case to MkdirAll, whose
	// message names neither the link nor what it points at
	t.Run("a dangling symlink at an ancestor names that link", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "volume1")
		nas := filepath.Join(t.TempDir(), "nas")
		if err := os.Symlink(target, nas); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		configured := filepath.Join(nas, "books")

		_, err := resolveDir("library directory", configured)
		if err == nil {
			t.Fatal("resolveDir under a dangling ancestor: want an error, got nil")
		}
		if !strings.Contains(err.Error(), nas) || !strings.Contains(err.Error(), target) {
			t.Errorf("error = %q, want the ancestor link %q and its target %q named", err, nas, target)
		}
	})

	// os.Readlink hands back the text of the link, so a relative one names
	// nothing the reader can go and look at
	t.Run("a relative target is named as a path", func(t *testing.T) {
		dir := t.TempDir()
		link := filepath.Join(dir, "library")
		if err := os.Symlink("../books", link); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		_, err := resolveDir("library directory", link)
		if err == nil {
			t.Fatal("resolveDir on a relative dangling link: want an error, got nil")
		}
		want := filepath.Join(filepath.Dir(dir), "books")
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want the target resolved to %q", err, want)
		}
	})
}

// The end-to-end shape the resolution exists for, and the one a unit test
// of resolveDir alone cannot catch: a resolved value that is computed and
// then not passed on leaves the scanner walking the link, which finds
// nothing behind it
func TestRunIndexesALibraryBehindASymlink(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "book.fb2"), []byte("book content"), 0o644); err != nil {
		t.Fatalf("write book: %v", err)
	}
	link := filepath.Join(t.TempDir(), "library")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink library: %v", err)
	}

	addr := freeAddr(t)
	dbPath := filepath.Join(t.TempDir(), "library.db")
	t.Setenv("ADDR", addr)
	t.Setenv("DB_PATH", dbPath)
	t.Setenv("LIBRARY_DIR", link)
	t.Setenv("COVERS_DIR", t.TempDir())
	t.Setenv("WATCH_ENABLED", "false")
	t.Setenv("METADATA_PROVIDERS", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	// /healthz answering is the signal that run has opened the database
	// and applied its migrations. The test must not open it before that:
	// storage.Open applies migrations too, and two handles doing so at
	// once fail each other — a race the test would own rather than
	// observe
	waitFor(t, done, "the server to answer /healthz", func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	waitFor(t, done, "the first sweep to index the book behind the link", func() bool {
		n, err := db.CountBooks(ctx)
		return err == nil && n == 1
	})
}

// freeAddr returns a loopback address nothing is listening on. run needs a
// fixed one — with :0 the chosen port is only ever named in a log line the
// test cannot read, since run installs its own logger
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

// waitFor polls until ready reports true, failing the test if run returns
// first (its error is the useful one) or if the wait runs out
func waitFor(t *testing.T, done <-chan error, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		select {
		case err := <-done:
			t.Fatalf("run returned while waiting for %s: %v", what, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 10s waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A LIBRARY_DIR that resolves nowhere is a mount that did not come up.
// Startup is where that has to be said: reaching the scanner, it is one
// unfollowable entry and an empty library, which the sweep can report but
// cannot refuse to run on
func TestRunRejectsADanglingLibraryDir(t *testing.T) {
	target := filepath.Join(t.TempDir(), "volume1", "books")
	link := filepath.Join(t.TempDir(), "library")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink library: %v", err)
	}

	t.Setenv("ADDR", "127.0.0.1:0")
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "library.db"))
	t.Setenv("LIBRARY_DIR", link)
	t.Setenv("COVERS_DIR", t.TempDir())
	t.Setenv("METADATA_PROVIDERS", "")

	err := run(context.Background())
	if err == nil {
		t.Fatal("run with a dangling LIBRARY_DIR: want an error, got nil")
	}
	if !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), target) {
		t.Errorf("error = %q, want both the configured link %q and its target %q named", err, link, target)
	}
}

// The Warn is what points at residue nothing else surfaces, and the Info is
// what stops it drowning the log every fifteen minutes for the life of a
// renamed folder. Getting the two the wrong way round leaves both the
// initial silence and the eventual noise, with every other test still green.
func TestUnconfirmedDirLogLevel(t *testing.T) {
	cases := []struct {
		name     string
		reported map[string]bool
		dir      string
		want     slog.Level
	}{
		{"first sweep of a restart", nil, "fiction", slog.LevelWarn},
		{"new since the last sweep", map[string]bool{"other": true}, "fiction", slog.LevelWarn},
		{"still unconfirmed", map[string]bool{"fiction": true}, "fiction", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unconfirmedLevel(tc.reported, tc.dir); got != tc.want {
				t.Errorf("unconfirmedLevel(%v, %q) = %v, want %v", tc.reported, tc.dir, got, tc.want)
			}
		})
	}
}

// The set handed to the next sweep is this sweep's, not a union: a
// directory that recovers and later empties again is news the second time.
func TestLogUnconfirmedDirsReturnsThisSweepsSet(t *testing.T) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	got := logUnconfirmedDirs(map[string]bool{"recovered": true}, map[string]int{"fiction": 2})
	if len(got) != 1 || !got["fiction"] {
		t.Errorf("logUnconfirmedDirs = %v, want only fiction", got)
	}
}
