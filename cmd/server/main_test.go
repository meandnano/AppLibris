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
