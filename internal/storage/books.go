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
	CoverRetry    bool
	Format        string
	AddedAt       time.Time
	ModifiedAt    time.Time
	DerivedFrom   sql.NullInt64
}

// BookFile carries one physical location plus the owning book fields the scanner needs on its cheap path
type BookFile struct {
	ID              int64
	BookID          int64
	FilePath        string
	FileSize        int64
	ModifiedAt      time.Time
	AddedAt         time.Time
	MissingSince    sql.NullTime
	BookContentHash string
	BookCoverPath   string
	BookCoverRetry  bool
}

const bookColumns = `id, content_hash, title, sort_title, publisher, published_date, language, isbn, description, cover_path, cover_retry, format, added_at, modified_at, derived_from`

// qualifiedBookColumns is bookColumns with every column prefixed
// books. — needed wherever a query joins books against another table that
// has columns of the same name (books_fts, in SearchBooks, has its own
// title and isbn), so an unqualified list would be ambiguous.
const qualifiedBookColumns = `books.id, books.content_hash, books.title, books.sort_title, books.publisher, books.published_date, books.language, books.isbn, books.description, books.cover_path, books.cover_retry, books.format, books.added_at, books.modified_at, books.derived_from`

// scanBooks reads every row of a bookColumns/qualifiedBookColumns result set
// into a slice, closing rows itself. Shared by ListBooks and SearchBooks so
// the two don't carry two copies of the same Scan call list.
func scanBooks(rows *sql.Rows) ([]Book, error) {
	defer rows.Close()
	var books []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.ContentHash, &b.Title, &b.SortTitle, &b.Publisher, &b.PublishedDate,
			&b.Language, &b.ISBN, &b.Description, &b.CoverPath, &b.CoverRetry, &b.Format,
			&b.AddedAt, &b.ModifiedAt, &b.DerivedFrom); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func scanBook(row *sql.Row) (*Book, error) {
	var b Book
	err := row.Scan(&b.ID, &b.ContentHash, &b.Title, &b.SortTitle, &b.Publisher, &b.PublishedDate,
		&b.Language, &b.ISBN, &b.Description, &b.CoverPath, &b.CoverRetry, &b.Format,
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
const qualifiedBookFileColumns = `book_files.id, book_files.book_id, book_files.file_path, book_files.file_size, book_files.modified_at, book_files.added_at, book_files.missing_since`

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

// FindBookByID returns the book with the given id, or nil if none exists —
// the book detail page's lookup, and an unknown id turning up nil rather
// than an error is what lets the handler turn it into a plain 404.
func (db *DB) FindBookByID(ctx context.Context, id int64) (*Book, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+bookColumns+` FROM books WHERE id = ?`, id)
	return scanBook(row)
}

// BookPage is a keyset cursor into the (sort_title, id) ordering both
// ListBooks and SearchBooks return rows in. The zero value is the first
// page.
//
// Keyset rather than LIMIT/OFFSET, for a reason specific to this
// application: the library changes underneath the reader. The scanner runs
// every fifteen minutes and on every filesystem event, and it inserts
// wherever a book's sort_title falls. With OFFSET, a book inserted above
// the reader's position shifts every later row down by one, so the next
// page repeats a card — or, on a delete, silently skips one. A cursor
// naming the last row seen has no such window.
//
// AfterID is part of the cursor because sort_title is emphatically **not**
// unique: it is a normalised title with the leading article stripped and
// the case folded, so two editions of one book collide by construction.
// A cursor on a non-unique column either loops on the collision or skips
// past it.
//
// AfterTitle is compared with the same NOCASE collation the column
// carries. That is the single most likely bug here — a case-sensitive
// comparison disagrees with the ORDER BY, and pages then overlap or drop
// rows between them — and it is inherited from the column's declaration
// rather than spelled out in the query, which is not merely a style
// choice: writing `(sort_title COLLATE NOCASE, id) > (?, ?)` turns the
// plan from `SEARCH ... (sort_title>?)` back into a full `SCAN` of the
// index, because an explicitly collated expression is no longer the
// indexed one. So the collation is guaranteed by the schema plus
// TestPagingRespectsNOCASECollation, not by being visible here.
//
// A Limit of zero means unbounded, which is what keeps the scanner's and
// the tests' whole-library calls working unchanged. The web transport
// never passes zero; that path exists for callers that genuinely want
// every row.
type BookPage struct {
	AfterTitle string
	AfterID    int64
	Limit      int
}

// paginate extends query with the cursor comparison, the ordering and the
// limit for p, returning the extended query and the arguments to append.
// joiner is the keyword the comparison needs — "WHERE" for a query that
// has no clause yet, "AND" for one that does.
//
// All three are emitted here together rather than assembled separately by
// each caller, because the comparison and the ORDER BY have to agree about
// both columns and their collation, and a reader should be able to see
// that they do without holding two call sites in their head.
//
// The comparison is SQLite's row-value form. It is not merely tidier than
// the equivalent `a > ? OR (a = ? AND b > ?)`: the expanded form plans as
// a full `SCAN` of books_sort_title_id, while this one plans as
// `SEARCH ... (sort_title>?)` and actually seeks — which is the whole
// reason that index exists. (On the FTS join the index cannot be used
// either way: the MATCH drives the query and the ordering falls back to a
// temp B-tree. Bounded by the match set, and not worth contorting.)
func (p BookPage) paginate(query, joiner, titleColumn, idColumn string) (string, []any) {
	var args []any
	if p.AfterTitle != "" || p.AfterID != 0 {
		query += " " + joiner + " (" + titleColumn + ", " + idColumn + ") > (?, ?)"
		args = append(args, p.AfterTitle, p.AfterID)
	}
	query += " ORDER BY " + titleColumn + ", " + idColumn
	if p.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, p.Limit)
	}
	return query, args
}

// ListBooks returns one page of books, ordered by (sort_title, id). A zero
// BookPage returns every book, which is what the scanner and the tests
// want.
func (db *DB) ListBooks(ctx context.Context, page BookPage) ([]Book, error) {
	query, args := page.paginate(`SELECT `+bookColumns+` FROM books`, "WHERE", "sort_title", "id")

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanBooks(rows)
}

// CountBooks returns the total number of books, independent of any search
// filter — the library-wide count a masthead shows, as distinct from how
// many a search matched.
func (db *DB) CountBooks(ctx context.Context) (int, error) {
	var n int
	err := db.read.QueryRowContext(ctx, `SELECT count(*) FROM books`).Scan(&n)
	return n, err
}

// SearchBooks returns every book whose books_fts row matches query, ordered
// by sort_title rather than relevance — the search UI filters the grid in
// place rather than opening a results page (docs/notes/storage.md), and reordering a grid
// the user is actively scanning while they type would be jarring. query
// must already be a valid FTS5 MATCH expression; SanitizeFTSQuery is what
// produces one from raw user input.
// It pages through the same BookPage cursor ListBooks uses, and can
// because this ordering is sort_title rather than relevance: one cursor
// serves both paths, and a search matching nine hundred books is not the
// case that got forgotten.
func (db *DB) SearchBooks(ctx context.Context, query string, page BookPage) ([]Book, error) {
	sql, pageArgs := page.paginate(`
		SELECT `+qualifiedBookColumns+`
		FROM books
		JOIN books_fts ON books_fts.rowid = books.id
		WHERE books_fts MATCH ?`, "AND", "books.sort_title", "books.id")
	args := append([]any{query}, pageArgs...)

	rows, err := db.read.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return scanBooks(rows)
}

// CountSearchBooks returns how many books match query, independent of any
// page — the "4" in the results line's "4 of 1,284".
//
// It exists because paging took that number away from the transport: an
// unfiltered grid used to hold every match and could count them in hand,
// and a bounded page cannot. The alternative was a results line reading
// "48 of 1,284" for a search that matched nine hundred books, which is a
// claim the page cannot support. query must already be a valid FTS5 MATCH
// expression, as SearchBooks requires.
func (db *DB) CountSearchBooks(ctx context.Context, query string) (int, error) {
	var n int
	err := db.read.QueryRowContext(ctx, `
		SELECT count(*)
		FROM books
		JOIN books_fts ON books_fts.rowid = books.id
		WHERE books_fts MATCH ?`, query).Scan(&n)
	return n, err
}

// ftsColumns are the books_fts columns, in the order MatchedSearchFields
// reports them — the order the results line names them in, which is the
// order a reader scans for ("title" before "isbn"), not the schema's.
var ftsColumns = []string{"title", "authors", "description", "isbn"}

// MatchedSearchFields reports which books_fts columns produced a match for
// query, across the result set as a whole rather than per book — the
// "matched title, author" half of the results line, which exists so a hit
// on a description or an ISBN doesn't look like a mystery.
//
// One round trip: four EXISTS over the same index the search itself used,
// each scoped to a single column with FTS5's `{col} : (expr)` filter. The
// parentheses matter — without them the filter binds to the first term
// only, so a two-word query would report the wrong columns. query must
// already be a valid MATCH expression, as for SearchBooks.
func (db *DB) MatchedSearchFields(ctx context.Context, query string) ([]string, error) {
	args := make([]any, len(ftsColumns))
	for i, col := range ftsColumns {
		args[i] = "{" + col + "} : (" + query + ")"
	}

	var title, authors, description, isbn bool
	if err := db.read.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM books_fts WHERE books_fts MATCH ?),
		       EXISTS(SELECT 1 FROM books_fts WHERE books_fts MATCH ?),
		       EXISTS(SELECT 1 FROM books_fts WHERE books_fts MATCH ?),
		       EXISTS(SELECT 1 FROM books_fts WHERE books_fts MATCH ?)`,
		args...).Scan(&title, &authors, &description, &isbn); err != nil {
		return nil, err
	}

	var matched []string
	for i, hit := range []bool{title, authors, description, isbn} {
		if hit {
			matched = append(matched, ftsColumns[i])
		}
	}
	return matched, nil
}

// ListBookAuthors returns every book's author names, keyed by book id, in
// each book's own credited order — author_id order (first-sight-in-the-
// library order) is not the same thing once an author is shared between
// books, so position is what this orders by.
func (db *DB) ListBookAuthors(ctx context.Context) (map[int64][]string, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT book_authors.book_id, authors.name
		FROM book_authors
		JOIN authors ON authors.id = book_authors.author_id
		ORDER BY book_authors.book_id, book_authors.position`)
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

// ListAuthorsForBook returns one book's author names, in the order its
// source file credited them — unlike ListBookAuthors, which loads the
// whole library's author map (right for the grid page, wrong for a
// single-book page). Returns an empty, non-nil slice for an authorless
// book rather than an error.
func (db *DB) ListAuthorsForBook(ctx context.Context, bookID int64) ([]string, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT authors.name
		FROM book_authors
		JOIN authors ON authors.id = book_authors.author_id
		WHERE book_authors.book_id = ?
		ORDER BY book_authors.position`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// CountFilesByBook returns how many file locations each book has, keyed by
// book id — a plain GROUP BY, so a book with zero rows in book_files is
// simply absent from the result rather than present with an explicit zero;
// looking up an absent id in the returned map yields Go's int zero value,
// which reads the same as an explicit zero to any caller. Zero rows should
// be exceptional in practice, not routine: the last location's deletion
// prunes the book in the same transaction, so book_files should never
// actually hold a book_id with none. That invariant lives in the scanner's
// orphan-pruning, though, not here — this reports book_files as it stands.
// A caller only interested in the multi-location threshold can treat an
// absent entry and an explicit 1 alike, since neither is worth flagging.
//
// Missing locations are counted. A row whose path has vanished stays in
// book_files until it has been missing past MISSING_GRACE, and the detail
// page lists it — annotated — for that whole window. Filtering them out
// here would make the grid and the detail page disagree about the same
// book's location count while linking to each other.
func (db *DB) CountFilesByBook(ctx context.Context) (map[int64]int, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT book_id, count(*) FROM book_files GROUP BY book_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[int64]int)
	for rows.Next() {
		var bookID int64
		var count int
		if err := rows.Scan(&bookID, &count); err != nil {
			return nil, err
		}
		counts[bookID] = count
	}
	return counts, rows.Err()
}

// FindFileByPath returns the location and its owning book's cover fields, or nil if none exists
func (db *DB) FindFileByPath(ctx context.Context, path string) (*BookFile, error) {
	row := db.read.QueryRowContext(ctx, `
		SELECT `+qualifiedBookFileColumns+`, books.content_hash, books.cover_path, books.cover_retry
		FROM book_files
		JOIN books ON books.id = book_files.book_id
		WHERE book_files.file_path = ?`, path)
	var f BookFile
	err := row.Scan(&f.ID, &f.BookID, &f.FilePath, &f.FileSize, &f.ModifiedAt, &f.AddedAt, &f.MissingSince,
		&f.BookContentHash, &f.BookCoverPath, &f.BookCoverRetry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
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

// ListBookFiles returns one book's file locations, ordered by path for
// stable display — a targeted query rather than a filter over
// ListFilesUnder(""), which loads every location in the library to serve
// one book's row.
func (db *DB) ListBookFiles(ctx context.Context, bookID int64) ([]BookFile, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+bookFileColumns+` FROM book_files WHERE book_id = ? ORDER BY file_path`, bookID)
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
// by name and linking it via book_authors at its position in authorNames —
// the order the source file credited them in, which is what ListBookAuthors
// later returns them in. A name repeated in authorNames links once, at its
// first position: book_authors' primary key is (book_id, author_id), so a
// second link for the same author would fail the whole transaction, and a
// book cannot sensibly hold one author at two positions anyway. It must be
// called from inside a DB.Write callback — see DB.Write's contract.
func createBookTx(ctx context.Context, tx *sql.Tx, b Book, authorNames []string) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO books (content_hash, title, sort_title, publisher, published_date, language, isbn, description, cover_path, cover_retry, format)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ContentHash, b.Title, b.SortTitle, b.Publisher, b.PublishedDate, b.Language,
		b.ISBN, b.Description, b.CoverPath, b.CoverRetry, b.Format)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	seen := make(map[string]bool, len(authorNames))
	position := 0
	for _, name := range authorNames {
		if seen[name] {
			continue // a name credited twice is one author, at its first position
		}
		seen[name] = true

		authorID, err := findOrCreateAuthor(ctx, tx, name)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO book_authors (book_id, author_id, position) VALUES (?, ?, ?)`,
			id, authorID, position); err != nil {
			return 0, err
		}
		position++
	}
	if err := setEmbeddedFieldSourcesTx(ctx, tx, id, b, authorNames); err != nil {
		return 0, err
	}
	return id, nil
}

// syncBookFTSTx recomputes bookID's books_fts row from the books/
// book_authors/authors tables as they stand right now, replacing whatever
// was there. Recompute-from-scratch rather than incremental update means
// there is no separate "what changed" bookkeeping to keep in sync with
// reality — the same call serves book creation and every future metadata
// edit. Must run after both the book row and its author links exist, since
// the FTS row needs to see the authors; it must be called from inside a
// DB.Write callback — see DB.Write's contract.
//
// The indexed isbn is books.isbn with hyphens and spaces stripped, not the
// column's own value verbatim: internal/epub normalizes a stored ISBN to
// bare digits, but internal/fb2 does not, so books.isbn holds either shape
// depending on which parser found it. Indexing the normalized form (and
// having SanitizeFTSQuery normalize an ISBN-shaped query the same way)
// means a search matches a book regardless of which format its ISBN
// happened to arrive in. books.isbn itself — the display value — is
// untouched.
func syncBookFTSTx(ctx context.Context, tx *sql.Tx, bookID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, bookID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO books_fts (rowid, title, authors, description, isbn)
		SELECT b.id, b.title, coalesce(group_concat(a.name, ' '), ''), b.description,
			replace(replace(b.isbn, '-', ''), ' ', '')
		FROM books b
		LEFT JOIN book_authors ba ON ba.book_id = b.id
		LEFT JOIN authors a ON a.id = ba.author_id
		WHERE b.id = ?
		GROUP BY b.id`, bookID)
	return err
}

// CreateBook inserts a new book row along with its authors, find-or-creating
// each author by name and linking it via book_authors. Runs as one write
// transaction. Callers attach the book's first file location separately via
// UpsertBookFile.
func (db *DB) CreateBook(ctx context.Context, b Book, authorNames []string) (id int64, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err = createBookTx(ctx, tx, b, authorNames)
		if err != nil {
			return err
		}
		return syncBookFTSTx(ctx, tx, id)
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
// it up afterward. Before that deletion, whatever the previous owner
// carried that a person put there moves onto the new book — see
// inheritFromReplacedBookTx — and inherited names the fields that did.
//
// The FTS row is synced last, once inheritance has settled the title,
// description, ISBN and authors it indexes.
func (db *DB) CreateBookWithFile(ctx context.Context, b Book, authorNames []string, path string, size int64, mtime time.Time) (id int64, orphanedID int64, orphanedTitle string, inherited []MetadataField, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Reset on every attempt, for the reason ApplyEnrichedFields resets
		// its own: appending to a slice from an outer scope would otherwise
		// report a field twice.
		inherited = nil

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

		inherited, err = inheritFromReplacedBookTx(ctx, tx, previousOwner, id)
		if err != nil {
			return err
		}

		orphanedID, orphanedTitle, err = pruneOrphanIfEmptyTx(ctx, tx, previousOwner, id)
		if err != nil {
			return err
		}
		return syncBookFTSTx(ctx, tx, id)
	})
	return id, orphanedID, orphanedTitle, inherited, err
}

// inheritFromReplacedBookTx carries what a person put into the book that
// previously owned a path onto the book that now does. It runs only when
// previousOwner is a real, different book the caller's upsert has just left
// with no locations at all — the same test pruneOrphanIfEmptyTx makes, and
// it must run before that deletion.
//
// That state is one path's whole view of two different events: the same
// book rewritten (Calibre's metadata write-back, ebook-polish, kepubify, a
// sync tool re-downloading a re-zipped file) and a different book dropped at
// the same filename. Nothing distinguishes them, and the obvious guard —
// inherit only where the file's embedded title still matches — is wrong in
// both directions, since write-back is precisely the case where the title in
// the file has just changed to match the edit. The population reaching here
// is overwhelmingly the same book, and an inherited value is visible on the
// detail page with an edit affordance beside it, where a lost one is gone.
//
// Only what a person is the author of moves. A provider-sourced value is
// recreated by a Fetch from the same catalogues, and a provider's guess
// about the old file is not a fact about the new one; enrichment_jobs
// cascade, since a pending intention about the old content is meaningless
// for the new. An empty manual value is inherited like any other — a cleared
// field stays manual, and someone who cleared a wrong publisher does not
// want the file's wrong publisher back.
//
// The stamp is the new book's own modified_at, read back rather than taken
// from a clock: createBookTx leaves that column to its schema default, so
// this is the instant the row was created and the whole creation carries
// one.
//
// It must be called from inside a DB.Write callback — see DB.Write's
// contract.
func inheritFromReplacedBookTx(ctx context.Context, tx *sql.Tx, previousOwner sql.NullInt64, newOwnerID int64) ([]MetadataField, error) {
	if !previousOwner.Valid || previousOwner.Int64 == newOwnerID {
		return nil, nil
	}
	oldID := previousOwner.Int64

	var stillHasFiles bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM book_files WHERE book_id = ?)`, oldID).Scan(&stillHasFiles); err != nil {
		return nil, err
	}
	if stillHasFiles {
		return nil, nil
	}

	if err := repointSendLogTx(ctx, tx, oldID, newOwnerID); err != nil {
		return nil, err
	}

	fields, err := manualFieldSourcesTx(ctx, tx, oldID)
	if err != nil || len(fields) == 0 {
		return nil, err
	}
	modifiedAt, err := bookModifiedAtTx(ctx, tx, newOwnerID)
	if err != nil {
		return nil, err
	}

	inherited := make([]MetadataField, 0, len(fields))
	for _, field := range fields {
		if field == FieldAuthors {
			names, err := bookAuthorNamesTx(ctx, tx, oldID)
			if err != nil {
				return nil, err
			}
			if err := updateBookAuthorsTx(ctx, tx, newOwnerID, names, "manual", modifiedAt); err != nil {
				return nil, err
			}
			inherited = append(inherited, field)
			continue
		}
		value, err := bookScalarFieldTx(ctx, tx, oldID, field)
		if err != nil {
			return nil, err
		}
		if err := updateBookColumnTx(ctx, tx, newOwnerID, field, value, modifiedAt); err != nil {
			return nil, err
		}
		if err := setFieldSourceTx(ctx, tx, newOwnerID, field, "manual"); err != nil {
			return nil, err
		}
		inherited = append(inherited, field)
	}
	return inherited, nil
}

// manualFieldSourcesTx lists the fields bookID records as hand-edited, in
// metadataFieldOrder rather than the query's order so the same book reads
// the same way twice. cover is excluded here rather than at the call site,
// since no caller has a use for a cover row claiming to be manual: the field
// is unreachable from UpdateBookField, and its value is a path keyed to one
// book's content hash. It must be called from inside a DB.Write callback —
// see DB.Write's contract.
func manualFieldSourcesTx(ctx context.Context, tx *sql.Tx, bookID int64) ([]MetadataField, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT field FROM field_sources WHERE book_id = ? AND source = 'manual'`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	manual := make(map[MetadataField]bool)
	for rows.Next() {
		var field MetadataField
		if err := rows.Scan(&field); err != nil {
			return nil, err
		}
		manual[field] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var fields []MetadataField
	for _, field := range metadataFieldOrder {
		if field != FieldCover && manual[field] {
			fields = append(fields, field)
		}
	}
	return fields, nil
}

// bookScalarFieldTx reads the single books column field backs. It must be
// called from inside a DB.Write callback — see DB.Write's contract.
func bookScalarFieldTx(ctx context.Context, tx *sql.Tx, bookID int64, field MetadataField) (string, error) {
	column, ok := scalarColumnName(field)
	if !ok {
		return "", ErrInvalidMetadataField
	}
	var value string
	err := tx.QueryRowContext(ctx, `SELECT `+column+` FROM books WHERE id = ?`, bookID).Scan(&value)
	return value, err
}

// bookAuthorNamesTx reads bookID's author names in the order the book
// credits them, the form updateBookAuthorsTx takes. It must be called from
// inside a DB.Write callback — see DB.Write's contract.
func bookAuthorNamesTx(ctx context.Context, tx *sql.Tx, bookID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.name
		FROM book_authors ba
		JOIN authors a ON a.id = ba.author_id
		WHERE ba.book_id = ?
		ORDER BY ba.position`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// bookModifiedAtTx reads bookID's current modified_at. It must be called
// from inside a DB.Write callback — see DB.Write's contract.
func bookModifiedAtTx(ctx context.Context, tx *sql.Tx, bookID int64) (time.Time, error) {
	var at time.Time
	err := tx.QueryRowContext(ctx, `SELECT modified_at FROM books WHERE id = ?`, bookID).Scan(&at)
	return at, err
}

// repointSendLogTx moves every send_log row from one book to another.
// send_log.book_id is the schema's one ON DELETE SET NULL key, with
// book_title denormalised beside it, which is the right fallback for a book
// that is genuinely gone; a book replaced in place is still on the shelf, so
// its history follows it and both the detail page's status box and the "did
// I already send this?" answer survive the rewrite. book_title is left as it
// stands: it records what was sent, not what the book is called now. It must
// be called from inside a DB.Write callback — see DB.Write's contract.
func repointSendLogTx(ctx context.Context, tx *sql.Tx, from, to int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE send_log SET book_id = ? WHERE book_id = ?`, to, from)
	return err
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

// UpdateBookCoverPath records the current derived cover location, clears any
// retry marker, and drops the cover row from field_sources.
//
// The delete is not incidental: this method is the scanner's, and a cover
// the scanner extracted from the book's own file has no provenance by
// definition — that absence is the whole discriminator internal/scanner
// reads to tell a scanner cover from a provider's. Without it, a book whose
// provider cover was re-extracted from its embedded original keeps a row
// claiming a provider supplied the scanner's image, which is the state
// Decision 1's table forbids and which the sweep would later act on.
//
// All three in one transaction, since a path written without the row
// removed is exactly the false state this prevents.
func (db *DB) UpdateBookCoverPath(ctx context.Context, bookID int64, path string) error {
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE books SET cover_path = ?, cover_retry = 0 WHERE id = ?`, path, bookID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM field_sources WHERE book_id = ? AND field = ?`, bookID, FieldCover)
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

// ForgetMissingFile deletes one of bookID's locations, and the book with it
// if that was the last one. It is a person's answer to a row the scanner
// will not prune on its own: a row whose top-level directory yielded no
// files is refused forever, because that is also what an offline sub-mount
// looks like, and only someone who can see the shelf knows which of the two
// it is.
//
// The membership and missing checks are conditions on the DELETE rather than
// a read taken before it. A sweep clearing missing_since in the window
// between such a read and the delete would forget a path that had just come
// back, and forgetting is not reversible. PruneMissingFiles is not reused
// for the same reason: it verifies nothing by design, on the understanding
// that its caller confirmed each absence with a live Lstat this sweep, which
// a click has not.
//
// A row that matches neither book nor mark returns (false, false, nil) — a
// double click, or a row a sweep has since cleared, is a slip and not an
// error, the same contract DeleteRecipient holds.
func (db *DB) ForgetMissingFile(ctx context.Context, bookID, fileID int64) (forgotten bool, bookDeleted bool, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		forgotten, bookDeleted = false, false

		res, err := tx.ExecContext(ctx, `
			DELETE FROM book_files
			WHERE id = ? AND book_id = ? AND missing_since IS NOT NULL`, fileID, bookID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil || affected == 0 {
			return err
		}
		forgotten = true

		bookDeleted, _, err = pruneOrphanedBookTx(ctx, tx, bookID)
		return err
	})
	return forgotten, bookDeleted, err
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
