package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"library/internal/scanner"
	"library/internal/storage"
)

func main() {
	dbPath := envOrDefault("DB_PATH", "./data/library.db")
	addr := envOrDefault("ADDR", ":8080")
	libraryDir := envOrDefault("LIBRARY_DIR", "./library")

	scanInterval, err := time.ParseDuration(envOrDefault("SCAN_INTERVAL", "15m"))
	if err != nil {
		log.Fatalf("parse SCAN_INTERVAL: %v", err)
	}

	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		log.Fatalf("create library directory: %v", err)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	runScan(db, libraryDir)
	go periodicScan(db, libraryDir, scanInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("listening on %s (db: %s)", addr, dbPath)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func periodicScan(db *storage.DB, libraryDir string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		runScan(db, libraryDir)
	}
}

func runScan(db *storage.DB, libraryDir string) {
	result, err := scanner.Scan(context.Background(), db, libraryDir)
	if err != nil {
		log.Printf("scan: %v", err)
		return
	}
	log.Printf("scan complete: %d scanned, %d new, %d moved, %d unchanged, %d errors",
		result.Scanned, result.New, result.Moved, result.Unchanged, result.Errors)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
