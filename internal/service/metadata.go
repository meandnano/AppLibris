package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"library/internal/storage"
)

var ErrInvalidMetadata = errors.New("invalid metadata")

type MetadataUpdate struct {
	Field string
	Value string
}

type metadataValidationError struct {
	message string
}

func (e metadataValidationError) Error() string {
	return e.message
}

func (e metadataValidationError) Unwrap() error {
	return ErrInvalidMetadata
}

func MetadataValidationMessage(err error) string {
	var validation metadataValidationError
	if errors.As(err, &validation) {
		return validation.message
	}
	return "Invalid value"
}

func (s *Service) UpdateBookMetadata(ctx context.Context, bookID int64, update MetadataUpdate) (*BookDetail, error) {
	field, ok := storage.ParseMetadataField(update.Field)
	if !ok {
		return nil, metadataValidationError{message: "Unknown metadata field"}
	}

	var exists bool
	var err error
	if field == storage.FieldAuthors {
		var names []string
		names, err = normalizeAuthors(update.Value)
		if err == nil {
			exists, err = s.db.UpdateBookAuthors(ctx, bookID, names, time.Now())
		}
	} else {
		var value string
		value, err = normalizeField(field, update.Value)
		if err == nil {
			exists, err = s.db.UpdateBookField(ctx, bookID, field, value, time.Now())
		}
	}
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return s.GetBook(ctx, bookID)
}

// Field length limits, in bytes of UTF-8 rather than runes: they exist to
// bound what reaches the database and the request body, and bytes are the
// unit both of those are measured in. MaxDescriptionBytes is exported
// because internal/web sizes its request-body cap from it — the encoded
// body is a multiple of the decoded value, so the two limits have to be
// derived from one number to stay consistent.
const (
	MaxDescriptionBytes = 64 * 1024
	maxTitleBytes       = 1024
	maxAuthorNameBytes  = 1024
	maxScalarBytes      = 4096
	maxAuthors          = 100
)

func normalizeField(field storage.MetadataField, value string) (string, error) {
	value = strings.TrimSpace(value)
	if field == storage.FieldTitle {
		// The one required field: a book with no title is unfindable in a
		// list sorted by title, and sort_title is derived from it.
		if value == "" {
			return "", metadataValidationError{message: "Title is required"}
		}
		if len(value) > maxTitleBytes {
			return "", metadataValidationError{message: "Title is too long"}
		}
		return value, nil
	}
	limit := maxScalarBytes
	if field == storage.FieldDescription {
		limit = MaxDescriptionBytes
	}
	if len(value) > limit {
		return "", metadataValidationError{message: fmt.Sprintf("Value is too long (maximum %d bytes)", limit)}
	}
	return value, nil
}

func normalizeAuthors(value string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	names := make([]string, 0, len(lines))
	seen := make(map[string]bool)
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		if len(name) > maxAuthorNameBytes {
			return nil, metadataValidationError{message: "An author name is too long"}
		}
		seen[name] = true
		names = append(names, name)
		if len(names) > maxAuthors {
			return nil, metadataValidationError{message: fmt.Sprintf("Too many authors (maximum %d)", maxAuthors)}
		}
	}
	return names, nil
}
