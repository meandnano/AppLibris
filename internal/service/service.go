// Package service is the layer beneath the web (and, later, API) handlers:
// business logic and storage access live here, per DESIGN.md's "Layering
// for a future API," so handlers stay thin transports over these methods.
package service

import (
	"context"

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
