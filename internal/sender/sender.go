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
	"io/fs"
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

// markTimeout bounds the write that records a send's outcome. It runs on a
// context detached from the worker's, so it needs a deadline of its own —
// short, because it is one local SQLite write and the process may be inside
// its shutdown budget when it happens.
const markTimeout = 5 * time.Second

// fileGoneReason is recorded when a send's book has no file location left
// to send — the book was pruned, or every copy is currently marked
// missing. Resolved at send time, not enqueue time, since a queue is a
// promise to act later and the library moves underneath it.
const fileGoneReason = "the file is no longer in the library"

// lookupFailedReason is recorded when the library index itself could not be
// read, which is not the same thing as the file being gone and must not
// claim to be: the book may be perfectly fine. Says "try again" because,
// unlike every other failure here, this one plausibly succeeds next time.
const lookupFailedReason = "could not read the library index — try again"

// fileUnreadableReason is recorded when the file is there as far as the
// index knows but the filesystem refused to hand it over: a permissions
// change, a failing disk, a stale NFS handle, an SMB mount that dropped.
// None of those is "the file is gone", and on a NAS they are the common
// failure; claiming fileGoneReason for them writes a false statement into
// send_log. Phrased like lookupFailedReason because, like it, this
// plausibly succeeds next time — and the OS error text goes to the log, not
// the status box, since EACCES is not a sentence a person acts on.
const fileUnreadableReason = "could not read the file — try again"

// timedOutReason is recorded when the per-send deadline expired before
// Resend answered. That outcome is unknown, not failed — the body may have
// been fully uploaded and accepted with only the response outstanding, the
// same ambiguity FailInterruptedSends hedges over — but unlike a shutdown,
// a timeout leaves the process running, and a row left sending would sit
// until the next restart with the UI polling it forever. failed is the one
// terminal state that offers Retry, so the row takes it, and the sentence
// carries the doubt the state cannot: a person who checks the device first
// avoids the duplicate a raw "context deadline exceeded" invited.
const timedOutReason = "timed out before Resend answered — check the Kindle before sending again"

// minUplinkBytesPerSecond is the slowest uplink a send deadline is sized
// for: 1 Mbit/s, a slow domestic line or a NAS behind one. The deadline
// for an attachment is its base64-encoded length over this rate plus half
// of resend.SendTimeout as slack, floored at SendTimeout itself — so a
// small file gets five minutes as before, while a 28MB one (~37MB
// encoded, ~313s at this rate) gets about seven and three-quarter minutes
// instead of being cut off at five with the outcome unknown. Retune on
// measurement, not instinct; the arithmetic is here so the measurement
// has something to be compared against.
const minUplinkBytesPerSecond = 125_000

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

	// sendTimeout is the floor of every per-send deadline — see
	// sendDeadline. resend.SendTimeout in production; a field rather than
	// the constant so a test can drive the timeout path in milliseconds.
	sendTimeout time.Duration
}

// New returns a Worker that reads jobs from db, sends them via t, and
// resolves book_files paths (which are stored relative to LIBRARY_DIR)
// against libraryDir.
func New(db *storage.DB, t Transport, libraryDir string) *Worker {
	return &Worker{
		db:          db,
		transport:   t,
		libraryDir:  libraryDir,
		notify:      make(chan struct{}, 1),
		sendTimeout: resend.SendTimeout,
	}
}

// sendDeadline is how long a send of size raw bytes is given before its
// outcome is declared unknown: the encoded upload at minUplinkBytesPerSecond
// plus half the floor as slack, never less than the floor. Sized on the
// base64 length rather than the file's, since that is what crosses the
// wire.
func (w *Worker) sendDeadline(size int64) time.Duration {
	encoded := (size + 2) / 3 * 4
	upload := time.Duration(encoded) * time.Second / minUplinkBytesPerSecond
	if d := upload + w.sendTimeout/2; d > w.sendTimeout {
		return d
	}
	return w.sendTimeout
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
			// A claim that lost its context is shutdown, not failure.
			if ctx.Err() == nil {
				slog.Error("claim next send", "error", err)
			}
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
		// Only errFileGone is a sentence written for a reader; anything
		// else is a storage failure, whose text belongs in the log rather
		// than in the status box under the Send button — and which says
		// nothing about whether the book is still there.
		reason := fileGoneReason
		if !errors.Is(err, errFileGone) {
			slog.Error("resolve send file", "send_id", send.ID, "error", err)
			reason = lookupFailedReason
		}
		w.fail(ctx, send.ID, reason)
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		w.failFileError(ctx, send.ID, path, err)
		return
	}
	if info.Size() > resend.MaxAttachmentSize {
		w.fail(ctx, send.ID, fmt.Sprintf("%.1f MB exceeds the %d MB limit",
			float64(info.Size())/(1<<20), resend.MaxAttachmentSize/(1<<20)))
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		w.failFileError(ctx, send.ID, path, err)
		return
	}

	deadline := w.sendDeadline(info.Size())
	sendCtx, cancel := context.WithTimeout(ctx, deadline)
	messageID, err := w.transport.Send(sendCtx, send.RecipientAddress, resend.Attachment{
		Filename: filename,
		Content:  content,
	})
	cancel()
	if err != nil {
		// A transport error that arrives with the worker's own context
		// already cancelled says nothing about whether Resend received
		// the message — the request was abandoned, not answered. Leave
		// the row sending for FailInterruptedSends to surface at the
		// next startup, rather than recording a failure that might be a
		// silent success.
		if ctx.Err() != nil {
			return
		}
		// The per-send deadline expiring is the same unknown without a
		// restart coming to resolve it, so it is recorded — see
		// timedOutReason for why as failed, and with that sentence rather
		// than the error's own text, which names a URL and a Go context
		// and nothing a person can act on. Every other transport error
		// is an answer.
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("send timed out", "send_id", send.ID, "deadline", deadline, "size", info.Size(), "error", err)
			w.fail(ctx, send.ID, timedOutReason)
			return
		}
		w.fail(ctx, send.ID, truncate(err.Error(), maxFailureReason))
		return
	}

	// Deliberately not ctx: Resend has accepted the message, and a
	// shutdown landing in this gap would otherwise leave the row sending
	// for FailInterruptedSends to rewrite as failed — reporting a book
	// that did arrive as one that didn't, and inviting a duplicate send.
	// The outcome of a completed request is always worth the write.
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	if err := w.db.MarkSendDelivered(markCtx, send.ID, messageID, time.Now()); err != nil {
		slog.Error("mark send delivered", "send_id", send.ID, "error", err)
	}
}

// failFileError records a filesystem failure against the send's file. Only
// fs.ErrNotExist means the file is gone; every other error — EACCES, EIO,
// ESTALE, a path component that is no longer a directory — is an unknown,
// and an unknown is not evidence, the same posture the scanner's
// missing-file reconciliation takes toward a non-ErrNotExist Lstat. Those
// are logged with the path, since the status box deliberately does not
// carry the OS error and this line is otherwise the only place to
// diagnose from.
func (w *Worker) failFileError(ctx context.Context, sendID int64, path string, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		w.fail(ctx, sendID, fileGoneReason)
		return
	}
	slog.Error("read send file", "send_id", sendID, "path", path, "error", err)
	w.fail(ctx, sendID, fileUnreadableReason)
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

// fail records sendID's terminal failure. Like the delivered path, it
// writes on a context detached from ctx: every caller has reached a
// verdict worth keeping — the file is gone, too large, unreadable, the
// transport answered with a rejection, or the deadline expired and the
// reason says so — and losing it to a shutdown would turn that into the
// ambiguous sending row recovery has to hedge over. The one case that is
// ambiguous *and* about to be resolved by a restart, a transport call
// abandoned because the worker's own context was cancelled, does not reach
// here at all.
func (w *Worker) fail(ctx context.Context, sendID int64, reason string) {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	if err := w.db.MarkSendFailed(markCtx, sendID, reason, time.Now()); err != nil {
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
