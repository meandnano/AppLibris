// Package service is the layer beneath the web (and, later, API) handlers:
// business logic and storage access live here, per DESIGN.md's "Layering
// for a future API," so handlers stay thin transports over these methods.
package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"library/internal/storage"
)

// Service exposes the application's operations over a storage.DB.
type Service struct {
	db *storage.DB

	// now is the service's clock. The plan for inline editing puts time
	// ownership here rather than letting each write reach for time.Now, so
	// modified_at propagation can be asserted at this boundary without a
	// timing assumption. Defaulted by New, overridden in package tests.
	now func() time.Time

	// Notify, if set, is called after a send is successfully queued —
	// cmd/server's hook into the worker's poke, so a send starts the
	// instant the button is pressed instead of waiting for its next
	// poll tick. nil in tests and whenever sending is unconfigured.
	// A function field rather than an interface: it's one nullary call,
	// and an interface would make this package depend on internal/sender,
	// which depends on storage — a cycle waiting to happen.
	Notify func()

	// NotifyEnrichment is the same hook for the enrichment worker, kept
	// separate rather than multiplexed: the two queues are drained by two
	// workers, and poking the wrong one would leave a job sitting until
	// its own poll tick. nil in tests and whenever no provider is
	// configured.
	NotifyEnrichment func()
}

// New returns a Service backed by db.
func New(db *storage.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// BookSummary is what a library-browse entry needs.
type BookSummary struct {
	ID        int64
	Title     string
	Authors   []string
	Format    string
	CoverPath string
	// Locations is how many places on disk this book's content sits at.
	// 1 for almost every book; more than 1 is what the grid flags.
	Locations int
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
	// FieldSources maps a field name to where its current value came from
	// — "embedded", "manual", or a provider's name. The detail page marks
	// only the provider ones; see internal/web's makeFieldViews for why
	// provenance is rendered as a caveat rather than a label on all three.
	FieldSources map[string]string
}

// EnrichmentState is one enrichment job as the detail page's control needs
// it, shaped exactly as SendState is — see enrichmentStateFrom for why the
// two stay separate rather than sharing a generic job type.
type EnrichmentState struct {
	ID            int64
	BookID        int64
	Status        string
	FailureReason string
	// UpdatedFields names what the run wrote, so the terminal fragment can
	// say which fields moved. Empty is the "nothing to add" case, which is
	// a success: it is the common outcome for a book whose embedded
	// metadata is already complete, and rendering it as a failure would
	// train people to distrust a working feature.
	UpdatedFields []string
	At            time.Time // finished_at once terminal, else queued_at
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

	sources, err := s.db.FieldSourcesForBook(ctx, id)
	if err != nil {
		return nil, err
	}
	fieldSources := make(map[string]string, len(sources))
	for field, source := range sources {
		fieldSources[string(field)] = source
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
		FieldSources:  fieldSources,
	}, nil
}

// summarize attaches authors and a location count to books and shapes both
// into BookSummary. ListBookAuthors and CountFilesByBook both load the
// whole library's map rather than a filtered variant scoped to books — at
// this library's scale the whole-table map is cheaper than the extra query
// plumbing a filtered version would need, and ListBooks already set that
// precedent before SearchBooks needed the same shape.
//
// locationsByBook[b.ID] reads as 0 for a book with no book_files rows — a
// state that should be exceptional in practice (the last location's
// deletion prunes the book in the same transaction) but that
// CountFilesByBook itself does not rule out; it just reports book_files as
// it stands. Since 0 and 1 both mean "don't show the multi-location badge",
// this normalizes 0 up to 1 rather than carrying the storage layer's raw
// count forward, so BookSummary.Locations reads as an actual location
// count instead of an artifact of the map being unset.
func (s *Service) summarize(ctx context.Context, books []storage.Book) ([]BookSummary, error) {
	authorsByBook, err := s.db.ListBookAuthors(ctx)
	if err != nil {
		return nil, err
	}
	locationsByBook, err := s.db.CountFilesByBook(ctx)
	if err != nil {
		return nil, err
	}

	summaries := make([]BookSummary, len(books))
	for i, b := range books {
		locations := locationsByBook[b.ID]
		if locations == 0 {
			locations = 1
		}
		summaries[i] = BookSummary{
			ID:        b.ID,
			Title:     b.Title,
			Authors:   authorsByBook[b.ID],
			Format:    b.Format,
			CoverPath: b.CoverPath,
			Locations: locations,
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

	now := s.now()
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

// EnrichBook puts bookID on the enrichment queue and returns the resulting
// state, or nil, nil for a book that doesn't exist — the same
// absent-isn't-an-error contract GetBook uses, so the handler turns it into
// a 404 the same way.
//
// The state is read back rather than synthesised, because EnqueueEnrichment
// is idempotent while a job is already queued: pressing the button twice
// does not make a second promise, and what the caller wants back either way
// is the job the book actually has. LatestEnrichmentForBook returns that in
// both cases — the newly inserted row, or the queued one that blocked it.
func (s *Service) EnrichBook(ctx context.Context, bookID int64) (*EnrichmentState, error) {
	book, err := s.db.FindBookByID(ctx, bookID)
	if err != nil {
		return nil, err
	}
	if book == nil {
		return nil, nil
	}

	if _, err := s.db.EnqueueEnrichment(ctx, bookID, s.now()); err != nil {
		return nil, err
	}

	if s.NotifyEnrichment != nil {
		s.NotifyEnrichment()
	}

	job, err := s.db.LatestEnrichmentForBook(ctx, bookID)
	if err != nil {
		return nil, err
	}
	return enrichmentStateFrom(job), nil
}

// EnrichmentState returns jobID's current state, or nil if it doesn't
// exist — the status poll's lookup, mirroring SendState.
func (s *Service) EnrichmentState(ctx context.Context, jobID int64) (*EnrichmentState, error) {
	job, err := s.db.GetEnrichmentJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return enrichmentStateFrom(job), nil
}

// LatestEnrichment returns the most recent job for bookID, or nil if it has
// never been enriched — the detail page's initial render, mirroring
// LatestSend, so a page loaded mid-job resumes polling instead of showing a
// bare button.
func (s *Service) LatestEnrichment(ctx context.Context, bookID int64) (*EnrichmentState, error) {
	job, err := s.db.LatestEnrichmentForBook(ctx, bookID)
	if err != nil {
		return nil, err
	}
	return enrichmentStateFrom(job), nil
}

// enrichmentAt is enrichment's half of the same collapse sendAt makes:
// finished_at once the job is terminal, queued_at otherwise, so the
// template picks one field and never branches on which produced it.
func enrichmentAt(job storage.EnrichmentJob) time.Time {
	if job.FinishedAt.Valid {
		return job.FinishedAt.Time
	}
	return job.QueuedAt
}

// enrichmentStateFrom shapes a storage.EnrichmentJob into an
// EnrichmentState, splitting the stored comma-separated field list back
// into names.
//
// The symmetry with sendStateFrom is now three pairs deep — state, latest,
// shaping — and it stays two parallel surfaces rather than one generic job
// type on purpose. An abstraction over exactly two cases has no third
// instance to test its shape against, and the two differ in the part that
// would have to be generic anyway: a send's terminal detail is an address
// and a failure reason, an enrichment's is a list of fields it wrote. If a
// third queued-job type ever appears, that is the moment.
func enrichmentStateFrom(job *storage.EnrichmentJob) *EnrichmentState {
	if job == nil {
		return nil
	}
	var updated []string
	for _, name := range strings.Split(job.UpdatedFields, ",") {
		if name = strings.TrimSpace(name); name != "" {
			updated = append(updated, name)
		}
	}
	return &EnrichmentState{
		ID:            job.ID,
		BookID:        job.BookID,
		Status:        string(job.Status),
		FailureReason: job.FailureReason,
		UpdatedFields: updated,
		At:            enrichmentAt(*job),
	}
}

// sendAt collapses a send's "when did this happen" to one instant —
// finished_at once the send is terminal, queued_at otherwise — shared by
// sendStateFrom and sendRecordFrom so the detail page's status box and the
// history view can't drift on the same rule.
func sendAt(send storage.Send) time.Time {
	if send.FinishedAt.Valid {
		return send.FinishedAt.Time
	}
	return send.QueuedAt
}

// sendStateFrom shapes a storage.Send into a SendState. The at-a-glance
// timestamp collapses to one field via sendAt so the template needs no
// branching to pick it.
func sendStateFrom(send *storage.Send) *SendState {
	if send == nil {
		return nil
	}
	return &SendState{
		ID:            send.ID,
		BookID:        send.BookID.Int64, // 0 if send.BookID is NULL (the book was since pruned)
		Status:        string(send.Status),
		Recipient:     send.RecipientAddress,
		FailureReason: send.FailureReason,
		At:            sendAt(*send),
	}
}

// sendHistoryWindow is how far back the send history view looks — plate
// 07's "last 30 days" scope line. An unbounded log is a page whose render
// cost grows forever; a rolling window keeps it bounded without an
// archival or pagination story this library doesn't need at DESIGN.md's
// stated volume ("a handful of sends a week").
const sendHistoryWindow = 30 * 24 * time.Hour

// SendHistoryLimit caps how many rows the history view will ever render,
// even inside the window. A time window alone is not a bound — nothing
// stops a scripted burst from putting thousands of rows inside 30 days —
// and at DESIGN.md's stated volume, 500 rows is roughly a decade of
// ordinary use, so this should never actually bite in practice. It exists
// so that if it ever does, SendHistory can say so instead of the page
// silently rendering a "last 30 days" that is no longer true.
//
// Exported, the same reason MaxMetadataValueBytes is: internal/web spells
// the exact number out in the scope line ("last 30 days · 500 most
// recent") when SendHistory reports truncated, and a copy of the literal
// there would drift from this the day either one changes.
const SendHistoryLimit = 500

// SendRecord is one row of the send history view.
type SendRecord struct {
	SendID        int64
	BookID        int64 // 0 when the book has since been deleted
	BookTitle     string
	Recipient     string
	Status        string
	FailureReason string
	At            time.Time // finished_at once terminal, else queued_at
}

// sendRecordFrom shapes one storage.Send into a SendRecord, sharing sendAt
// with sendStateFrom so the detail page and the history view report the
// same instant for the same send.
func sendRecordFrom(send storage.Send) SendRecord {
	return SendRecord{
		SendID:        send.ID,
		BookID:        send.BookID.Int64, // 0 when the book has since been deleted
		BookTitle:     send.BookTitle,
		Recipient:     send.RecipientAddress,
		Status:        string(send.Status),
		FailureReason: send.FailureReason,
		At:            sendAt(send),
	}
}

// SendHistory returns recent sends, newest first, over the last
// sendHistoryWindow measured from the service clock — so the window is
// testable without waiting. truncated reports whether SendHistoryLimit
// actually cut rows out of that window: a fixed "last 30 days" line over a
// silently truncated list would be a claim the page cannot support, and
// this page exists to be believed, so the caller has to be able to say so.
//
// Asking storage for one row past the limit is what makes truncated
// answerable without a second COUNT query: getting back SendHistoryLimit+1
// rows proves at least one more exists in the window, at which point the
// extra row is trimmed back off before returning.
func (s *Service) SendHistory(ctx context.Context) (records []SendRecord, truncated bool, err error) {
	since := s.now().Add(-sendHistoryWindow)
	sends, err := s.db.ListSendsSince(ctx, since, SendHistoryLimit+1)
	if err != nil {
		return nil, false, err
	}
	truncated = len(sends) > SendHistoryLimit
	if truncated {
		sends = sends[:SendHistoryLimit]
	}

	records = make([]SendRecord, len(sends))
	for i, send := range sends {
		records[i] = sendRecordFrom(send)
	}
	return records, truncated, nil
}

// RemoveRecipient deletes a saved address, reporting whether one went. See
// storage.DeleteRecipient for the case-insensitive match and why send_log
// is left untouched.
func (s *Service) RemoveRecipient(ctx context.Context, address string) (bool, error) {
	return s.db.DeleteRecipient(ctx, address)
}
