package storage

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
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

func TestOpenBoundsReadPool(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "library.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got := db.Read().Stats().MaxOpenConnections; got != readPoolSize {
		t.Errorf("read pool MaxOpenConnections = %d, want %d", got, readPoolSize)
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

// Before the nested-write guard, this deadlocked: the inner Write's BeginTx
// blocked forever waiting for the single connection the outer Write already
// holds. A short timeout means a regression fails in two seconds instead of
// hanging the whole suite until go test's ten-minute panic.
func TestNestedWriteReturnsErrNestedWrite(t *testing.T) {
	db := openTestDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error { return nil })
	})
	if !errors.Is(err, ErrNestedWrite) {
		t.Fatalf("nested Write error = %v, want ErrNestedWrite", err)
	}
}

// The nested-write guard is scoped to the ctx passed through a call chain,
// not to the DB as a whole, specifically so it can't mistake two
// independent, genuinely concurrent callers for nesting. This proves that:
// the second Write here must block on BeginTx behind the first (the
// pre-existing, unchanged property of a one-connection pool) and then
// succeed, rather than fail immediately with ErrNestedWrite.
func TestConcurrentWritesFromDifferentGoroutinesBothSucceed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	errs := make(chan error, 2)

	go func() {
		errs <- db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			close(firstStarted)
			<-releaseFirst
			_, err := createBookTx(ctx, tx, Book{ContentHash: "hash-first", Title: "First"}, nil)
			return err
		})
	}()
	<-firstStarted // the first Write now holds the pool's one connection

	secondDone := make(chan struct{})
	go func() {
		errs <- db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			_, err := createBookTx(ctx, tx, Book{ContentHash: "hash-second", Title: "Second"}, nil)
			return err
		})
		close(secondDone)
	}()

	select {
	case <-secondDone:
		t.Fatal("second Write returned before the first released the connection; want it blocked, not failed with ErrNestedWrite")
	case <-time.After(50 * time.Millisecond):
		// Still blocked on BeginTx, as wanted.
	}

	close(releaseFirst)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Write %d: %v", i, err)
		}
	}

	for _, hash := range []string{"hash-first", "hash-second"} {
		b, err := db.FindBookByContentHash(ctx, hash)
		if err != nil || b == nil {
			t.Errorf("FindBookByContentHash(%q) = %v, %v; want a book", hash, b, err)
		}
	}
}
