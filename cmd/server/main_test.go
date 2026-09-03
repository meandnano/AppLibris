package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
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
// producing nonsense.
func TestRunRejectsBadWatchConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"negative settle", "WATCH_SETTLE", "-5s"},
		{"unparseable settle", "WATCH_SETTLE", "soon"},
		{"unparseable enabled", "WATCH_ENABLED", "sometimes"},
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
