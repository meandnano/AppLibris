package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// EnrichmentStatus is one of enrichment_jobs' CHECK-constrained states.
type EnrichmentStatus string

const (
	EnrichmentQueued  EnrichmentStatus = "queued"
	EnrichmentRunning EnrichmentStatus = "running"
	EnrichmentDone    EnrichmentStatus = "done"
	EnrichmentFailed  EnrichmentStatus = "failed"
)

// EnrichmentJob mirrors the enrichment_jobs table.
type EnrichmentJob struct {
	ID            int64
	BookID        int64
	Status        EnrichmentStatus
	FailureReason string
	QueuedAt      time.Time
	StartedAt     sql.NullTime
	FinishedAt    sql.NullTime
}

const enrichmentJobColumns = `id, book_id, status, failure_reason, queued_at, started_at, finished_at`

func scanEnrichmentJob(row rowScanner) (*EnrichmentJob, error) {
	var j EnrichmentJob
	var status string
	err := row.Scan(&j.ID, &j.BookID, &status, &j.FailureReason, &j.QueuedAt, &j.StartedAt, &j.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Status = EnrichmentStatus(status)
	return &j, nil
}

// EnqueueEnrichment queues bookID for enrichment, unless it already has a
// job in the queued state — a book with two queued promises is not asked
// about twice as urgently, so the second enqueue is simply a no-op rather
// than a second row. A book whose only job is running, done or failed is
// not blocked: a running job is already someone else's problem, and a
// terminal one is history, not a standing promise. One statement, so the
// check and insert can't race against a second caller.
func (db *DB) EnqueueEnrichment(ctx context.Context, bookID int64, now time.Time) (queued bool, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO enrichment_jobs (book_id, status, queued_at)
			SELECT ?, ?, ?
			WHERE NOT EXISTS (SELECT 1 FROM enrichment_jobs WHERE book_id = ? AND status = ?)`,
			bookID, string(EnrichmentQueued), formatTime(now), bookID, string(EnrichmentQueued))
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		queued = affected > 0
		return err
	})
	return queued, err
}

// ClaimNextEnrichment atomically claims the oldest queued job, flipping it
// to running with started_at set, and returns it. Returns nil, nil when
// the queue is empty. Mirrors ClaimNextSend.
func (db *DB) ClaimNextEnrichment(ctx context.Context, now time.Time) (*EnrichmentJob, error) {
	var job *EnrichmentJob
	err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM enrichment_jobs WHERE status = ? ORDER BY queued_at LIMIT 1`, string(EnrichmentQueued)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE enrichment_jobs SET status = ?, started_at = ? WHERE id = ?`,
			string(EnrichmentRunning), formatTime(now), id); err != nil {
			return err
		}

		row := tx.QueryRowContext(ctx, `SELECT `+enrichmentJobColumns+` FROM enrichment_jobs WHERE id = ?`, id)
		job, err = scanEnrichmentJob(row)
		return err
	})
	return job, err
}

// MarkEnrichmentDone records id's successful completion — "successful"
// meaning the job ran to term, not that any field actually changed; most
// jobs will have nothing missing or nothing a provider answered. Scoped to
// WHERE status = 'running' so a terminal row can never be rewritten by a
// late call, the same guard MarkSend* uses.
func (db *DB) MarkEnrichmentDone(ctx context.Context, id int64, at time.Time) error {
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE enrichment_jobs SET status = ?, finished_at = ? WHERE id = ? AND status = ?`,
			string(EnrichmentDone), formatTime(at), id, string(EnrichmentRunning))
		return err
	})
}

// MarkEnrichmentFailed records id's failure with reason. Same terminal-state
// guard as MarkEnrichmentDone.
func (db *DB) MarkEnrichmentFailed(ctx context.Context, id int64, reason string, at time.Time) error {
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE enrichment_jobs SET status = ?, failure_reason = ?, finished_at = ? WHERE id = ? AND status = ?`,
			string(EnrichmentFailed), reason, formatTime(at), id, string(EnrichmentRunning))
		return err
	})
}

// RequeueInterruptedEnrichment puts every row still running back to queued
// — startup recovery for a process that died mid-job. This is the inverse
// of FailInterruptedSends, deliberately: a send's side effect (a message
// leaving the process) is not repeatable, so an interrupted one is failed
// rather than guessed at. An enrichment job's only effect is writing fields
// Resolve computed from data already in the database, Resolve is a pure
// function of that data, and running it again on the same inputs lands the
// same values — there is no outbound act to risk duplicating. So recovery
// here requeues instead of failing, and the worker may safely re-run a job
// that died mid-flight. queued_at is reset to now rather than left as it
// was: the row's place in ClaimNextEnrichment's oldest-first order should
// reflect that it is, once again, simply waiting, not that it has been
// waiting since before the crash.
func (db *DB) RequeueInterruptedEnrichment(ctx context.Context, now time.Time) (n int, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE enrichment_jobs SET status = ?, queued_at = ?, started_at = NULL WHERE status = ?`,
			string(EnrichmentQueued), formatTime(now), string(EnrichmentRunning))
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		n = int(affected)
		return err
	})
	return n, err
}

// FieldSourcesForBook returns bookID's field_sources rows keyed by field —
// the first read of field_sources in the project's history. A field with
// no row (never embedded, never edited) is simply absent from the map,
// which a lookup reads back as the empty string — the same as an
// explicitly-recorded source never being "manual", so it costs the
// resolver's missing-field test nothing to treat "no row" and "no source"
// alike.
func (db *DB) FieldSourcesForBook(ctx context.Context, bookID int64) (map[MetadataField]string, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT field, source FROM field_sources WHERE book_id = ?`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sources := map[MetadataField]string{}
	for rows.Next() {
		var field, source string
		if err := rows.Scan(&field, &source); err != nil {
			return nil, err
		}
		sources[MetadataField(field)] = source
	}
	return sources, rows.Err()
}

// authorsSeparator joins/splits FieldAuthors' value in the map
// ApplyEnrichedFields takes — the same newline convention the web layer's
// author textarea already uses, so a provider-supplied author list and a
// hand-typed one are the same shape by the time either reaches storage.
const authorsSeparator = "\n"

// splitAuthors reverses the join FieldAuthors' resolved value was built
// with, trimming and dropping blanks. Unlike internal/service's
// normalizeAuthors, it does no length validation — a provider's answer
// isn't form input a person needs an error message for, and an
// oversized name is simply a name updateBookAuthorsTx stores as given.
func splitAuthors(value string) []string {
	lines := strings.Split(value, authorsSeparator)
	names := make([]string, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// ApplyEnrichedFields writes every field in values together with its
// provenance (source — a provider's Name(), never "manual"; enrichment
// never claims a human made the edit) and the books_fts row, all in one
// transaction, so a book is never left half-enriched by a write that fails
// partway through. FieldAuthors is handled through the same join-table
// path UpdateBookAuthors uses, its value newline-separated per
// authorsSeparator; every other field is a plain column write via
// updateBookColumnTx. Returns false for an unknown book, the same
// absent-isn't-an-error contract UpdateBookField uses.
func (db *DB) ApplyEnrichedFields(ctx context.Context, bookID int64, values map[MetadataField]string, source string, modifiedAt time.Time) (exists bool, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM books WHERE id = ?)`, bookID).Scan(&exists); err != nil || !exists {
			return err
		}

		for field, value := range values {
			if field == FieldAuthors {
				if err := updateBookAuthorsTx(ctx, tx, bookID, splitAuthors(value), source, modifiedAt); err != nil {
					return err
				}
				continue
			}
			if err := updateBookColumnTx(ctx, tx, bookID, field, value, modifiedAt); err != nil {
				return err
			}
			if err := setFieldSourceTx(ctx, tx, bookID, field, source); err != nil {
				return err
			}
		}
		return syncBookFTSTx(ctx, tx, bookID)
	})
	return exists, err
}
