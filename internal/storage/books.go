package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
	ID           int64
	BookID       int64
	FilePath     string
	FileSize     int64
	ModifiedAt   time.Time
	AddedAt      time.Time
	MissingSince sql.NullTime
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

const bookFileColumns = `id, book_id, file_path, file_size, modified_at, added_at, missing_since`

func scanBookFile(row *sql.Row) (*BookFile, error) {
	var f BookFile
	err := row.Scan(&f.ID, &f.BookID, &f.FilePath, &f.FileSize, &f.ModifiedAt, &f.AddedAt, &f.MissingSince)
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

// FindBookByID returns the book with the given id, or nil if none exists
func (db *DB) FindBookByID(ctx context.Context, id int64) (*Book, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+bookColumns+` FROM books WHERE id = ?`, id)
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

// ListFilesUnder returns every book_files row whose path is nested under
// prefix; prefix "" matches every row. Used to fetch the whole table for
// missing-file reconciliation, which needs each row's MissingSince to
// decide what to mark, clear, or leave alone.
func (db *DB) ListFilesUnder(ctx context.Context, prefix string) ([]BookFile, error) {
	pattern := "%"
	if prefix != "" {
		pattern = prefix + "/%"
	}
	rows, err := db.read.QueryContext(ctx, `SELECT `+bookFileColumns+` FROM book_files WHERE file_path LIKE ?`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []BookFile
	for rows.Next() {
		var f BookFile
		if err := rows.Scan(&f.ID, &f.BookID, &f.FilePath, &f.FileSize, &f.ModifiedAt, &f.AddedAt, &f.MissingSince); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
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
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err = createBookTx(ctx, tx, b, authorNames)
		return err
	})
	return id, err
}

// CreateBookWithFile inserts a new book row, its authors, and its first
// file location in one transaction — a crash or write error between the
// two inserts can no longer leave a book with zero locations. Use this
// instead of CreateBook + UpsertBookFile whenever both are needed together,
// which is every case except a derived book created ahead of its (not yet
// written) file.
//
// path may already belong to a different book (a re-download over the same
// filename, or a library tool rewriting a file in place) — upsertBookFileTx
// reassigns it unconditionally, same as it always has, so this also prunes
// that previous owner if the reassignment leaves it with zero locations.
// Returns the id and title of any book this deleted (both zero if none
// was), same contract as ReassignFileAndPruneOrphan and for the same
// reason: the row is gone by the time this returns, so a caller can't look
// it up afterward.
func (db *DB) CreateBookWithFile(ctx context.Context, b Book, authorNames []string, path string, size int64, mtime time.Time) (id int64, orphanedID int64, orphanedTitle string, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		previousOwner, err := previousFileOwnerTx(ctx, tx, path)
		if err != nil {
			return err
		}

		id, err = createBookTx(ctx, tx, b, authorNames)
		if err != nil {
			return err
		}
		if _, err := upsertBookFileTx(ctx, tx, id, path, size, mtime); err != nil {
			return err
		}

		orphanedID, orphanedTitle, err = pruneOrphanIfEmptyTx(ctx, tx, previousOwner, id)
		return err
	})
	return id, orphanedID, orphanedTitle, err
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
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err = upsertBookFileTx(ctx, tx, bookID, path, size, mtime)
		return err
	})
	return id, err
}

// ReassignFileAndPruneOrphan is upsertBookFileTx run as its own write
// transaction, followed by pruning whatever book previously owned path if
// the reassignment left it orphaned — see pruneOrphanIfEmptyTx.
//
// Returns the id and title of any book this deleted (both zero if none
// was), so the caller can log and count it without a second lookup — the
// row is gone by the time this returns, so that lookup would be too late.
func (db *DB) ReassignFileAndPruneOrphan(ctx context.Context, bookID int64, path string, size int64, mtime time.Time) (fileID int64, orphanedID int64, orphanedTitle string, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		previousOwner, err := previousFileOwnerTx(ctx, tx, path)
		if err != nil {
			return err
		}

		fileID, err = upsertBookFileTx(ctx, tx, bookID, path, size, mtime)
		if err != nil {
			return err
		}

		orphanedID, orphanedTitle, err = pruneOrphanIfEmptyTx(ctx, tx, previousOwner, bookID)
		return err
	})
	return fileID, orphanedID, orphanedTitle, err
}

// previousFileOwnerTx returns the book_id currently at path, if any, for a
// caller about to reassign that path via upsertBookFileTx — read first,
// since the upsert overwrites it. It must be called from inside a DB.Write
// callback — see DB.Write's contract.
func previousFileOwnerTx(ctx context.Context, tx *sql.Tx, path string) (sql.NullInt64, error) {
	var owner sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT book_id FROM book_files WHERE file_path = ?`, path).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, err
	}
	return owner, nil
}

// pruneOrphanedBookTx deletes bookID if it now has zero book_files rows.
// The cascade from book_authors takes its author links with it; the
// authors rows themselves are left alone — an author with no books is not
// itself wrong, and reference-counting authors is a separate concern.
//
// Shared by every caller that can leave a book with no locations: a path
// reassignment (CreateBookWithFile, ReassignFileAndPruneOrphan, via
// pruneOrphanIfEmptyTx below) and missing-file pruning (PruneMissingFiles).
// It must be called from inside a DB.Write callback, after whatever change
// might have emptied the book — see DB.Write's contract.
func pruneOrphanedBookTx(ctx context.Context, tx *sql.Tx, bookID int64) (deleted bool, title string, err error) {
	var stillHasFiles bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM book_files WHERE book_id = ?)`, bookID).Scan(&stillHasFiles)
	if err != nil || stillHasFiles {
		return false, "", err
	}

	if err := tx.QueryRowContext(ctx, `SELECT title FROM books WHERE id = ?`, bookID).Scan(&title); err != nil {
		return false, "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, bookID); err != nil {
		return false, "", err
	}
	return true, title, nil
}

// pruneOrphanIfEmptyTx deletes previousOwner via pruneOrphanedBookTx if it
// is a real, different book (not newOwnerID itself — a path reassigned to
// the book that already owned it, e.g. a plain stat refresh, is not a
// reassignment at all) that upsertBookFileTx has just left with zero
// book_files rows.
//
// The "still has rows" check runs after the upsert, not as a count taken
// before it: a book with two known locations that loses one to a
// reassignment is not an orphan, and a before-check gets that wrong.
//
// It must be called from inside a DB.Write callback, after the upsert —
// see DB.Write's contract.
func pruneOrphanIfEmptyTx(ctx context.Context, tx *sql.Tx, previousOwner sql.NullInt64, newOwnerID int64) (orphanedID int64, orphanedTitle string, err error) {
	if !previousOwner.Valid || previousOwner.Int64 == newOwnerID {
		return 0, "", nil
	}
	deleted, title, err := pruneOrphanedBookTx(ctx, tx, previousOwner.Int64)
	if err != nil || !deleted {
		return 0, "", err
	}
	return previousOwner.Int64, title, nil
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
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return updateBookFileStatTx(ctx, tx, fileID, size, mtime)
	})
}

// UpdateBookCoverPath records the current derived cover location
func (db *DB) UpdateBookCoverPath(ctx context.Context, bookID int64, path string) error {
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE books SET cover_path = ? WHERE id = ?`, path, bookID)
		return err
	})
}

// SetFilesMissing marks each of fileIDs missing as of at — but only a row
// that doesn't already have missing_since set. First-seen-missing wins, so
// a row already marked keeps its original timestamp rather than having its
// grace period restarted by every sweep that still can't find it.
func (db *DB) SetFilesMissing(ctx context.Context, fileIDs []int64, at time.Time) error {
	if len(fileIDs) == 0 {
		return nil
	}
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range fileIDs {
			if _, err := tx.ExecContext(ctx,
				`UPDATE book_files SET missing_since = ? WHERE id = ? AND missing_since IS NULL`,
				formatTime(at), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClearFilesMissing clears missing_since for each of fileIDs — a row seen
// again during a sweep is no longer missing, whatever grace period remained.
func (db *DB) ClearFilesMissing(ctx context.Context, fileIDs []int64) error {
	if len(fileIDs) == 0 {
		return nil
	}
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range fileIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE book_files SET missing_since = NULL WHERE id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneMissingFiles deletes exactly the book_files rows named by fileIDs,
// then deletes any book those deletions left with no locations.
//
// It does no re-verification of its own — no age check, no path exclusion.
// Deciding what's safe to delete belongs entirely to the caller, which is
// the only place with access to live filesystem state: fileIDs must
// already be rows the caller independently confirmed, this exact sweep,
// via a current os.Lstat returning fs.ErrNotExist, and separately past
// their grace period. A row merely aged past grace, or under a directory
// the sweep couldn't re-read, must never appear here — only a
// currently-reconfirmed absence earns deletion.
// pruneMissingFilesChunkSize bounds how many IDs go into a single DELETE ...
// IN (...) statement. SQLite's per-statement bound-parameter limit
// (SQLITE_MAX_VARIABLE_NUMBER) is 999 pre-3.32 and 32766 since — this stays
// well under either so a large overdue batch can never overflow it.
const pruneMissingFilesChunkSize = 500

func (db *DB) PruneMissingFiles(ctx context.Context, fileIDs []int64) (files int, books int, err error) {
	if len(fileIDs) == 0 {
		return 0, 0, nil
	}
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		affectedBooks := map[int64]bool{}

		for start := 0; start < len(fileIDs); start += pruneMissingFilesChunkSize {
			end := min(start+pruneMissingFilesChunkSize, len(fileIDs))
			chunk := fileIDs[start:end]

			placeholders := make([]string, len(chunk))
			args := make([]any, len(chunk))
			for i, id := range chunk {
				placeholders[i] = "?"
				args[i] = id
			}

			rows, qErr := tx.QueryContext(ctx,
				`DELETE FROM book_files WHERE id IN (`+strings.Join(placeholders, ",")+`) RETURNING book_id`,
				args...)
			if qErr != nil {
				return qErr
			}
			for rows.Next() {
				var bookID int64
				if err := rows.Scan(&bookID); err != nil {
					rows.Close()
					return err
				}
				affectedBooks[bookID] = true
				files++
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}

		for bookID := range affectedBooks {
			deleted, _, err := pruneOrphanedBookTx(ctx, tx, bookID)
			if err != nil {
				return err
			}
			if deleted {
				books++
			}
		}
		return nil
	})
	return files, books, err
}
