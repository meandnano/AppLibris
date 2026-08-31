package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"library/internal/scanner"
	"library/internal/service"
	"library/internal/storage"
	"library/internal/web"
)

func main() {
	dbPath := envOrDefault("DB_PATH", "./data/library.db")
	addr := envOrDefault("ADDR", ":8080")
	libraryDir := envOrDefault("LIBRARY_DIR", "./library")
	coversDir := envOrDefault("COVERS_DIR", "./data/covers")

	var level slog.Level
	if err := level.UnmarshalText([]byte(envOrDefault("LOG_LEVEL", "INFO"))); err != nil {
		slog.Error("parse LOG_LEVEL", "error", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	scanInterval, err := time.ParseDuration(envOrDefault("SCAN_INTERVAL", "15m"))
	if err != nil {
		slog.Error("parse SCAN_INTERVAL", "error", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		slog.Error("create library directory", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		slog.Error("create covers directory", "error", err)
		os.Exit(1)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	runScan(db, libraryDir, coversDir)
	go periodicScan(db, libraryDir, coversDir, scanInterval)

	svc := service.New(db)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/", web.Routes(svc, coversDir))

	slog.Info("listening", "addr", addr, "db_path", dbPath)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}

func periodicScan(db *storage.DB, libraryDir, coversDir string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		runScan(db, libraryDir, coversDir)
	}
}

func runScan(db *storage.DB, libraryDir, coversDir string) {
	slog.Debug("scan starting", "library_dir", libraryDir)

	result, err := scanner.Scan(context.Background(), db, libraryDir, coversDir)
	if err != nil {
		slog.Error("scan", "error", err)
		return
	}

	attrs := []any{"scanned", result.Scanned, "new", result.New, "moved", result.Moved,
		"unchanged", result.Unchanged, "orphaned", result.Orphaned, "errors", result.Errors}
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
