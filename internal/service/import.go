package service

import (
	"context"
	"errors"
	"io"

	"library/internal/importer"
)

// ErrImportDisabled is what every import method answers when there is no
// Stager — which is what cmd/server builds when the library directory
// cannot be written. One disabled state and one convention, the same
// Notify follows. The routes stay registered so a stale tab gets this
// explanation rather than a 404.
var ErrImportDisabled = errors.New("service: import is not available")

// ImportPreview is one staged import as the import page needs it.
//
// An alias rather than a copy. Every field of importer.Staged is one the
// page renders, and the one thing a separate type would restate — the
// verdict, as a string — the transport casts straight back to compare. A
// per-page copy of sixteen identical fields is a rename of nothing, which
// is the same call BookDetail makes in carrying service.FileLocation as it
// comes. internal/web depends on internal/importer for the error sentinels
// a refusal is matched against anyway, so a copy buys no independence
// either.
type ImportPreview = importer.Staged

// ImportEnabled reports whether this run can import at all, which is what
// decides whether the nav offers the page.
func (s *Service) ImportEnabled() bool {
	return s.importer != nil
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
	return &staged, nil
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
	return &staged, nil
}

// StagedCover returns the cover a staged file had embedded, with its media
// type, or nil when it had none — what the preview's own image is served
// from, since a staged book has no entry in COVERS_DIR and should not
// acquire one before anybody has said to keep it.
//
// The type travels with the bytes because the transport must not decide it
// by sniffing: see importer.Stager.Cover.
func (s *Service) StagedCover(id string) (data []byte, contentType string) {
	if s.importer == nil {
		return nil, ""
	}
	cover, contentType, ok := s.importer.Cover(id)
	if !ok {
		return nil, ""
	}
	return cover, contentType
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
