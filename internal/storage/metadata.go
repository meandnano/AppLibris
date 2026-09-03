package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrInvalidMetadataField = errors.New("invalid metadata field")

type MetadataField string

const (
	FieldTitle         MetadataField = "title"
	FieldAuthors       MetadataField = "authors"
	FieldPublisher     MetadataField = "publisher"
	FieldPublishedDate MetadataField = "published_date"
	FieldLanguage      MetadataField = "language"
	FieldISBN          MetadataField = "isbn"
	FieldDescription   MetadataField = "description"
)

var metadataFields = map[string]MetadataField{
	string(FieldTitle):         FieldTitle,
	string(FieldAuthors):       FieldAuthors,
	string(FieldPublisher):     FieldPublisher,
	string(FieldPublishedDate): FieldPublishedDate,
	string(FieldLanguage):      FieldLanguage,
	string(FieldISBN):          FieldISBN,
	string(FieldDescription):   FieldDescription,
}

func ParseMetadataField(value string) (MetadataField, bool) {
	field, ok := metadataFields[value]
	return field, ok
}

// leadingArticles are stripped from a title to derive its sort form, longest
// first so "An Ideal Husband" doesn't lose only its "A".
//
// English only. Guessing articles across languages ("Der", "La", "El")
// mis-files any title that legitimately starts with one of those words, and
// the collection is mostly English.
var leadingArticles = []string{"the ", "an ", "a "}

// SortTitle derives the form a title files under: one leading article
// removed and the rest case folded, so "The Hobbit" sorts under H and
// "apple book" sorts among the A's rather than after every capitalised
// title. Punctuation and digits are deliberately left alone — "'Salem's
// Lot" and "1984" file under "'" and "1", which is the "before A" bucket a
// reader scanning alphabetically expects.
//
// It lives here, not in internal/scanner, because two callers now derive
// the same column: the scanner on first sight of a file, and UpdateBookField
// when a title is edited. A second copy of this rule is a library that
// sorts differently depending on how a title arrived.
func SortTitle(title string) string {
	trimmed := strings.TrimSpace(title)

	stripped := trimmed
	for _, article := range leadingArticles {
		// Strictly longer, so a title that *is* an article ("A") keeps it.
		if len(stripped) > len(article) && strings.EqualFold(stripped[:len(article)], article) {
			stripped = strings.TrimSpace(stripped[len(article):])
			break
		}
	}

	// Never return empty: a blank sort_title files the book above the
	// whole library.
	if stripped == "" {
		return strings.ToLower(trimmed)
	}
	return strings.ToLower(stripped)
}

func setFieldSourceTx(ctx context.Context, tx *sql.Tx, bookID int64, field MetadataField, source string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO field_sources (book_id, field, source)
		VALUES (?, ?, ?)
		ON CONFLICT(book_id, field) DO UPDATE SET source = excluded.source`, bookID, field, source)
	return err
}

func setEmbeddedFieldSourcesTx(ctx context.Context, tx *sql.Tx, bookID int64, b Book, authorNames []string) error {
	values := map[MetadataField]string{
		FieldTitle:         b.Title,
		FieldPublisher:     b.Publisher,
		FieldPublishedDate: b.PublishedDate,
		FieldLanguage:      b.Language,
		FieldISBN:          b.ISBN,
		FieldDescription:   b.Description,
	}
	for field, value := range values {
		if value != "" {
			if err := setFieldSourceTx(ctx, tx, bookID, field, "embedded"); err != nil {
				return err
			}
		}
	}
	if len(authorNames) > 0 {
		return setFieldSourceTx(ctx, tx, bookID, FieldAuthors, "embedded")
	}
	return nil
}

func (db *DB) UpdateBookField(ctx context.Context, bookID int64, field MetadataField, value string, modifiedAt time.Time) (exists bool, err error) {
	if field == FieldAuthors {
		return false, ErrInvalidMetadataField
	}
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM books WHERE id = ?)`, bookID).Scan(&exists); err != nil || !exists {
			return err
		}

		var updateErr error
		switch field {
		case FieldTitle:
			_, updateErr = tx.ExecContext(ctx, `UPDATE books SET title = ?, sort_title = ?, modified_at = ? WHERE id = ?`, value, SortTitle(value), formatTime(modifiedAt), bookID)
		case FieldPublisher:
			_, updateErr = tx.ExecContext(ctx, `UPDATE books SET publisher = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
		case FieldPublishedDate:
			_, updateErr = tx.ExecContext(ctx, `UPDATE books SET published_date = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
		case FieldLanguage:
			_, updateErr = tx.ExecContext(ctx, `UPDATE books SET language = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
		case FieldISBN:
			_, updateErr = tx.ExecContext(ctx, `UPDATE books SET isbn = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
		case FieldDescription:
			_, updateErr = tx.ExecContext(ctx, `UPDATE books SET description = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
		default:
			return ErrInvalidMetadataField
		}
		if updateErr != nil {
			return updateErr
		}
		if err := setFieldSourceTx(ctx, tx, bookID, field, "manual"); err != nil {
			return err
		}
		return syncBookFTSTx(ctx, tx, bookID)
	})
	return exists, err
}

func (db *DB) UpdateBookAuthors(ctx context.Context, bookID int64, names []string, modifiedAt time.Time) (exists bool, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM books WHERE id = ?)`, bookID).Scan(&exists); err != nil || !exists {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM book_authors WHERE book_id = ?`, bookID); err != nil {
			return err
		}
		for position, name := range names {
			authorID, err := findOrCreateAuthor(ctx, tx, name)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO book_authors (book_id, author_id, position) VALUES (?, ?, ?)`, bookID, authorID, position); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE books SET modified_at = ? WHERE id = ?`, formatTime(modifiedAt), bookID); err != nil {
			return err
		}
		if err := setFieldSourceTx(ctx, tx, bookID, FieldAuthors, "manual"); err != nil {
			return err
		}
		return syncBookFTSTx(ctx, tx, bookID)
	})
	return exists, err
}
