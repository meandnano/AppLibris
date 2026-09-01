package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Book mirrors the books table.
type Book struct {
	ID            int64
	ContentHash   string
	Title         string
	SortTitle     string
	Publisher     string
	PublishedDate string
	Language      string
	ISBN          string
	Description   string
	CoverPath     string
	FilePath      string
	Format        string
	FileSize      int64
	AddedAt       time.Time
	ModifiedAt    time.Time
	DerivedFrom   sql.NullInt64
}

const bookColumns = `id, content_hash, title, sort_title, publisher, published_date, language, isbn, description, cover_path, file_path, format, file_size, added_at, modified_at, derived_from`

func scanBook(row *sql.Row) (*Book, error) {
	var b Book
	err := row.Scan(&b.ID, &b.ContentHash, &b.Title, &b.SortTitle, &b.Publisher, &b.PublishedDate,
		&b.Language, &b.ISBN, &b.Description, &b.CoverPath, &b.FilePath, &b.Format, &b.FileSize,
		&b.AddedAt, &b.ModifiedAt, &b.DerivedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// FindBookByPath returns the book at the given file_path, or nil if none exists.
func (db *DB) FindBookByPath(ctx context.Context, path string) (*Book, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+bookColumns+` FROM books WHERE file_path = ?`, path)
	return scanBook(row)
}

// FindBookByContentHash returns the book with the given content_hash, or nil if none exists.
func (db *DB) FindBookByContentHash(ctx context.Context, hash string) (*Book, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+bookColumns+` FROM books WHERE content_hash = ?`, hash)
	return scanBook(row)
}

// CreateBook inserts a new book row along with its authors, find-or-creating
// each author by name and linking it via book_authors. Runs as one write
// transaction.
func (db *DB) CreateBook(ctx context.Context, b Book, authorNames []string) (int64, error) {
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO books (content_hash, title, sort_title, publisher, published_date, language, isbn, description, cover_path, file_path, format, file_size, modified_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.ContentHash, b.Title, b.SortTitle, b.Publisher, b.PublishedDate, b.Language,
			b.ISBN, b.Description, b.CoverPath, b.FilePath, b.Format, b.FileSize, b.ModifiedAt)
		if err != nil {
			return err
		}
		bookID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		id = bookID

		for _, name := range authorNames {
			authorID, err := findOrCreateAuthor(ctx, tx, name)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO book_authors (book_id, author_id) VALUES (?, ?)`, id, authorID); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

func findOrCreateAuthor(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM authors WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO authors (name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateBookFileLocation records that a book's content was found at a new
// path (e.g. a move or rename). Metadata is left untouched.
func (db *DB) UpdateBookFileLocation(ctx context.Context, id int64, path string, size int64, mtime time.Time) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET file_path = ?, file_size = ?, modified_at = ? WHERE id = ?`, path, size, mtime, id)
		return err
	})
}

// UpdateBookFileStat refreshes the cheap-check fields (size, mtime) for a
// book whose content hash is unchanged but whose file was touched.
func (db *DB) UpdateBookFileStat(ctx context.Context, id int64, size int64, mtime time.Time) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET file_size = ?, modified_at = ? WHERE id = ?`, size, mtime, id)
		return err
	})
}
