// Package storage is the SQLite-backed persistence layer. It owns the
// database file, schema migrations, and the read/write connection split
// described in DESIGN.md.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// ErrNestedWrite is returned by Write when the ctx passed to it already
// carries another Write's in-progress marker — see Write's doc comment.
var ErrNestedWrite = errors.New("storage: nested DB.Write")

// readPoolSize bounds the read connection pool — see the comment on
// SetMaxOpenConns in Open for why. A constant rather than runtime.NumCPU:
// SQLite reads are I/O-bound against the page cache, not CPU-bound, so
// sizing this to the host's core count would vary the pool between a
// laptop and the target box for no reason connected to what it does. Not
// 1 or 2: the scanner's reconcileMissing reads through this same pool, and
// too small a ceiling would let a sweep's read block page renders even
// though WAL mode means readers never block each other at the SQLite
// level — only the pool itself would serialize them.
const readPoolSize = 8

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
	// The write pool is pinned to one connection to serialise writes. The
	// read pool is bounded for a different reason: database/sql defaults to
	// an unlimited ceiling with only 2 idle connections, so any concurrency
	// above two opens connections, uses them once and discards them —
	// measured at 49 discarded across 200 page renders at concurrency 4,
	// and each discard is a fresh SQLite open plus pragma application.
	// Matching max and idle keeps connections in the pool instead. This is
	// about predictability and a bounded page-cache footprint, not speed:
	// the wall-clock difference is inside the noise at this scale.
	read.SetMaxOpenConns(readPoolSize)
	read.SetMaxIdleConns(readPoolSize)

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

// writeInProgressKey marks, on the ctx Write hands to its callback, that a
// write transaction is already open on this call chain — see Write's doc
// comment.
type writeInProgressKey struct{}

// Write runs fn inside a transaction on the single-connection write pool,
// serializing it against every other write.
//
// fn is handed a ctx derived from the one passed to Write; use *that* ctx,
// not Write's own parameter, for anything fn calls — package-internal "…Tx"
// helpers (e.g. createBookTx) included. It carries a marker recording that
// a write is already in flight on this call chain, so that calling an
// exported *DB method with it — the mistake this guards against, since the
// pool has exactly one connection and that nested call's own BeginTx would
// otherwise block on the connection this call already holds, hanging until
// ctx expires rather than returning an error — is instead caught
// immediately and returns ErrNestedWrite.
//
// A genuinely concurrent call made with an unrelated ctx carries no such
// marker, so it is unaffected: it blocks on BeginTx like any second caller
// of a one-connection pool, and succeeds once this transaction commits.
// The marker is scoped to the ctx passed through a call chain, not to the
// DB as a whole, specifically so it cannot mistake that ordinary
// concurrency for nesting.
func (db *DB) Write(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	if ctx.Value(writeInProgressKey{}) != nil {
		return ErrNestedWrite
	}
	ctx = context.WithValue(ctx, writeInProgressKey{}, true)

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
