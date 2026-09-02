// Package service is the layer beneath the web (and, later, API) handlers:
// business logic and storage access live here, per DESIGN.md's "Layering
// for a future API," so handlers stay thin transports over these methods.
package service

import (
	"context"
	"errors"
	"net/mail"
	"time"

	"library/internal/storage"
)

// Service exposes the application's operations over a storage.DB.
type Service struct {
	db *storage.DB

	// Notify, if set, is called after a send is successfully queued —
	// cmd/server's hook into the worker's poke, so a send starts the
	// instant the button is pressed instead of waiting for its next
	// poll tick. nil in tests and whenever sending is unconfigured.
	// A function field rather than an interface: it's one nullary call,
	// and an interface would make this package depend on internal/sender,
	// which depends on storage — a cycle waiting to happen.
	Notify func()
}

// New returns a Service backed by db.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// BookSummary is what a library-browse entry needs.
type BookSummary struct {
	ID        int64
	Title     string
	Authors   []string
	Format    string
	CoverPath string
}

// ListBooks returns every book, ordered by sort_title, with its authors attached.
func (s *Service) ListBooks(ctx context.Context) ([]BookSummary, error) {
	books, err := s.db.ListBooks(ctx)
	if err != nil {
		return nil, err
	}
	return s.summarize(ctx, books)
}

// SearchBooks returns every book matching query, with its authors attached,
// ordered by sort_title. A blank or whitespace-only query — the state
// SanitizeFTSQuery reports by returning "" — is treated as no search at
// all: the empty search box and a freshly-loaded page are the same state,
// so the handler needs no special-casing between them. Anything else, even
// input that's only punctuation or an FTS5 keyword like AND, still becomes
// a literal search term rather than degrading to "no search" — see
// SanitizeFTSQuery.
//
// SearchResult.Searched reports which of those two happened, so a caller
// rendering a "searching" state doesn't have to re-derive it from the raw
// query and get it wrong. Blank-after-trimming is not the only input that sanitizes
// to nothing: SanitizeFTSQuery also strips control characters, so "?q=%00"
// is a non-blank query that is nonetheless no search at all. Deciding it
// here keeps the one definition of "this was a search" next to the one
// place that can answer it.
// SearchResult is what one search rendered: the matching books, whether a
// search actually ran, and which indexed fields produced the matches.
// Fields carries books_fts column names ("title", "authors",
// "description", "isbn") and is empty when nothing matched or no search
// ran — the results line names them so a hit on a description or an ISBN
// isn't a mystery.
type SearchResult struct {
	Books    []BookSummary
	Searched bool
	Fields   []string
}

func (s *Service) SearchBooks(ctx context.Context, query string) (SearchResult, error) {
	sanitized := storage.SanitizeFTSQuery(query)
	if sanitized == "" {
		books, err := s.ListBooks(ctx)
		return SearchResult{Books: books}, err
	}

	books, err := s.db.SearchBooks(ctx, sanitized)
	if err != nil {
		return SearchResult{Searched: true}, err
	}
	summaries, err := s.summarize(ctx, books)
	if err != nil {
		return SearchResult{Searched: true}, err
	}

	result := SearchResult{Books: summaries, Searched: true}
	if len(summaries) > 0 {
		// Only worth asking when something matched: with no results there
		// are no fields to name, and the no-matches state says which
		// fields were searched instead.
		result.Fields, err = s.db.MatchedSearchFields(ctx, sanitized)
		if err != nil {
			return SearchResult{Searched: true}, err
		}
	}
	return result, nil
}

// CountBooks returns the total number of books, independent of any search
// filter.
func (s *Service) CountBooks(ctx context.Context) (int, error) {
	return s.db.CountBooks(ctx)
}

// BookDetail is what the book detail page needs.
type BookDetail struct {
	ID            int64
	Title         string
	Authors       []string
	Publisher     string
	PublishedDate string
	Language      string
	ISBN          string
	Description   string
	CoverPath     string
	Format        string
	FileSize      int64
	HasFileSize   bool
	AddedAt       time.Time
	Locations     []FileLocation
}

// FileLocation is one of a book's physical file locations, as the detail
// page needs it — just enough to list it and flag it if it's within its
// missing-file grace period.
type FileLocation struct {
	Path    string
	Missing bool
}

// GetBook assembles one book's full detail, or nil, nil if id doesn't
// exist — "absent" is not an error at this layer, consistent with the
// storage finders; the web handler turns it into a 404.
//
// FileSize is taken from the book's first location rather than carried
// per-location: every location of one book is byte-identical by
// construction (content hash is identity), so their sizes are equal, and
// showing a size per path would imply a difference that cannot exist. A
// book can't actually be observed with zero locations — the last
// location's deletion prunes the book in the same transaction — but if
// that race is ever hit anyway, this renders no size rather than erroring.
// HasFileSize is what says so: without it a book with no location is
// indistinguishable from one whose file is genuinely zero bytes, and the
// page would claim "0 B" for a size it does not actually know.
func (s *Service) GetBook(ctx context.Context, id int64) (*BookDetail, error) {
	book, err := s.db.FindBookByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if book == nil {
		return nil, nil
	}

	authors, err := s.db.ListAuthorsForBook(ctx, id)
	if err != nil {
		return nil, err
	}

	files, err := s.db.ListBookFiles(ctx, id)
	if err != nil {
		return nil, err
	}

	var fileSize int64
	hasFileSize := len(files) > 0
	locations := make([]FileLocation, len(files))
	for i, f := range files {
		if i == 0 {
			fileSize = f.FileSize
		}
		locations[i] = FileLocation{Path: f.FilePath, Missing: f.MissingSince.Valid}
	}

	return &BookDetail{
		ID:            book.ID,
		Title:         book.Title,
		Authors:       authors,
		Publisher:     book.Publisher,
		PublishedDate: book.PublishedDate,
		Language:      book.Language,
		ISBN:          book.ISBN,
		Description:   book.Description,
		CoverPath:     book.CoverPath,
		Format:        book.Format,
		FileSize:      fileSize,
		HasFileSize:   hasFileSize,
		AddedAt:       book.AddedAt,
		Locations:     locations,
	}, nil
}

// summarize attaches authors to books and shapes both into BookSummary.
// ListBookAuthors loads every book's authors rather than a filtered
// variant scoped to books — at this library's scale the whole-table map is
// cheaper than the extra query plumbing a filtered version would need, and
// ListBooks already set that precedent before SearchBooks needed the same
// shape.
func (s *Service) summarize(ctx context.Context, books []storage.Book) ([]BookSummary, error) {
	authorsByBook, err := s.db.ListBookAuthors(ctx)
	if err != nil {
		return nil, err
	}

	summaries := make([]BookSummary, len(books))
	for i, b := range books {
		summaries[i] = BookSummary{
			ID:        b.ID,
			Title:     b.Title,
			Authors:   authorsByBook[b.ID],
			Format:    b.Format,
			CoverPath: b.CoverPath,
		}
	}
	return summaries, nil
}

// ErrInvalidAddress is returned by QueueSend when the given address doesn't
// parse as an email address. The handler renders this as a field error on
// the send form, not a 500.
var ErrInvalidAddress = errors.New("service: invalid recipient address")

// SendState is what the send status box needs — the shared shape for the
// detail page's initial render and every polled fragment, so the template
// branches on one set of fields regardless of which produced them. BookID
// is what lets the poll route (scoped under a book id in its URL) reject a
// send that belongs to a different book, rather than leak its status
// across the mismatch; it is 0 if the book has since been pruned, which a
// real book id can never be.
type SendState struct {
	ID            int64
	BookID        int64
	Status        string
	Recipient     string
	FailureReason string
	At            time.Time // finished_at once terminal, else queued_at
}

// RecipientOption is one entry in the send control's recipient picker.
type RecipientOption struct{ Address, Label string }

// Recipients returns the saved recipients for the picker, most-recently-
// used first — see storage.ListRecipients for the ordering.
func (s *Service) Recipients(ctx context.Context) ([]RecipientOption, error) {
	recipients, err := s.db.ListRecipients(ctx)
	if err != nil {
		return nil, err
	}
	options := make([]RecipientOption, len(recipients))
	for i, r := range recipients {
		options[i] = RecipientOption{Address: r.Address, Label: r.Label}
	}
	return options, nil
}

// QueueSend is where send-to-Kindle's business rules live, per DESIGN.md's
// layering note that a handler must stay "parse request, call service
// method, render":
//
//   - address is validated with net/mail.ParseAddress and only its mailbox
//     (addr.Address) is stored, so a pasted "Mike <mike@kindle.com>" saves
//     the address, not the display name. An unparseable address returns
//     ErrInvalidAddress and queues nothing.
//   - bookID that doesn't exist returns nil, nil — the same absent-isn't-
//     an-error contract GetBook uses, so the handler turns it into a 404
//     the same way. This reads the book directly via storage rather than
//     through GetBook, which also loads authors and file locations that a
//     title snapshot has no use for.
//   - The recipient is saved (idempotently — re-adding a known address is
//     a user slip, not an error) before the send is queued, and Notify is
//     called only once both writes succeed.
func (s *Service) QueueSend(ctx context.Context, bookID int64, address, label string) (*SendState, error) {
	parsed, err := mail.ParseAddress(address)
	if err != nil {
		return nil, ErrInvalidAddress
	}

	book, err := s.db.FindBookByID(ctx, bookID)
	if err != nil {
		return nil, err
	}
	if book == nil {
		return nil, nil
	}

	now := time.Now()
	if _, err := s.db.CreateRecipient(ctx, parsed.Address, label, now); err != nil {
		return nil, err
	}
	sendID, err := s.db.EnqueueSend(ctx, bookID, book.Title, parsed.Address, now)
	if err != nil {
		return nil, err
	}

	if s.Notify != nil {
		s.Notify()
	}

	send, err := s.db.GetSend(ctx, sendID)
	if err != nil {
		return nil, err
	}
	return sendStateFrom(send), nil
}

// SendState returns sendID's current state, or nil if it doesn't exist —
// the status poll's lookup.
func (s *Service) SendState(ctx context.Context, sendID int64) (*SendState, error) {
	send, err := s.db.GetSend(ctx, sendID)
	if err != nil {
		return nil, err
	}
	return sendStateFrom(send), nil
}

// LatestSend returns the most recent send for bookID, or nil if it has
// never been sent — the detail page's initial render, so a page loaded
// mid-send resumes polling and one loaded after a completed send shows its
// outcome, instead of both showing a bare button.
func (s *Service) LatestSend(ctx context.Context, bookID int64) (*SendState, error) {
	send, err := s.db.LatestSendForBook(ctx, bookID)
	if err != nil {
		return nil, err
	}
	return sendStateFrom(send), nil
}

// sendStateFrom shapes a storage.Send into a SendState. The at-a-glance
// timestamp collapses to one field — finished_at once a send is terminal,
// queued_at otherwise — so the template needs no branching to pick it.
func sendStateFrom(send *storage.Send) *SendState {
	if send == nil {
		return nil
	}
	at := send.QueuedAt
	if send.FinishedAt.Valid {
		at = send.FinishedAt.Time
	}
	return &SendState{
		ID:            send.ID,
		BookID:        send.BookID.Int64, // 0 if send.BookID is NULL (the book was since pruned)
		Status:        string(send.Status),
		Recipient:     send.RecipientAddress,
		FailureReason: send.FailureReason,
		At:            at,
	}
}
