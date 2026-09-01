package storage

import (
	"io/fs"
	"path/filepath"
	"testing"
)

func TestOpen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "library.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	wantMigrations := len(entries)

	var gotMigrations int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&gotMigrations); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if gotMigrations != wantMigrations {
		t.Errorf("schema_migrations rows = %d, want %d", gotMigrations, wantMigrations)
	}

	var journalMode string
	if err := db.Read().QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want %q", journalMode, "wal")
	}

	for _, table := range []string{"books", "authors", "book_authors"} {
		var name string
		err := db.Read().QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "library.db")

	db1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	var firstCount int
	if err := db1.Read().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&firstCount); err != nil {
		t.Fatalf("count schema_migrations after first open: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close first DB: %v", err)
	}

	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()

	var secondCount int
	if err := db2.Read().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&secondCount); err != nil {
		t.Fatalf("count schema_migrations after second open: %v", err)
	}
	if secondCount != firstCount {
		t.Errorf("schema_migrations rows after reopen = %d, want %d (no new migrations applied)", secondCount, firstCount)
	}
}
