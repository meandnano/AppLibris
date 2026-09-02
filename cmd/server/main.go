package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"library/internal/resend"
	"library/internal/scanner"
	"library/internal/sender"
	"library/internal/service"
	"library/internal/storage"
	"library/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("run", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dbPath := envOrDefault("DB_PATH", "./data/library.db")
	addr := envOrDefault("ADDR", ":8080")
	libraryDir := envOrDefault("LIBRARY_DIR", "./library")
	coversDir := envOrDefault("COVERS_DIR", "./data/covers")

	var level slog.Level
	if err := level.UnmarshalText([]byte(envOrDefault("LOG_LEVEL", "INFO"))); err != nil {
		return fmt.Errorf("parse LOG_LEVEL: %w", err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	scanInterval, err := time.ParseDuration(envOrDefault("SCAN_INTERVAL", "15m"))
	if err != nil {
		return fmt.Errorf("parse SCAN_INTERVAL: %w", err)
	}
	missingGrace, err := time.ParseDuration(envOrDefault("MISSING_GRACE", "24h"))
	if err != nil {
		return fmt.Errorf("parse MISSING_GRACE: %w", err)
	}
	if missingGrace < 0 {
		// A negative grace pulls the pruning cutoff into the future, so a
		// file marked missing this very sweep would immediately qualify for
		// deletion — silently bypassing the two-phase safeguard entirely.
		return fmt.Errorf("parse MISSING_GRACE: must not be negative: %s", missingGrace)
	}

	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		return fmt.Errorf("create library directory: %w", err)
	}
	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		return fmt.Errorf("create covers directory: %w", err)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	svc := service.New(db)

	// Both RESEND_API_KEY and RESEND_FROM must be set to send anything —
	// browsing must still work on a dev machine with neither, so a missing
	// one only disables sending rather than failing startup. sendEnabled
	// is threaded into web.Routes either way: the send routes stay
	// registered so a stale open tab gets an explanation instead of a 404.
	resendAPIKey := os.Getenv("RESEND_API_KEY")
	resendFrom := os.Getenv("RESEND_FROM")
	sendEnabled := resendAPIKey != "" && resendFrom != ""

	var worker *sender.Worker
	if sendEnabled {
		worker = sender.New(db, resend.NewClient(resendAPIKey, resendFrom), libraryDir)
		svc.Notify = worker.Notify
	} else if resendAPIKey == "" {
		slog.Warn("sending disabled: RESEND_API_KEY is not set")
	} else {
		slog.Warn("sending disabled: RESEND_FROM is not set")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/", web.Routes(svc, coversDir, sendEnabled))

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout guards a client that opens a connection and
		// never sends a request line. WriteTimeout is sized with
		// send-to-Kindle in mind: DESIGN.md makes sends a queued
		// background job precisely so a handler never holds a request
		// open reading a multi-megabyte book off disk — the worker does
		// that instead — so 60s here is headroom, not a design constraint.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr, "db_path", dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// The scan goroutine gets its own cancellable context, derived from ctx
	// but stoppable independently of it: a serving failure (the address is
	// already in use, say) needs to cancel and wait for the scan just as
	// reliably as a signal does, rather than closing the database out from
	// under whatever the scan is doing. The health endpoint must answer the
	// moment the process is up, so the first sweep runs in the background
	// alongside the periodic one rather than blocking startup — a large
	// library's first scan is minutes of hashing, during which an
	// orchestrator's readiness probe would otherwise conclude the container
	// is dead and restart it.
	// The worker runs on the same independently-cancellable scanCtx as the
	// scan loop — send-to-Kindle jobs and library sweeps both want the
	// same shutdown ordering (unwind before the database closes under
	// them), so one child context and one wait pattern serves both.
	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		runScan(scanCtx, db, libraryDir, coversDir, missingGrace)
		periodicScan(scanCtx, db, libraryDir, coversDir, scanInterval, missingGrace)
	}()

	var workerDone chan struct{}
	if sendEnabled {
		// A row still "sending" means the process died between handing
		// the bytes to Resend and recording the answer — which side of
		// that request it died on is unknowable, so recovery fails the
		// row rather than requeueing it (see internal/sender's doc
		// comment). Runs once, before the worker starts claiming jobs.
		if _, err := db.FailInterruptedSends(ctx, "interrupted by a restart — send again if it didn't arrive", time.Now()); err != nil {
			slog.Error("fail interrupted sends", "error", err)
		}

		workerDone = make(chan struct{})
		go func() {
			defer close(workerDone)
			worker.Run(scanCtx)
		}()
	}

	select {
	case err := <-serveErr:
		// A serving failure isn't a signal, so ctx (and scanCtx, derived
		// from it) is still live — cancel scanCtx explicitly and wait out
		// the same bounded budget the signal path uses below, so neither
		// background goroutine can still be using db when it's closed a
		// few lines down.
		deadline, cancelDeadline := context.WithTimeout(context.Background(), 10*time.Second)
		waitForBackground(cancelScan, scanDone, deadline.Done(), "scan")
		if workerDone != nil {
			waitForBackground(cancelScan, workerDone, deadline.Done(), "sender")
		}
		cancelDeadline()
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("close database", "error", closeErr)
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "error", err)
	}
	<-serveErr

	// Give the background goroutines the rest of the shutdown budget to
	// notice cancellation and unwind cleanly (an in-flight write returns on
	// cancellation, rolling back rather than committing a partial one)
	// before the database closes out from under them. cancelScan is
	// already implied by ctx.Done() here (scanCtx is derived from ctx),
	// but calling it explicitly costs nothing and keeps this path
	// symmetric with the serveErr one above. A goroutine stuck outside any
	// context-aware call — mid-hash on a large file, mid-upload on a
	// send — can still miss this window; the bound exists so that doesn't
	// hang shutdown.
	waitForBackground(cancelScan, scanDone, shutdownCtx.Done(), "scan")
	if workerDone != nil {
		waitForBackground(cancelScan, workerDone, shutdownCtx.Done(), "sender")
	}

	return db.Close()
}

// waitForBackground cancels the background goroutine driven by cancel and
// waits for done to close, up to deadline, warning (naming name as the
// task attribute) if it doesn't. Shared by the scan loop and the send-to-Kindle
// worker, which run on the same cancellable scanCtx: the caller must not
// close the database until every call sharing that ctx has returned, or a
// goroutine that missed its deadline could still write onto a closed
// connection. Calling cancel more than once (once per shared-ctx goroutine
// waited on) is safe — context.CancelFunc is idempotent.
func waitForBackground(cancel context.CancelFunc, done <-chan struct{}, deadline <-chan struct{}, name string) {
	cancel()
	select {
	case <-done:
	case <-deadline:
		slog.Warn("background task did not exit before shutdown deadline", "task", name)
	}
}

func periodicScan(ctx context.Context, db *storage.DB, libraryDir, coversDir string, interval, missingGrace time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			runScan(ctx, db, libraryDir, coversDir, missingGrace)
		case <-ctx.Done():
			return
		}
	}
}

func runScan(ctx context.Context, db *storage.DB, libraryDir, coversDir string, missingGrace time.Duration) {
	slog.Debug("scan starting", "library_dir", libraryDir)

	result, err := scanner.Scan(ctx, db, libraryDir, coversDir, missingGrace)
	if err != nil {
		slog.Error("scan", "error", err)
		return
	}

	attrs := []any{"scanned", result.Scanned, "new", result.New, "moved", result.Moved,
		"unchanged", result.Unchanged, "orphaned", result.Orphaned, "missing", result.Missing,
		"pruned", result.Pruned, "covers_regenerated", result.CoversRegenerated, "errors", result.Errors}
	if result.Errors > 0 {
		slog.Warn("scan complete", attrs...)
	} else {
		slog.Info("scan complete", attrs...)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
