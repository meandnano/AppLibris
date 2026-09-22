package storagetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"library/internal/storage"
)

// The template has to carry the schema itself, not merely open into one:
// Close is what checkpoints the WAL into the main file, and bytes read
// before that checkpoint hold an empty database that storage.Open would
// silently migrate from scratch on every call — the whole cost this
// package exists to avoid, and invisible through storage.Open.
//
// So this reads the template bytes with a bare driver handle, which runs no
// migrations, and compares what it finds with a database migrated the long
// way.
func TestTemplateCarriesTheMigratedSchema(t *testing.T) {
	migrated, err := template()
	if err != nil {
		t.Fatalf("build template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "template.db")
	if err := os.WriteFile(path, migrated, 0o644); err != nil {
		t.Fatalf("write template database: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open template database: %v", err)
	}
	t.Cleanup(func() { raw.Close() })

	fresh, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { fresh.Close() })

	freshVersions := queryStrings(t, fresh.Read(), `SELECT version FROM schema_migrations ORDER BY version`)
	if len(freshVersions) == 0 {
		t.Fatal("a freshly migrated database reports no applied migrations")
	}
	templateVersions := queryStrings(t, raw, `SELECT version FROM schema_migrations ORDER BY version`)
	if fmt.Sprint(templateVersions) != fmt.Sprint(freshVersions) {
		t.Errorf("template applied migrations = %v, want %v", templateVersions, freshVersions)
	}

	const schemaQuery = `SELECT type || ' ' || name || ': ' || IFNULL(sql, '') FROM sqlite_master ORDER BY type, name`
	freshSchema := queryStrings(t, fresh.Read(), schemaQuery)
	if len(freshSchema) == 0 {
		t.Fatal("a freshly migrated database reports no schema objects")
	}
	templateSchema := queryStrings(t, raw, schemaQuery)
	if fmt.Sprint(templateSchema) != fmt.Sprint(freshSchema) {
		t.Errorf("template schema = %v, want %v", templateSchema, freshSchema)
	}
}

func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()

	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan %s: %v", query, err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return values
}

// SeedSends writes send_log rows by hand, so what it writes has to read
// back the way EnqueueSend's own row does — the timestamp shape included,
// since history selects and orders on queued_at as text.
func TestSeedSendsReadsBackLikeAnEnqueuedRow(t *testing.T) {
	db := Open(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

	bookID, err := db.CreateBook(ctx, storage.Book{
		ContentHash: "hash-1", Title: "Book", SortTitle: "Book", Format: "epub",
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, _, err := db.EnqueueSend(ctx, bookID, "Book", "enqueued@kindle.com", at); err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	SeedSends(t, db, bookID, "Book", []time.Time{at})

	sends, err := db.ListSendsSince(ctx, at, 10)
	if err != nil {
		t.Fatalf("ListSendsSince: %v", err)
	}
	if len(sends) != 2 {
		t.Fatalf("ListSendsSince returned %d rows, want the seeded and the enqueued one", len(sends))
	}

	seeded, enqueued := sends[0], sends[1]
	if seeded.RecipientAddress == "enqueued@kindle.com" {
		seeded, enqueued = enqueued, seeded
	}
	if seeded.RecipientAddress != "reader0@kindle.com" {
		t.Fatalf("seeded row address = %q, want reader0@kindle.com", seeded.RecipientAddress)
	}

	if got, want := describeSend(seeded), describeSend(enqueued); got != want {
		t.Errorf("seeded row reads as %s, want %s", got, want)
	}
}

// describeSend renders everything about a row except the two columns that
// are meant to differ: its id and the address that distinguishes it.
func describeSend(s storage.Send) string {
	return fmt.Sprintf("book_id=%v title=%q status=%q message_id=%q reason=%q queued=%s started=%v finished=%v",
		s.BookID, s.BookTitle, s.Status, s.ProviderMessageID, s.FailureReason,
		s.QueuedAt.UTC(), nullTime(s.StartedAt), nullTime(s.FinishedAt))
}

func nullTime(t sql.NullTime) string {
	if !t.Valid {
		return "unset"
	}
	return t.Time.UTC().String()
}
