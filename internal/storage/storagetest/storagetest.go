// Package storagetest opens the database a test works against. Every
// package above storage uses it instead of storage.Open on a fresh path,
// because running all the migrations again costs twenty times what copying
// an already-migrated file does — see docs/notes/testing.md.
package storagetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"library/internal/storage"
)

var (
	templateOnce  sync.Once
	templateBytes []byte
	templateErr   error
)

// template returns the bytes of a database with every migration applied,
// building it on the first call. Holding the bytes rather than a path
// leaves nothing on disk for a TestMain to clean up, and building it from
// the migrations the binary was compiled with means it can never go stale
// the way a committed fixture would.
func template() ([]byte, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "applibris-storagetest")
		if err != nil {
			templateErr = err
			return
		}
		defer os.RemoveAll(dir)

		path := filepath.Join(dir, "library.db")
		db, err := storage.Open(path)
		if err != nil {
			templateErr = err
			return
		}
		// Close is what checkpoints the WAL into the main file, so the read
		// has to follow it: the -wal file goes with the directory, and bytes
		// read before the checkpoint would carry none of the schema.
		if err := db.Close(); err != nil {
			templateErr = err
			return
		}
		templateBytes, templateErr = os.ReadFile(path)
	})
	return templateBytes, templateErr
}

// Open returns a database migrated to the current schema, closed when the
// test ends. storage.Open still runs migrate over the copy, which finds
// every migration already applied.
//
// It is safe inside a synctest bubble: the template build opens and closes
// within this one call, so it leaves behind no connection opener, and the
// pool this hands back was made with the bubble's own t.
func Open(t testing.TB) *storage.DB {
	t.Helper()

	migrated, err := template()
	if err != nil {
		t.Fatalf("build template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "library.db")
	if err := os.WriteFile(path, migrated, 0o644); err != nil {
		t.Fatalf("write template database: %v", err)
	}

	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// SeedSends inserts one queued send_log row per timestamp in at, in a
// single transaction, with the columns and status EnqueueSend writes and a
// distinct address per row.
//
// Seeding the table alone is faithful because history reads send_log's
// denormalised book_title and recipient_address and never joins books or
// recipients. It exists for the two tests that need the history cap's worth
// of rows, where a call to EnqueueSend each costs a transaction each.
func SeedSends(t testing.TB, db *storage.DB, bookID int64, title string, at []time.Time) {
	t.Helper()

	err := db.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		for i, when := range at {
			address := fmt.Sprintf("reader%d@kindle.com", i)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO send_log (book_id, book_title, recipient_address, status, queued_at)
				VALUES (?, ?, ?, ?, ?)`,
				bookID, title, address, "queued", when.UTC().Format(sqliteTimeLayout)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed send_log: %v", err)
	}
}

// sqliteTimeLayout is storage's own timestamp format, which is unexported
// there. A row written in any other shape would sort and compare wrongly
// against the rows EnqueueSend writes, which is what this package's test
// pins.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"
