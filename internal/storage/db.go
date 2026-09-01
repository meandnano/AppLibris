// Package storage is the SQLite-backed persistence layer. It owns the
// database file, schema migrations, and the read/write connection split
// described in DESIGN.md.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps a single SQLite file with two connection pools sharing it: a
// multi-connection read pool, and a single-connection write pool. WAL mode
// lets reads proceed concurrently with an in-flight write. Restricting the
// write pool to one connection serializes writes without a hand-rolled
// goroutine/channel.
type DB struct {
	read  *sql.DB
	write *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path, enables
// WAL mode and foreign keys, and applies any pending migrations.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read pool: %w", err)
	}

	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		read.Close()
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	write.SetMaxOpenConns(1)

	if err := migrate(write); err != nil {
		read.Close()
		write.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &DB{read: read, write: write}, nil
}

// Close closes both underlying connection pools.
func (db *DB) Close() error {
	writeErr := db.write.Close()
	readErr := db.read.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// Read returns the pool to use for read-only queries.
func (db *DB) Read() *sql.DB {
	return db.read
}

// Write runs fn inside a transaction on the single-connection write pool,
// serializing it against every other write.
func (db *DB) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
