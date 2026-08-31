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

	"library/internal/scanner"
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/", web.Routes(svc, coversDir))

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout guards a client that opens a connection and
		// never sends a request line. WriteTimeout is sized for
		// send-to-Kindle in mind: DESIGN.md makes sends a queued
		// background job precisely so a handler never holds a request
		// open reading a multi-megabyte book off disk, so 60s here is
		// headroom, not a design constraint — don't tune it down without
		// noticing that.
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

	// The health endpoint must answer the moment the process is up, so the
	// first sweep runs in the background alongside the periodic one rather
	// than blocking startup — a large library's first scan is minutes of
	// hashing, during which an orchestrator's readiness probe would
	// otherwise conclude the container is dead and restart it.
	go func() {
		runScan(ctx, db, libraryDir, coversDir, missingGrace)
		periodicScan(ctx, db, libraryDir, coversDir, scanInterval, missingGrace)
	}()

	select {
	case err := <-serveErr:
		db.Close()
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "error", err)
	}
	<-serveErr

	return db.Close()
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
