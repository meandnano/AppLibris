package storage

import (
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

// openMigratedTo opens a fresh SQLite database and applies every embedded
// migration up to and including upTo (a filename), leaving the rest
// unapplied — reproducing the state a real deployment was in the moment
// before a later migration file was added, which is what a "does old data
// survive this migration" test needs to start from.
func openMigratedTo(t *testing.T, upTo string) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "library.db") + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(createMigrationsTable); err != nil {
		t.Fatalf("create schema_migrations table: %v", err)
	}

	for _, name := range migrationFileNames(t) {
		if err := applyMigration(db, name); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
		if name == upTo {
			break
		}
	}
	return db
}

func migrationFileNames(t *testing.T) []string {
	t.Helper()
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestFieldSourcesRebuildPreservesExistingRows drives the four-file
// field_sources rebuild (2026090304-2026090307) directly: starting from the
// schema as it stood right after the original table was created
// (2026090206), with a row already in it, then applying the rest of the
// embedded migrations and checking the row is still there afterward. Open's
// own tests already prove every migration applies to a brand new database;
// this is the one a rebuild needs and a fresh-database test can't
// exercise, since a fresh database never has pre-rebuild data to lose.
func TestFieldSourcesRebuildPreservesExistingRows(t *testing.T) {
	db := openMigratedTo(t, "2026090206_create_field_sources_table.sql")

	res, err := db.Exec(`INSERT INTO books (content_hash, title, sort_title, format) VALUES (?, ?, ?, ?)`,
		"pre-rebuild", "Pre-rebuild Book", "pre-rebuild book", "epub")
	if err != nil {
		t.Fatalf("insert book: %v", err)
	}
	bookID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO field_sources (book_id, field, source) VALUES (?, ?, ?)`,
		bookID, "title", "embedded"); err != nil {
		t.Fatalf("insert field_sources row before rebuild: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate through the rebuild: %v", err)
	}

	var source string
	if err := db.QueryRow(`SELECT source FROM field_sources WHERE book_id = ? AND field = ?`, bookID, "title").Scan(&source); err != nil {
		t.Fatalf("read surviving row: %v", err)
	}
	if source != "embedded" {
		t.Errorf("source after rebuild = %q, want embedded", source)
	}

	// The rebuild's whole point: cover is now an accepted value...
	if _, err := db.Exec(`INSERT INTO field_sources (book_id, field, source) VALUES (?, ?, ?)`,
		bookID, "cover", "openlibrary"); err != nil {
		t.Errorf("insert a cover field_sources row after rebuild: %v", err)
	}
	// ...and the CHECK constraint still rejects everything else.
	if _, err := db.Exec(`INSERT INTO field_sources (book_id, field, source) VALUES (?, ?, ?)`,
		bookID, "bogus", "openlibrary"); err == nil {
		t.Error("insert an unrecognised field after rebuild = nil error, want a CHECK-constraint failure")
	}
}
