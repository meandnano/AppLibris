// Package sender is the send-to-Kindle queue worker: it claims queued rows
// from internal/storage's send_log, resolves each to a file on disk, and
// hands it to a Transport. Not internal/service, because nothing calls a
// worker the way a transport calls a service method; not internal/resend,
// which is deliberately "a thin wrapper over the single POST /emails
// endpoint, not a general mail abstraction."
package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"library/internal/resend"
	"library/internal/storage"
)

// pollInterval is the safety-net tick that catches anything a Notify poke
// missed — a row left queued by a crash between insert and notify, most
// obviously. Deliberately the same shape as the scanner's watcher-versus-
// periodic-rescan relationship: the poke is an optimisation, the tick is
// the mechanism. No env var, unlike the scanner's SCAN_INTERVAL: there is
// no deployment whose queue latency wants tuning.
const pollInterval = 1 * time.Minute

// maxFailureReason bounds how much of a transport error's text is
// persisted — Resend's API errors are already short sentences, but nothing
// stops a future transport from returning something enormous.
const maxFailureReason = 500

// fileGoneReason is recorded when a send's book has no file location left
// to send — the book was pruned, or every copy is currently marked
// missing. Resolved at send time, not enqueue time, since a queue is a
// promise to act later and the library moves underneath it.
const fileGoneReason = "the file is no longer in the library"

// Transport is what the worker needs from a mail provider. Declared here,
// on the consumer side, rather than in internal/resend — which notes it
// has no Sender interface "because nothing else implements one yet".
// *resend.Client satisfies this without changes.
type Transport interface {
	Send(ctx context.Context, to string, a resend.Attachment) (string, error)
}

// Worker claims and processes send_log jobs one at a time, in queue order.
// Concurrency buys nothing at a handful of sends a week, and costs the
// memory bound that keeps internal/resend's Send deliberately unstreamed:
// one send in flight means one attachment resident.
type Worker struct {
	db         *storage.DB
	transport  Transport
	libraryDir string
	notify     chan struct{}
}

// New returns a Worker that reads jobs from db, sends them via t, and
// resolves book_files paths (which are stored relative to LIBRARY_DIR)
// against libraryDir.
func New(db *storage.DB, t Transport, libraryDir string) *Worker {
	return &Worker{
		db:         db,
		transport:  t,
		libraryDir: libraryDir,
		notify:     make(chan struct{}, 1),
	}
}

// Notify pokes the worker to check the queue immediately, instead of
// waiting for the next pollInterval tick. Non-blocking: the channel has
// capacity 1 and a full channel means a poke is already pending, so a
// burst of enqueues coalesces into one wake-up rather than piling up or
// blocking the caller.
func (w *Worker) Notify() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// Run drains the queue, then blocks waiting for a Notify poke or the next
// pollInterval tick, until ctx is done. A job in flight when ctx is
// cancelled fails its context and is left in the sending state — the
// process is going away and the send may or may not have reached the
// transport, so it is not this call's place to guess. Recovering that row
// is the caller's job at next startup, via storage.FailInterruptedSends.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		w.drain(ctx)

		select {
		case <-w.notify:
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// drain processes claimed jobs until the queue is empty, ctx is done, or a
// claim fails outright.
func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		send, err := w.db.ClaimNextSend(ctx, time.Now())
		if err != nil {
			slog.Error("claim next send", "error", err)
			return
		}
		if send == nil {
			return
		}

		w.process(ctx, send)
	}
}

// process resolves and attempts one already-claimed send, marking it
// delivered or failed. A failure here — a missing file, an oversized one,
// a transport error — must never wedge the queue: it always ends in a
// terminal MarkSend* call so drain moves on to the next job.
func (w *Worker) process(ctx context.Context, send *storage.Send) {
	path, filename, err := w.resolveFile(ctx, send)
	if err != nil {
		w.fail(ctx, send.ID, err.Error())
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		w.fail(ctx, send.ID, fileGoneReason)
		return
	}
	if info.Size() > resend.MaxAttachmentSize {
		w.fail(ctx, send.ID, fmt.Sprintf("%.1f MB exceeds the %d MB limit",
			float64(info.Size())/(1<<20), resend.MaxAttachmentSize/(1<<20)))
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		w.fail(ctx, send.ID, fileGoneReason)
		return
	}

	sendCtx, cancel := context.WithTimeout(ctx, resend.SendTimeout)
	messageID, err := w.transport.Send(sendCtx, send.RecipientAddress, resend.Attachment{
		Filename: filename,
		Content:  content,
	})
	cancel()
	if err != nil {
		w.fail(ctx, send.ID, truncate(err.Error(), maxFailureReason))
		return
	}

	if err := w.db.MarkSendDelivered(ctx, send.ID, messageID, time.Now()); err != nil {
		slog.Error("mark send delivered", "send_id", send.ID, "error", err)
	}
}

// resolveFile picks send's book's first non-missing file location and
// returns its full on-disk path and display filename. Resolution happens
// here, at send time, rather than at enqueue time — see fileGoneReason.
func (w *Worker) resolveFile(ctx context.Context, send *storage.Send) (path, filename string, err error) {
	if !send.BookID.Valid {
		return "", "", errFileGone
	}

	files, err := w.db.ListBookFiles(ctx, send.BookID.Int64)
	if err != nil {
		return "", "", err
	}
	for _, f := range files {
		if !f.MissingSince.Valid {
			return filepath.Join(w.libraryDir, f.FilePath), filepath.Base(f.FilePath), nil
		}
	}
	return "", "", errFileGone
}

// errFileGone is resolveFile's sentinel; its Error() is exactly
// fileGoneReason so process can pass it straight into MarkSendFailed
// without distinguishing "how" it's gone.
var errFileGone = errors.New(fileGoneReason)

func (w *Worker) fail(ctx context.Context, sendID int64, reason string) {
	if err := w.db.MarkSendFailed(ctx, sendID, reason, time.Now()); err != nil {
		slog.Error("mark send failed", "send_id", sendID, "error", err)
	}
}

// truncate returns s cut to at most n bytes, respecting UTF-8 boundaries —
// a defensive bound on transport error text, not expected to fire against
// Resend's own short error sentences.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isUTF8Boundary(s[n]) {
		n--
	}
	return s[:n]
}

func isUTF8Boundary(b byte) bool { return b&0xC0 != 0x80 }
