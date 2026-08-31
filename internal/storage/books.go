package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// sqliteTimeLayout is the one format this package writes timestamps in:
// UTC, fixed-width so text ordering matches chronological ordering, and
// parseable by SQLite's own date functions. The CREATE TABLE defaults use
// strftime to produce the identical shape.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(sqliteTimeLayout) }

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
	Format        string
	AddedAt       time.Time
	ModifiedAt    time.Time
	DerivedFrom   sql.NullInt64
}

// BookFile mirrors the book_files table: one row per physical location a
// book's content is known to live at.
type BookFile struct {
	ID         int64
	BookID     int64
	FilePath   string
	FileSize   int64
	ModifiedAt time.Time
	AddedAt    time.Time
}

const bookColumns = `id, content_hash, title, sort_title, publisher, published_date, language, isbn, description, cover_path, format, added_at, modified_at, derived_from`

func scanBook(row *sql.Row) (*Book, error) {
	var b Book
	err := row.Scan(&b.ID, &b.ContentHash, &b.Title, &b.SortTitle, &b.Publisher, &b.PublishedDate,
		&b.Language, &b.ISBN, &b.Description, &b.CoverPath, &b.Format,
		&b.AddedAt, &b.ModifiedAt, &b.DerivedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

const bookFileColumns = `id, book_id, file_path, file_size, modified_at, added_at`

func scanBookFile(row *sql.Row) (*BookFile, error) {
	var f BookFile
	err := row.Scan(&f.ID, &f.BookID, &f.FilePath, &f.FileSize, &f.ModifiedAt, &f.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// FindBookByContentHash returns the book with the given content_hash, or nil if none exists.
func (db *DB) FindBookByContentHash(ctx context.Context, hash string) (*Book, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+bookColumns+` FROM books WHERE content_hash = ?`, hash)
	return scanBook(row)
}

// ListBooks returns every book, ordered by sort_title.
func (db *DB) ListBooks(ctx context.Context) ([]Book, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT `+bookColumns+` FROM books ORDER BY sort_title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.ContentHash, &b.Title, &b.SortTitle, &b.Publisher, &b.PublishedDate,
			&b.Language, &b.ISBN, &b.Description, &b.CoverPath, &b.Format,
			&b.AddedAt, &b.ModifiedAt, &b.DerivedFrom); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

// ListBookAuthors returns every book's author names, keyed by book id.
func (db *DB) ListBookAuthors(ctx context.Context) (map[int64][]string, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT book_authors.book_id, authors.name
		FROM book_authors
		JOIN authors ON authors.id = book_authors.author_id
		ORDER BY book_authors.book_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	authorsByBook := make(map[int64][]string)
	for rows.Next() {
		var bookID int64
		var name string
		if err := rows.Scan(&bookID, &name); err != nil {
			return nil, err
		}
		authorsByBook[bookID] = append(authorsByBook[bookID], name)
	}
	return authorsByBook, rows.Err()
}

// FindFileByPath returns the book_files row at the given path, or nil if none exists.
func (db *DB) FindFileByPath(ctx context.Context, path string) (*BookFile, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+bookFileColumns+` FROM book_files WHERE file_path = ?`, path)
	return scanBookFile(row)
}

// createBookTx inserts a book and its authors, find-or-creating each author
// by name and linking it via book_authors. It must be called from inside a
// DB.Write callback — see DB.Write's contract.
func createBookTx(ctx context.Context, tx *sql.Tx, b Book, authorNames []string) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO books (content_hash, title, sort_title, publisher, published_date, language, isbn, description, cover_path, format)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ContentHash, b.Title, b.SortTitle, b.Publisher, b.PublishedDate, b.Language,
		b.ISBN, b.Description, b.CoverPath, b.Format)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, name := range authorNames {
		authorID, err := findOrCreateAuthor(ctx, tx, name)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO book_authors (book_id, author_id) VALUES (?, ?)`, id, authorID); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// CreateBook inserts a new book row along with its authors, find-or-creating
// each author by name and linking it via book_authors. Runs as one write
// transaction. Callers attach the book's first file location separately via
// UpsertBookFile.
func (db *DB) CreateBook(ctx context.Context, b Book, authorNames []string) (id int64, err error) {
	err = db.Write(ctx, func(tx *sql.Tx) error {
		id, err = createBookTx(ctx, tx, b, authorNames)
		return err
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

// upsertBookFileTx records that bookID's content is known to live at path,
// with the given size/mtime. If path already has a book_files row (because
// it used to point at different content, or simply needs a stat refresh),
// that row is updated in place rather than erroring on the unique file_path
// constraint — this single operation covers a brand-new location, a
// moved/renamed file, an additional location for already-known duplicate
// content, and a path whose content changed to match different already-known
// content. It must be called from inside a DB.Write callback — see
// DB.Write's contract.
func upsertBookFileTx(ctx context.Context, tx *sql.Tx, bookID int64, path string, size int64, mtime time.Time) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO book_files (book_id, file_path, file_size, modified_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			book_id = excluded.book_id,
			file_size = excluded.file_size,
			modified_at = excluded.modified_at
		RETURNING id`,
		bookID, path, size, formatTime(mtime)).Scan(&id)
	return id, err
}

// UpsertBookFile is upsertBookFileTx run as its own write transaction. See
// upsertBookFileTx for what it does.
func (db *DB) UpsertBookFile(ctx context.Context, bookID int64, path string, size int64, mtime time.Time) (id int64, err error) {
	err = db.Write(ctx, func(tx *sql.Tx) error {
		id, err = upsertBookFileTx(ctx, tx, bookID, path, size, mtime)
		return err
	})
	return id, err
}

// updateBookFileStatTx refreshes the cheap-check fields (size, mtime) for a
// book_files row whose content hash is unchanged but whose file was
// touched. It must be called from inside a DB.Write callback — see
// DB.Write's contract.
func updateBookFileStatTx(ctx context.Context, tx *sql.Tx, fileID int64, size int64, mtime time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE book_files SET file_size = ?, modified_at = ? WHERE id = ?`, size, formatTime(mtime), fileID)
	return err
}

// UpdateBookFileStat is updateBookFileStatTx run as its own write
// transaction. See updateBookFileStatTx for what it does.
func (db *DB) UpdateBookFileStat(ctx context.Context, fileID int64, size int64, mtime time.Time) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		return updateBookFileStatTx(ctx, tx, fileID, size, mtime)
	})
}
