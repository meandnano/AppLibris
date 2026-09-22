package storagetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	// The bare sql.Open below needs the driver registered in its own right,
	// not by way of whatever else this file happens to import
	_ "modernc.org/sqlite"

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
// back the way EnqueueSend's own row does. The enqueued row is timestamped
// between the two seeded ones, so the order the three come back in is what
// pins the timestamp shape: queued_at is text, and only a fixed-width
// layout sorts chronologically.
func TestSeedSendsReadsBackLikeAnEnqueuedRow(t *testing.T) {
	db := Open(t)
	ctx := context.Background()
	first := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	between := first.Add(time.Second)
	last := first.Add(2 * time.Second)

	bookID, err := db.CreateBook(ctx, storage.Book{
		ContentHash: "hash-1", Title: "Book", SortTitle: "Book", Format: "epub",
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, _, err := db.EnqueueSend(ctx, bookID, "Book", "enqueued@kindle.com", between); err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	SeedSends(t, db, bookID, "Book", []time.Time{first, last})

	sends, err := db.ListSendsSince(ctx, first, 10)
	if err != nil {
		t.Fatalf("ListSendsSince: %v", err)
	}
	if len(sends) != 3 {
		t.Fatalf("ListSendsSince returned %d rows, want the two seeded and the enqueued one", len(sends))
	}

	gotOrder := []string{sends[0].RecipientAddress, sends[1].RecipientAddress, sends[2].RecipientAddress}
	wantOrder := []string{"reader1@kindle.com", "enqueued@kindle.com", "reader0@kindle.com"}
	if fmt.Sprint(gotOrder) != fmt.Sprint(wantOrder) {
		t.Errorf("ListSendsSince order = %v, want %v (newest first)", gotOrder, wantOrder)
	}
}

// The order above pins the layout only to the second a row lands in, and
// every other column besides. Two rows written for the same instant, one by
// each route, must read back identically and carry byte-identical text.
func TestSeedSendsWritesTheRowEnqueueSendWrites(t *testing.T) {
	db := Open(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 22, 10, 0, 0, 123456789, time.UTC)

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

	queuedAt := func(address string) string {
		t.Helper()
		var text string
		err := db.Read().QueryRowContext(ctx,
			`SELECT queued_at FROM send_log WHERE recipient_address = ?`, address).Scan(&text)
		if err != nil {
			t.Fatalf("read queued_at for %s: %v", address, err)
		}
		return text
	}
	if got, want := queuedAt("reader0@kindle.com"), queuedAt("enqueued@kindle.com"); got != want {
		t.Errorf("seeded queued_at = %q, want %q", got, want)
	}

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
