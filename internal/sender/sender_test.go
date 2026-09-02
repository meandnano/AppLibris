package sender

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"library/internal/resend"
	"library/internal/storage"
)

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stubTransport is a Transport with no HTTP: sendFunc decides the outcome
// per call, and every call is recorded for assertions.
type stubTransport struct {
	mu       sync.Mutex
	sendFunc func(ctx context.Context, to string, a resend.Attachment) (string, error)
	calls    []stubCall
}

type stubCall struct {
	To string
	A  resend.Attachment
}

func (s *stubTransport) Send(ctx context.Context, to string, a resend.Attachment) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{To: to, A: a})
	s.mu.Unlock()
	return s.sendFunc(ctx, to, a)
}

func (s *stubTransport) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// setupBookWithFile creates a book with one file location, writing content
// to libraryDir/path so the worker's os.Stat/os.ReadFile succeed against a
// real file.
func setupBookWithFile(t *testing.T, db *storage.DB, libraryDir, path string, content []byte) int64 {
	t.Helper()
	if err := os.WriteFile(filepath.Join(libraryDir, path), content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	bookID, _, _, err := db.CreateBookWithFile(context.Background(), storage.Book{
		ContentHash: "hash-" + path, Title: "Book: " + path, Format: "epub",
	}, nil, path, int64(len(content)), time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	return bookID
}

func TestWorkerHappyPathDelivers(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	content := []byte("epub bytes")
	bookID := setupBookWithFile(t, db, libraryDir, "book.epub", content)
	sendID, err := db.EnqueueSend(ctx, bookID, "Book", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	stub := &stubTransport{sendFunc: func(ctx context.Context, to string, a resend.Attachment) (string, error) {
		return "msg_123", nil
	}}
	w := New(db, stub, libraryDir)
	w.drain(ctx)

	if stub.hits() != 1 {
		t.Fatalf("transport hits = %d, want 1", stub.hits())
	}
	call := stub.calls[0]
	if call.To != "reader@kindle.com" {
		t.Errorf("To = %q, want reader@kindle.com", call.To)
	}
	if call.A.Filename != "book.epub" {
		t.Errorf("Filename = %q, want book.epub", call.A.Filename)
	}
	if string(call.A.Content) != string(content) {
		t.Errorf("Content = %q, want %q", call.A.Content, content)
	}

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != storage.SendDelivered {
		t.Errorf("Status = %q, want delivered", got.Status)
	}
	if got.ProviderMessageID != "msg_123" {
		t.Errorf("ProviderMessageID = %q, want msg_123", got.ProviderMessageID)
	}
}

// A transport error on one job must not stop the worker from claiming the
// next one — otherwise a single bad send wedges the whole queue behind it.
func TestWorkerTransportErrorFailsAndContinuesQueue(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	badBook := setupBookWithFile(t, db, libraryDir, "bad.epub", []byte("bad"))
	goodBook := setupBookWithFile(t, db, libraryDir, "good.epub", []byte("good"))

	now := time.Now()
	badID, err := db.EnqueueSend(ctx, badBook, "Bad", "reader@kindle.com", now)
	if err != nil {
		t.Fatalf("EnqueueSend bad: %v", err)
	}
	goodID, err := db.EnqueueSend(ctx, goodBook, "Good", "reader@kindle.com", now.Add(time.Second))
	if err != nil {
		t.Fatalf("EnqueueSend good: %v", err)
	}

	call := 0
	stub := &stubTransport{sendFunc: func(ctx context.Context, to string, a resend.Attachment) (string, error) {
		call++
		if call == 1 {
			return "", errors.New("552 attachment rejected")
		}
		return "msg_456", nil
	}}
	w := New(db, stub, libraryDir)
	w.drain(ctx)

	if stub.hits() != 2 {
		t.Fatalf("transport hits = %d, want 2 (the bad job must not block the good one)", stub.hits())
	}

	bad, err := db.GetSend(ctx, badID)
	if err != nil || bad == nil {
		t.Fatalf("GetSend bad: %+v, %v", bad, err)
	}
	if bad.Status != storage.SendFailed {
		t.Errorf("bad Status = %q, want failed", bad.Status)
	}
	if bad.FailureReason != "552 attachment rejected" {
		t.Errorf("bad FailureReason = %q, want the transport's error text", bad.FailureReason)
	}

	good, err := db.GetSend(ctx, goodID)
	if err != nil || good == nil {
		t.Fatalf("GetSend good: %+v, %v", good, err)
	}
	if good.Status != storage.SendDelivered {
		t.Errorf("good Status = %q, want delivered", good.Status)
	}
}

func TestWorkerOversizedFileFailsWithoutCallingTransport(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	const size = resend.MaxAttachmentSize + 1024*1024 // 29MB
	path := "huge.epub"
	f, err := os.Create(filepath.Join(libraryDir, path))
	if err != nil {
		t.Fatalf("create huge file: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate huge file: %v", err)
	}
	f.Close()

	bookID, _, _, err := db.CreateBookWithFile(ctx, storage.Book{ContentHash: "hash-huge", Title: "Huge", Format: "epub"}, nil, path, size, time.Now())
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	sendID, err := db.EnqueueSend(ctx, bookID, "Huge", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	stub := &stubTransport{sendFunc: func(ctx context.Context, to string, a resend.Attachment) (string, error) {
		t.Error("transport called for an oversized file; the pre-read stat check should have short-circuited it")
		return "", nil
	}}
	w := New(db, stub, libraryDir)
	w.drain(ctx)

	if stub.hits() != 0 {
		t.Fatalf("transport hits = %d, want 0", stub.hits())
	}

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != storage.SendFailed {
		t.Fatalf("Status = %q, want failed", got.Status)
	}
	want := fmt.Sprintf("%.1f MB exceeds the %d MB limit", float64(size)/(1<<20), resend.MaxAttachmentSize/(1<<20))
	if got.FailureReason != want {
		t.Errorf("FailureReason = %q, want %q", got.FailureReason, want)
	}
}

func TestWorkerMissingFileLocationFails(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	bookID := setupBookWithFile(t, db, libraryDir, "gone.epub", []byte("x"))
	f, err := db.FindFileByPath(ctx, "gone.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, time.Now()); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}

	sendID, err := db.EnqueueSend(ctx, bookID, "Gone", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	stub := &stubTransport{sendFunc: func(ctx context.Context, to string, a resend.Attachment) (string, error) {
		t.Error("transport called for a book with no non-missing location")
		return "", nil
	}}
	w := New(db, stub, libraryDir)
	w.drain(ctx)

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != storage.SendFailed || got.FailureReason != fileGoneReason {
		t.Errorf("send = %+v, want failed with %q", got, fileGoneReason)
	}
}

// A book whose only location was pruned (book_id gone NULL on the send_log
// row) is the same failure as every location being marked missing: there
// is nothing left to send.
func TestWorkerPrunedBookFails(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx := context.Background()

	bookID := setupBookWithFile(t, db, libraryDir, "prune.epub", []byte("x"))
	sendID, err := db.EnqueueSend(ctx, bookID, "Prune", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	f, err := db.FindFileByPath(ctx, "prune.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, old); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}
	if _, _, err := db.PruneMissingFiles(ctx, []int64{f.ID}); err != nil {
		t.Fatalf("PruneMissingFiles: %v", err)
	}

	stub := &stubTransport{sendFunc: func(ctx context.Context, to string, a resend.Attachment) (string, error) {
		t.Error("transport called for a pruned book")
		return "", nil
	}}
	w := New(db, stub, libraryDir)
	w.drain(ctx)

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.BookID.Valid {
		t.Fatalf("BookID = %v, want NULL after pruning (test setup problem, not the worker's)", got.BookID)
	}
	if got.Status != storage.SendFailed || got.FailureReason != fileGoneReason {
		t.Errorf("send = %+v, want failed with %q", got, fileGoneReason)
	}
}

// Notify must wake a worker that is already idle in Run's select, without
// waiting for the once-a-minute pollInterval tick.
func TestWorkerNotifyWakesIdleWorker(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stub := &stubTransport{sendFunc: func(ctx context.Context, to string, a resend.Attachment) (string, error) {
		return "msg_1", nil
	}}
	w := New(db, stub, libraryDir)

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	bookID := setupBookWithFile(t, db, libraryDir, "book.epub", []byte("x"))
	sendID, err := db.EnqueueSend(context.Background(), bookID, "Book", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	w.Notify()

	deadline := time.After(3 * time.Second)
	for {
		got, err := db.GetSend(context.Background(), sendID)
		if err != nil {
			t.Fatalf("GetSend: %v", err)
		}
		if got.Status == storage.SendDelivered {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("send did not reach delivered within 3s of Notify (status = %q); pollInterval is 1m, so this means Notify isn't waking the worker", got.Status)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}
}

// A job in flight when Run's context is cancelled is left in the sending
// state rather than guessed at — the recovery contract that
// storage.FailInterruptedSends resolves at next startup.
func TestWorkerCancellationLeavesRowSendingForRecovery(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	bookID := setupBookWithFile(t, db, libraryDir, "book.epub", []byte("x"))
	sendID, err := db.EnqueueSend(context.Background(), bookID, "Book", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	entered := make(chan struct{})
	stub := &stubTransport{sendFunc: func(ctx context.Context, to string, a resend.Attachment) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	w := New(db, stub, libraryDir)

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("transport was never called")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}

	got, err := db.GetSend(context.Background(), sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != storage.SendSending {
		t.Fatalf("Status = %q, want sending (the row a crash mid-send leaves behind)", got.Status)
	}

	n, err := db.FailInterruptedSends(context.Background(), "interrupted by a restart", time.Now())
	if err != nil {
		t.Fatalf("FailInterruptedSends: %v", err)
	}
	if n != 1 {
		t.Fatalf("FailInterruptedSends resolved %d rows, want 1", n)
	}

	recovered, err := db.GetSend(context.Background(), sendID)
	if err != nil || recovered == nil {
		t.Fatalf("GetSend after recovery: %+v, %v", recovered, err)
	}
	if recovered.Status != storage.SendFailed {
		t.Errorf("Status after recovery = %q, want failed", recovered.Status)
	}
}

func TestWorkerRecordsDeliveryEvenIfCancelledOnTheWayBack(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bookID := setupBookWithFile(t, db, libraryDir, "book.epub", []byte("x"))
	sendID, err := db.EnqueueSend(context.Background(), bookID, "Book", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	// Resend accepts the message and the process is told to shut down in
	// the same breath. Unlike an abandoned request, this outcome is not
	// ambiguous — the book arrived, and the row has to say so, or startup
	// recovery reports a delivered send as failed and invites a duplicate.
	stub := &stubTransport{sendFunc: func(context.Context, string, resend.Attachment) (string, error) {
		cancel()
		return "msg-1", nil
	}}

	New(db, stub, libraryDir).drain(ctx)

	got, err := db.GetSend(context.Background(), sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != storage.SendDelivered {
		t.Errorf("Status = %q, want delivered — the send completed before cancellation", got.Status)
	}
	if got.ProviderMessageID != "msg-1" {
		t.Errorf("ProviderMessageID = %q, want msg-1", got.ProviderMessageID)
	}
}

func TestWorkerStorageFailureDoesNotClaimTheFileIsGone(t *testing.T) {
	libraryDir := t.TempDir()
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	bookID := setupBookWithFile(t, db, libraryDir, "book.epub", []byte("x"))
	sendID, err := db.EnqueueSend(context.Background(), bookID, "Book", "reader@kindle.com", time.Now())
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	if _, err := db.ClaimNextSend(context.Background(), time.Now()); err != nil {
		t.Fatalf("ClaimNextSend: %v", err)
	}
	send, err := db.GetSend(context.Background(), sendID)
	if err != nil {
		t.Fatalf("GetSend: %v", err)
	}

	stub := &stubTransport{sendFunc: func(context.Context, string, resend.Attachment) (string, error) {
		t.Error("transport was called despite the file lookup failing")
		return "", nil
	}}
	// A dead context makes the book_files lookup fail while the file is
	// still sitting on disk — the shape of any storage failure, and the
	// one case where "the file is no longer in the library" would be a
	// confident lie about a book that is perfectly fine.
	cancel()
	New(db, stub, libraryDir).process(ctx, send)

	got, err := db.GetSend(context.Background(), sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != storage.SendFailed {
		t.Fatalf("Status = %q, want failed", got.Status)
	}
	if got.FailureReason != lookupFailedReason {
		t.Errorf("FailureReason = %q, want %q", got.FailureReason, lookupFailedReason)
	}
}
