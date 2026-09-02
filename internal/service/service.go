// Package service is the layer beneath the web (and, later, API) handlers:
// business logic and storage access live here, per DESIGN.md's "Layering
// for a future API," so handlers stay thin transports over these methods.
package service

import (
	"context"
	"time"

	"library/internal/storage"
)

// Service exposes the application's operations over a storage.DB.
type Service struct {
	db *storage.DB
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
func (s *Service) SearchBooks(ctx context.Context, query string) ([]BookSummary, error) {
	sanitized := storage.SanitizeFTSQuery(query)
	if sanitized == "" {
		return s.ListBooks(ctx)
	}

	books, err := s.db.SearchBooks(ctx, sanitized)
	if err != nil {
		return nil, err
	}
	return s.summarize(ctx, books)
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
