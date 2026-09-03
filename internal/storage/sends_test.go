package storage

import (
	"context"
	"testing"
	"time"
)

func TestEnqueueAndClaimSend(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Piranesi", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sendID, err := db.EnqueueSend(ctx, bookID, "Piranesi", "reader@kindle.com", queuedAt)
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	claimedAt := queuedAt.Add(time.Second)
	send, err := db.ClaimNextSend(ctx, claimedAt)
	if err != nil {
		t.Fatalf("ClaimNextSend: %v", err)
	}
	if send == nil {
		t.Fatal("ClaimNextSend = nil, want the queued send")
	}
	if send.ID != sendID {
		t.Errorf("claimed id = %d, want %d", send.ID, sendID)
	}
	if send.Status != SendSending {
		t.Errorf("Status = %q, want sending", send.Status)
	}
	if !send.StartedAt.Valid || !send.StartedAt.Time.Equal(claimedAt) {
		t.Errorf("StartedAt = %v, want %v", send.StartedAt, claimedAt)
	}
	if send.BookTitle != "Piranesi" || send.RecipientAddress != "reader@kindle.com" {
		t.Errorf("send = %+v, unexpected book/recipient", send)
	}

	again, err := db.ClaimNextSend(ctx, claimedAt)
	if err != nil {
		t.Fatalf("ClaimNextSend on empty queue: %v", err)
	}
	if again != nil {
		t.Errorf("ClaimNextSend on empty queue = %+v, want nil", again)
	}
}

func TestClaimNextSendClaimsOldestFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	later := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	earlier := later.Add(-time.Hour)

	secondID, err := db.EnqueueSend(ctx, bookID, "Book", "later@kindle.com", later)
	if err != nil {
		t.Fatalf("EnqueueSend later: %v", err)
	}
	firstID, err := db.EnqueueSend(ctx, bookID, "Book", "earlier@kindle.com", earlier)
	if err != nil {
		t.Fatalf("EnqueueSend earlier: %v", err)
	}

	claimed, err := db.ClaimNextSend(ctx, later)
	if err != nil {
		t.Fatalf("ClaimNextSend: %v", err)
	}
	if claimed == nil || claimed.ID != firstID {
		t.Fatalf("ClaimNextSend = %+v, want the earlier-queued send %d (not %d)", claimed, firstID, secondID)
	}
}

func TestMarkSendDeliveredSetsFinishedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.EnqueueSend(ctx, bookID, "Book", "reader@kindle.com", queuedAt); err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	send, err := db.ClaimNextSend(ctx, queuedAt)
	if err != nil || send == nil {
		t.Fatalf("ClaimNextSend: %+v, %v", send, err)
	}

	finishedAt := queuedAt.Add(2 * time.Second)
	if err := db.MarkSendDelivered(ctx, send.ID, "msg_123", finishedAt); err != nil {
		t.Fatalf("MarkSendDelivered: %v", err)
	}

	got, err := db.GetSend(ctx, send.ID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != SendDelivered {
		t.Errorf("Status = %q, want delivered", got.Status)
	}
	if got.ProviderMessageID != "msg_123" {
		t.Errorf("ProviderMessageID = %q, want msg_123", got.ProviderMessageID)
	}
	if !got.FinishedAt.Valid || !got.FinishedAt.Time.Equal(finishedAt) {
		t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, finishedAt)
	}
}

func TestMarkSendDeliveredRefusesANonSendingRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sendID, err := db.EnqueueSend(ctx, bookID, "Book", "reader@kindle.com", queuedAt)
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	// The row is still queued, never claimed — MarkSendDelivered must not
	// touch it.
	if err := db.MarkSendDelivered(ctx, sendID, "msg_123", queuedAt.Add(time.Second)); err != nil {
		t.Fatalf("MarkSendDelivered: %v", err)
	}

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != SendQueued {
		t.Errorf("Status = %q, want the row unchanged at queued", got.Status)
	}
	if got.ProviderMessageID != "" {
		t.Errorf("ProviderMessageID = %q, want empty", got.ProviderMessageID)
	}
	if got.FinishedAt.Valid {
		t.Errorf("FinishedAt = %v, want unset", got.FinishedAt)
	}
}

func TestMarkSendFailedRefusesANonSendingRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sendID, err := db.EnqueueSend(ctx, bookID, "Book", "reader@kindle.com", queuedAt)
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	if err := db.MarkSendFailed(ctx, sendID, "boom", queuedAt.Add(time.Second)); err != nil {
		t.Fatalf("MarkSendFailed: %v", err)
	}

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.Status != SendQueued || got.FailureReason != "" {
		t.Errorf("send = %+v, want unchanged (still queued, no failure reason)", got)
	}
}

func TestSendLogStatusCheckRejectsUnknownStatus(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	// The Go API can't produce an invalid status (SendStatus is a closed
	// set of constants), so this drives one through a raw Exec to prove the
	// CHECK constraint itself is what rejects it, not application code.
	_, execErr := db.write.ExecContext(ctx, `
		INSERT INTO send_log (book_id, book_title, recipient_address, status, queued_at)
		VALUES (?, ?, ?, ?, ?)`,
		bookID, "Book", "reader@kindle.com", "bogus", formatTime(mtime))
	if execErr == nil {
		t.Fatal("insert with an unknown status succeeded, want the CHECK constraint to reject it")
	}
}

func TestPruneMissingFilesLeavesSendLogRowWithNullBookID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Vanishing Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sendID, err := db.EnqueueSend(ctx, bookID, "Vanishing Book", "reader@kindle.com", queuedAt)
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	f, err := db.FindFileByPath(ctx, "a.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, old); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}
	files, books, err := db.PruneMissingFiles(ctx, []int64{f.ID})
	if err != nil {
		t.Fatalf("PruneMissingFiles: %v", err)
	}
	if files != 1 || books != 1 {
		t.Fatalf("PruneMissingFiles = files=%d books=%d, want 1, 1", files, books)
	}

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.BookID.Valid {
		t.Errorf("BookID = %v, want NULL after the book was deleted", got.BookID)
	}
	if got.BookTitle != "Vanishing Book" {
		t.Errorf("BookTitle = %q, want the snapshot to survive the book's deletion", got.BookTitle)
	}
}

func TestCreateRecipientIsIdempotentAcrossCase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	firstID, err := db.CreateRecipient(ctx, "Reader@kindle.com", "Mine", now)
	if err != nil {
		t.Fatalf("CreateRecipient: %v", err)
	}
	secondID, err := db.CreateRecipient(ctx, "reader@kindle.com", "Mine, again", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRecipient (different case): %v", err)
	}
	if firstID != secondID {
		t.Errorf("second CreateRecipient returned id %d, want the same id %d as the first", secondID, firstID)
	}

	recipients, err := db.ListRecipients(ctx)
	if err != nil {
		t.Fatalf("ListRecipients: %v", err)
	}
	if len(recipients) != 1 {
		t.Fatalf("ListRecipients = %+v, want exactly one row", recipients)
	}
}

func TestListRecipientsOrdersMostRecentlyUsedFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	neverUsedID, err := db.CreateRecipient(ctx, "never@kindle.com", "", now)
	if err != nil {
		t.Fatalf("CreateRecipient never: %v", err)
	}
	usedID, err := db.CreateRecipient(ctx, "used@kindle.com", "", now)
	if err != nil {
		t.Fatalf("CreateRecipient used: %v", err)
	}

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, now)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	if _, err := db.EnqueueSend(ctx, bookID, "Book", "used@kindle.com", now.Add(time.Hour)); err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	recipients, err := db.ListRecipients(ctx)
	if err != nil {
		t.Fatalf("ListRecipients: %v", err)
	}
	if len(recipients) != 2 {
		t.Fatalf("ListRecipients = %+v, want 2", recipients)
	}
	if recipients[0].ID != usedID {
		t.Errorf("ListRecipients[0] = %+v, want the used recipient %d first", recipients[0], usedID)
	}
	if recipients[1].ID != neverUsedID {
		t.Errorf("ListRecipients[1] = %+v, want the never-used recipient %d last", recipients[1], neverUsedID)
	}
	if !recipients[0].LastUsedAt.Valid {
		t.Error("used recipient LastUsedAt = invalid, want set by EnqueueSend")
	}
	if recipients[1].LastUsedAt.Valid {
		t.Error("never-used recipient LastUsedAt = valid, want unset")
	}
}

func TestFailInterruptedSendsFailsOnlySendingRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	interruptedID, err := db.EnqueueSend(ctx, bookID, "Book", "interrupted@kindle.com", queuedAt)
	if err != nil {
		t.Fatalf("EnqueueSend interrupted: %v", err)
	}
	stillQueuedID, err := db.EnqueueSend(ctx, bookID, "Book", "queued@kindle.com", queuedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("EnqueueSend still-queued: %v", err)
	}

	// Claim only the oldest (interruptedID), then simulate a crash by never
	// resolving it — stillQueuedID is never claimed.
	claimed, err := db.ClaimNextSend(ctx, queuedAt)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextSend: %+v, %v", claimed, err)
	}
	if claimed.ID != interruptedID {
		t.Fatalf("ClaimNextSend claimed %d, want the oldest-queued %d", claimed.ID, interruptedID)
	}

	at := queuedAt.Add(time.Minute)
	n, err := db.FailInterruptedSends(ctx, "interrupted by a restart", at)
	if err != nil {
		t.Fatalf("FailInterruptedSends: %v", err)
	}
	if n != 1 {
		t.Errorf("FailInterruptedSends returned %d, want 1", n)
	}

	interrupted, err := db.GetSend(ctx, interruptedID)
	if err != nil || interrupted == nil {
		t.Fatalf("GetSend interrupted: %+v, %v", interrupted, err)
	}
	if interrupted.Status != SendFailed || interrupted.FailureReason != "interrupted by a restart" {
		t.Errorf("interrupted send = %+v, want failed with the restart reason", interrupted)
	}
	if !interrupted.FinishedAt.Valid {
		t.Error("interrupted send FinishedAt = unset, want set")
	}

	stillQueued, err := db.GetSend(ctx, stillQueuedID)
	if err != nil || stillQueued == nil {
		t.Fatalf("GetSend still-queued: %+v, %v", stillQueued, err)
	}
	if stillQueued.Status != SendQueued {
		t.Errorf("still-queued send status = %q, want queued (untouched)", stillQueued.Status)
	}
}

func TestLatestSendForBookOrdersMostRecentFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	earlier := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)

	if _, err := db.EnqueueSend(ctx, bookID, "Book", "first@kindle.com", earlier); err != nil {
		t.Fatalf("EnqueueSend earlier: %v", err)
	}
	latestID, err := db.EnqueueSend(ctx, bookID, "Book", "second@kindle.com", later)
	if err != nil {
		t.Fatalf("EnqueueSend later: %v", err)
	}

	got, err := db.LatestSendForBook(ctx, bookID)
	if err != nil || got == nil {
		t.Fatalf("LatestSendForBook: %+v, %v", got, err)
	}
	if got.ID != latestID {
		t.Errorf("LatestSendForBook = %+v, want the most recently queued send %d", got, latestID)
	}
}

func TestLatestSendForBookUnknownBookReturnsNil(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	got, err := db.LatestSendForBook(ctx, 99999)
	if err != nil {
		t.Fatalf("LatestSendForBook: %v", err)
	}
	if got != nil {
		t.Errorf("LatestSendForBook(unknown) = %+v, want nil", got)
	}
}

func TestListSendsSinceOrdersNewestFirstAndRespectsSinceAndLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}

	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tooOld := since.Add(-time.Hour)
	oldest := since
	middle := since.Add(time.Hour)
	newest := since.Add(2 * time.Hour)

	if _, err := db.EnqueueSend(ctx, bookID, "Book", "excluded@kindle.com", tooOld); err != nil {
		t.Fatalf("EnqueueSend tooOld: %v", err)
	}
	oldestID, err := db.EnqueueSend(ctx, bookID, "Book", "oldest@kindle.com", oldest)
	if err != nil {
		t.Fatalf("EnqueueSend oldest: %v", err)
	}
	middleID, err := db.EnqueueSend(ctx, bookID, "Book", "middle@kindle.com", middle)
	if err != nil {
		t.Fatalf("EnqueueSend middle: %v", err)
	}
	newestID, err := db.EnqueueSend(ctx, bookID, "Book", "newest@kindle.com", newest)
	if err != nil {
		t.Fatalf("EnqueueSend newest: %v", err)
	}

	sends, err := db.ListSendsSince(ctx, since, 500)
	if err != nil {
		t.Fatalf("ListSendsSince: %v", err)
	}
	if len(sends) != 3 {
		t.Fatalf("ListSendsSince = %d rows, want 3 (the pre-since row excluded)", len(sends))
	}
	if got := []int64{sends[0].ID, sends[1].ID, sends[2].ID}; got[0] != newestID || got[1] != middleID || got[2] != oldestID {
		t.Errorf("ListSendsSince order = %v, want newest first [%d %d %d]", got, newestID, middleID, oldestID)
	}

	capped, err := db.ListSendsSince(ctx, since, 2)
	if err != nil {
		t.Fatalf("ListSendsSince capped: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("ListSendsSince with limit 2 = %d rows, want 2", len(capped))
	}
	if capped[0].ID != newestID || capped[1].ID != middleID {
		t.Errorf("ListSendsSince capped = %+v, want the two newest rows kept, not the oldest", capped)
	}
}

// The regression the handoff's own "joined to books and recipients" note
// would have caused: send_log denormalises book_title precisely so a
// pruned book's history survives it, and a join would silently drop this
// row.
func TestListSendsSinceIncludesSendForADeletedBook(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Vanishing Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sendID, err := db.EnqueueSend(ctx, bookID, "Vanishing Book", "reader@kindle.com", queuedAt)
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	f, err := db.FindFileByPath(ctx, "a.epub")
	if err != nil || f == nil {
		t.Fatalf("FindFileByPath: %+v, %v", f, err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := db.SetFilesMissing(ctx, []int64{f.ID}, old); err != nil {
		t.Fatalf("SetFilesMissing: %v", err)
	}
	if _, _, err := db.PruneMissingFiles(ctx, []int64{f.ID}); err != nil {
		t.Fatalf("PruneMissingFiles: %v", err)
	}

	sends, err := db.ListSendsSince(ctx, queuedAt.Add(-time.Hour), 500)
	if err != nil {
		t.Fatalf("ListSendsSince: %v", err)
	}
	if len(sends) != 1 || sends[0].ID != sendID {
		t.Fatalf("ListSendsSince = %+v, want the one send for the now-deleted book", sends)
	}
	if sends[0].BookID.Valid {
		t.Errorf("BookID = %v, want NULL after the book was pruned", sends[0].BookID)
	}
	if sends[0].BookTitle != "Vanishing Book" {
		t.Errorf("BookTitle = %q, want the snapshot to survive the book's deletion", sends[0].BookTitle)
	}
}

// Same idea, the other denormalised column: a send still names its
// recipient after the address has been removed from recipients.
func TestListSendsSinceIncludesSendToARemovedRecipient(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.CreateRecipient(ctx, "reader@kindle.com", "", queuedAt); err != nil {
		t.Fatalf("CreateRecipient: %v", err)
	}
	if _, err := db.EnqueueSend(ctx, bookID, "Book", "reader@kindle.com", queuedAt); err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}
	if deleted, err := db.DeleteRecipient(ctx, "reader@kindle.com"); err != nil || !deleted {
		t.Fatalf("DeleteRecipient: deleted=%v, err=%v", deleted, err)
	}

	sends, err := db.ListSendsSince(ctx, queuedAt.Add(-time.Hour), 500)
	if err != nil {
		t.Fatalf("ListSendsSince: %v", err)
	}
	if len(sends) != 1 || sends[0].RecipientAddress != "reader@kindle.com" {
		t.Fatalf("ListSendsSince = %+v, want the send to the now-removed address to survive", sends)
	}
}

func TestDeleteRecipient(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	if _, err := db.CreateRecipient(ctx, "reader@kindle.com", "Mine", now); err != nil {
		t.Fatalf("CreateRecipient: %v", err)
	}

	deleted, err := db.DeleteRecipient(ctx, "Reader@Kindle.com")
	if err != nil {
		t.Fatalf("DeleteRecipient: %v", err)
	}
	if !deleted {
		t.Error("DeleteRecipient (different case) = false, want true — the column is COLLATE NOCASE")
	}

	recipients, err := db.ListRecipients(ctx)
	if err != nil {
		t.Fatalf("ListRecipients: %v", err)
	}
	if len(recipients) != 0 {
		t.Errorf("ListRecipients after delete = %+v, want none", recipients)
	}

	again, err := db.DeleteRecipient(ctx, "reader@kindle.com")
	if err != nil {
		t.Fatalf("DeleteRecipient (already gone): %v", err)
	}
	if again {
		t.Error("DeleteRecipient on an already-removed address = true, want false")
	}
}

// The schema already guarantees this — recipient_address is a plain
// string, not a foreign key — so this test is what stops a future "tidy up
// orphaned history" migration from quietly reversing that decision.
func TestDeleteRecipientLeavesSendLogUntouched(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mtime := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	bookID, _, _, err := db.CreateBookWithFile(ctx, Book{ContentHash: "hash-1", Title: "Book", Format: "epub"}, nil, "a.epub", 100, mtime)
	if err != nil {
		t.Fatalf("CreateBookWithFile: %v", err)
	}
	queuedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.CreateRecipient(ctx, "reader@kindle.com", "", queuedAt); err != nil {
		t.Fatalf("CreateRecipient: %v", err)
	}
	sendID, err := db.EnqueueSend(ctx, bookID, "Book", "reader@kindle.com", queuedAt)
	if err != nil {
		t.Fatalf("EnqueueSend: %v", err)
	}

	before, err := db.ListSendsSince(ctx, queuedAt.Add(-time.Hour), 500)
	if err != nil {
		t.Fatalf("ListSendsSince before: %v", err)
	}

	if deleted, err := db.DeleteRecipient(ctx, "reader@kindle.com"); err != nil || !deleted {
		t.Fatalf("DeleteRecipient: deleted=%v, err=%v", deleted, err)
	}

	after, err := db.ListSendsSince(ctx, queuedAt.Add(-time.Hour), 500)
	if err != nil {
		t.Fatalf("ListSendsSince after: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("send_log row count changed after DeleteRecipient: before=%d after=%d", len(before), len(after))
	}

	got, err := db.GetSend(ctx, sendID)
	if err != nil || got == nil {
		t.Fatalf("GetSend: %+v, %v", got, err)
	}
	if got.RecipientAddress != "reader@kindle.com" {
		t.Errorf("RecipientAddress = %q, want it unchanged by DeleteRecipient", got.RecipientAddress)
	}
}
