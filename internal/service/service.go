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
