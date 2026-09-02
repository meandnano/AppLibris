package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Recipient mirrors the recipients table.
type Recipient struct {
	ID         int64
	Address    string
	Label      string
	LastUsedAt sql.NullTime
	AddedAt    time.Time
}

// SendStatus is one of send_log's CHECK-constrained states.
type SendStatus string

const (
	SendQueued    SendStatus = "queued"
	SendSending   SendStatus = "sending"
	SendDelivered SendStatus = "delivered"
	SendFailed    SendStatus = "failed"
)

// Send mirrors the send_log table. BookID is nullable because a book the
// scanner later deletes leaves its send history behind — see the
// send_log migration for why book_title is kept alongside it rather than
// joined from books.
type Send struct {
	ID                int64
	BookID            sql.NullInt64
	BookTitle         string
	RecipientAddress  string
	Status            SendStatus
	ProviderMessageID string
	FailureReason     string
	QueuedAt          time.Time
	StartedAt         sql.NullTime
	FinishedAt        sql.NullTime
}

const sendColumns = `id, book_id, book_title, recipient_address, status, provider_message_id, failure_reason, queued_at, started_at, finished_at`

func scanSend(row *sql.Row) (*Send, error) {
	var s Send
	var status string
	err := row.Scan(&s.ID, &s.BookID, &s.BookTitle, &s.RecipientAddress, &status, &s.ProviderMessageID,
		&s.FailureReason, &s.QueuedAt, &s.StartedAt, &s.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Status = SendStatus(status)
	return &s, nil
}

// ListRecipients returns every saved recipient, most-recently-used first —
// SQLite sorts NULL as smaller than any value, so ordering last_used_at
// DESC already puts never-used addresses last with no NULLS LAST clause or
// leading IS NULL term needed. The picker's default is then simply "the
// first option", with no ordering logic in the transport.
func (db *DB) ListRecipients(ctx context.Context) ([]Recipient, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT id, address, label, last_used_at, added_at
		FROM recipients
		ORDER BY last_used_at DESC, address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recipients []Recipient
	for rows.Next() {
		var r Recipient
		if err := rows.Scan(&r.ID, &r.Address, &r.Label, &r.LastUsedAt, &r.AddedAt); err != nil {
			return nil, err
		}
		recipients = append(recipients, r)
	}
	return recipients, rows.Err()
}

// createRecipientTx inserts address (find-or-creating it: an address
// already saved is returned as-is rather than failing the unique index) —
// adding an address you already have is a user slip, not an error worth a
// page. It must be called from inside a DB.Write callback — see DB.Write's
// contract.
func createRecipientTx(ctx context.Context, tx *sql.Tx, address, label string, now time.Time) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO recipients (address, label, added_at)
		VALUES (?, ?, ?)
		ON CONFLICT(address) DO NOTHING`,
		address, label, formatTime(now)); err != nil {
		return 0, err
	}

	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM recipients WHERE address = ?`, address).Scan(&id)
	return id, err
}

// CreateRecipient is createRecipientTx run as its own write transaction.
func (db *DB) CreateRecipient(ctx context.Context, address, label string, now time.Time) (id int64, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err = createRecipientTx(ctx, tx, address, label, now)
		return err
	})
	return id, err
}

// EnqueueSend records a new queued send and bumps the recipient's
// last_used_at, in one transaction. The bump happens at enqueue, not on
// delivery: "most recently used" means "the one I last chose", and a send
// that later fails should not send the picker back to a different address.
func (db *DB) EnqueueSend(ctx context.Context, bookID int64, title, address string, now time.Time) (id int64, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO send_log (book_id, book_title, recipient_address, status, queued_at)
			VALUES (?, ?, ?, ?, ?)`,
			bookID, title, address, string(SendQueued), formatTime(now))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `UPDATE recipients SET last_used_at = ? WHERE address = ?`, formatTime(now), address)
		return err
	})
	return id, err
}

// ClaimNextSend atomically claims the oldest queued send, flipping it to
// sending with started_at set, and returns it. Returns nil, nil when the
// queue is empty. Atomic even though today's single worker makes
// contention impossible: the claim is the one place a second worker would
// corrupt, and doing it correctly costs one extra statement inside a
// transaction already open.
func (db *DB) ClaimNextSend(ctx context.Context, now time.Time) (*Send, error) {
	var send *Send
	err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM send_log WHERE status = ? ORDER BY queued_at LIMIT 1`, string(SendQueued)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE send_log SET status = ?, started_at = ? WHERE id = ?`,
			string(SendSending), formatTime(now), id); err != nil {
			return err
		}

		row := tx.QueryRowContext(ctx, `SELECT `+sendColumns+` FROM send_log WHERE id = ?`, id)
		send, err = scanSend(row)
		return err
	})
	return send, err
}

// MarkSendDelivered records id's successful delivery. Scoped to WHERE
// status = 'sending' so a terminal row can never be rewritten by a late
// worker — the update is a no-op by design if id is no longer sending.
func (db *DB) MarkSendDelivered(ctx context.Context, id int64, messageID string, at time.Time) error {
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE send_log SET status = ?, provider_message_id = ?, finished_at = ?
			WHERE id = ? AND status = ?`,
			string(SendDelivered), messageID, formatTime(at), id, string(SendSending))
		return err
	})
}

// MarkSendFailed records id's failure with reason. Same terminal-state
// guard as MarkSendDelivered.
func (db *DB) MarkSendFailed(ctx context.Context, id int64, reason string, at time.Time) error {
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE send_log SET status = ?, failure_reason = ?, finished_at = ?
			WHERE id = ? AND status = ?`,
			string(SendFailed), reason, formatTime(at), id, string(SendSending))
		return err
	})
}

// FailInterruptedSends fails every row still in sending — startup recovery
// for a process that died between handing a send to the transport and
// recording its answer. Which side of that request it died on is
// unknowable, so this never requeues: requeueing risks a silent duplicate
// delivery, while failing surfaces the ambiguity to the one person who can
// resolve it and leaves retry a click away.
func (db *DB) FailInterruptedSends(ctx context.Context, reason string, at time.Time) (n int, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE send_log SET status = ?, failure_reason = ?, finished_at = ?
			WHERE status = ?`,
			string(SendFailed), reason, formatTime(at), string(SendSending))
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		n = int(affected)
		return err
	})
	return n, err
}

// GetSend returns one send by id, or nil if it doesn't exist — the status
// poll's lookup.
func (db *DB) GetSend(ctx context.Context, id int64) (*Send, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+sendColumns+` FROM send_log WHERE id = ?`, id)
	return scanSend(row)
}

// LatestSendForBook returns the most recently queued send for bookID, or
// nil if it has never been sent — the detail page's initial render, so a
// page loaded mid-send or after a completed one shows the right state
// instead of a bare button.
func (db *DB) LatestSendForBook(ctx context.Context, bookID int64) (*Send, error) {
	row := db.read.QueryRowContext(ctx, `
		SELECT `+sendColumns+` FROM send_log WHERE book_id = ? ORDER BY queued_at DESC, id DESC LIMIT 1`, bookID)
	return scanSend(row)
}
