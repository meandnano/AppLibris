package service

import (
	"context"
	"errors"
	"io"

	"library/internal/importer"
)

// ErrImportDisabled is what every import method answers when cmd/server
// found the library directory unwritable, or built no Stager at all. The
// routes stay registered so a stale tab gets this explanation rather than a
// 404.
var ErrImportDisabled = errors.New("service: import is not available")

// ImportPreview is one staged import as the import page needs it: the
// header fields BookDetail carries, plus what the file is, what it would be
// called, and what the index already knows about it.
//
// HasCover rather than a cover URL, the same split BookDetail makes with
// CoverPath: where a cover is served from is the transport's question.
type ImportPreview struct {
	ID            string
	OriginalName  string
	LibraryName   string
	Title         string
	Authors       []string
	Publisher     string
	PublishedDate string
	Language      string
	ISBN          string
	Description   string
	Format        string
	Size          int64
	HasCover      bool

	// Verdict is "new", "exists" or "title-match" — see importer.Verdict.
	// Importing is offered for the first and the third; the second only
	// links to the book the library already holds.
	Verdict       string
	ExistingID    int64
	ExistingTitle string
}

// ImportEnabled reports whether this run can import at all, which is what
// decides whether the nav offers the page.
func (s *Service) ImportEnabled() bool {
	return s.importer != nil && s.importer.Enabled()
}

// MaxImportBytes is the configured size cap, for the sentence a refusal
// shows. Zero when import is unavailable, where no refusal names it.
func (s *Service) MaxImportBytes() int64 {
	if s.importer == nil {
		return 0
	}
	return s.importer.MaxSize()
}

// StageImport writes an uploaded file to staging and previews it. name is
// the filename the client offered, which seeds the library name and nothing
// else — what the file is comes out of its content.
func (s *Service) StageImport(ctx context.Context, name string, r io.Reader) (*ImportPreview, error) {
	if s.importer == nil {
		return nil, ErrImportDisabled
	}
	staged, err := s.importer.Stage(ctx, name, r)
	if err != nil {
		return nil, err
	}
	preview := importPreviewFrom(staged)
	return &preview, nil
}

// StagedImport returns one staged import, or nil, nil when it has expired
// or was never there — the same absent-isn't-an-error contract GetBook
// uses, so the handler turns it into a 404 the same way.
func (s *Service) StagedImport(ctx context.Context, id string) (*ImportPreview, error) {
	if s.importer == nil {
		return nil, ErrImportDisabled
	}
	staged, ok := s.importer.Get(id)
	if !ok {
		return nil, nil
	}
	preview := importPreviewFrom(staged)
	return &preview, nil
}

// StagedCover returns the cover bytes a staged file had embedded, or nil
// when it had none — what the preview's own image is served from, since a
// staged book has no entry in COVERS_DIR and should not acquire one before
// anybody has said to keep it.
func (s *Service) StagedCover(id string) []byte {
	if s.importer == nil {
		return nil
	}
	cover, ok := s.importer.Cover(id)
	if !ok {
		return nil
	}
	return cover
}

// ConfirmImport copies a staged file into the library and indexes it.
//
// indexed false with a nil error is the one outcome that is neither success
// nor failure: the file is in the library and the index write failed, so
// the bytes are safe and the next sweep picks them up. There is no book to
// redirect to, which is exactly what the caller needs to know.
func (s *Service) ConfirmImport(ctx context.Context, id string) (bookID int64, indexed bool, err error) {
	if s.importer == nil {
		return 0, false, ErrImportDisabled
	}
	bookID, err = s.importer.Confirm(ctx, id)
	switch {
	case errors.Is(err, importer.ErrNotIndexed):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return bookID, true, nil
}

// DiscardImport drops a staged import and its file. An id that is already
// gone is not an error: what was asked for has happened.
func (s *Service) DiscardImport(ctx context.Context, id string) error {
	if s.importer == nil {
		return ErrImportDisabled
	}
	s.importer.Discard(id)
	return nil
}

func importPreviewFrom(staged importer.Staged) ImportPreview {
	return ImportPreview{
		ID:            staged.ID,
		OriginalName:  staged.OriginalName,
		LibraryName:   staged.LibraryName,
		Title:         staged.Title,
		Authors:       staged.Authors,
		Publisher:     staged.Publisher,
		PublishedDate: staged.PublishedDate,
		Language:      staged.Language,
		ISBN:          staged.ISBN,
		Description:   staged.Description,
		Format:        staged.Format,
		Size:          staged.Size,
		HasCover:      staged.HasCover,
		Verdict:       string(staged.Verdict),
		ExistingID:    staged.ExistingID,
		ExistingTitle: staged.ExistingTitle,
	}
}
