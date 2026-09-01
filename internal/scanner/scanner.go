// Package scanner walks the library directory and keeps the storage index
// in sync with it, per DESIGN.md's scanner and metadata-source rules.
package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"library/internal/cover"
	"library/internal/epub"
	"library/internal/storage"
)

// Result summarizes what a Scan did, for logging.
type Result struct {
	Scanned   int
	New       int
	Moved     int
	Unchanged int
	Orphaned  int
	Errors    int
}

var supportedExtensions = map[string]bool{
	".epub": true,
	".fb2":  true,
}

// Scan walks libraryDir and, for every supported book file, brings the
// index up to date: unchanged files are skipped via the path+size+mtime
// cheap check; known content found at a path with no book_files row yet
// gets one attached (covering a moved/renamed file and an additional
// location for byte-identical content alike — the two are indistinguishable
// from a single path's perspective, and both are recorded as extra
// locations rather than picked apart) — if that reassignment leaves the
// path's previous owner with no locations of its own, that book row is
// deleted as an orphan; genuinely new content becomes a new book, created
// together with its first file location in one transaction, with its cover
// (if any) extracted and stored under coversDir. A per-file error is
// logged and counted rather than aborting the sweep.
func Scan(ctx context.Context, db *storage.DB, libraryDir, coversDir string) (Result, error) {
	var result Result

	err := filepath.WalkDir(libraryDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !supportedExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}

		result.Scanned++
		if err := scanFile(ctx, db, path, coversDir, &result); err != nil {
			slog.Warn("scan file failed", "path", path, "error", err)
			result.Errors++
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("walk %s: %w", libraryDir, err)
	}
	return result, nil
}

func scanFile(ctx context.Context, db *storage.DB, path, coversDir string, result *Result) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	mtime := info.ModTime()

	bf, err := db.FindFileByPath(ctx, path)
	if err != nil {
		return fmt.Errorf("find file by path: %w", err)
	}
	if bf != nil && bf.FileSize == size && bf.ModifiedAt.Equal(mtime) {
		result.Unchanged++
		return nil
	}

	hash, err := hashFile(path)
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}

	book, err := db.FindBookByContentHash(ctx, hash)
	if err != nil {
		return fmt.Errorf("find book by content hash: %w", err)
	}

	if book != nil && bf != nil && bf.BookID == book.ID {
		// same book, same path: content unchanged, only size/mtime drifted (e.g. touched)
		if err := db.UpdateBookFileStat(ctx, bf.ID, size, mtime); err != nil {
			return fmt.Errorf("update file stat: %w", err)
		}
		result.Unchanged++
		return nil
	}

	if book == nil {
		orphanedID, orphanedTitle, err := createBook(ctx, db, path, hash, coversDir, size, mtime)
		if err != nil {
			return fmt.Errorf("create book: %w", err)
		}
		logOrphan(path, orphanedID, orphanedTitle, result)
		result.New++
		return nil
	}

	// known content at a path with no (or a stale) book_files row: a move,
	// a rename, or an additional location for byte-identical content
	_, orphanedID, orphanedTitle, err := db.ReassignFileAndPruneOrphan(ctx, book.ID, path, size, mtime)
	if err != nil {
		return fmt.Errorf("attach file location: %w", err)
	}
	logOrphan(path, orphanedID, orphanedTitle, result)
	result.Moved++
	return nil
}

// logOrphan records a book deleted because a path reassignment left it
// with zero locations — reachable both when the path's new content
// matches an existing book (ReassignFileAndPruneOrphan) and when it
// doesn't (CreateBookWithFile): upsertBookFileTx reassigns a path
// unconditionally either way, so either path can orphan whoever owned it
// before. orphanedID is 0 when nothing was orphaned.
func logOrphan(path string, orphanedID int64, orphanedTitle string, result *Result) {
	if orphanedID == 0 {
		return
	}
	slog.Info("book orphaned", "path", path, "orphaned_book_id", orphanedID, "orphaned_title", orphanedTitle)
	result.Orphaned++
}

func createBook(ctx context.Context, db *storage.DB, path, hash, coversDir string, size int64, mtime time.Time) (orphanedID int64, orphanedTitle string, err error) {
	ext := strings.ToLower(filepath.Ext(path))
	meta := extractMetadata(path, ext)

	var coverPath string
	if len(meta.Cover) > 0 {
		p, err := cover.Store(coversDir, hash, meta.Cover)
		if err != nil {
			slog.Warn("store cover failed", "path", path, "error", err)
		} else {
			coverPath = p
		}
	}

	book := storage.Book{
		ContentHash: hash,
		Title:       meta.Title,
		SortTitle:   sortTitle(meta.Title),
		Language:    meta.Language,
		ISBN:        meta.ISBN,
		Description: meta.Description,
		CoverPath:   coverPath,
		Format:      strings.TrimPrefix(ext, "."),
	}
	_, orphanedID, orphanedTitle, err = db.CreateBookWithFile(ctx, book, meta.Authors, path, size, mtime)
	return orphanedID, orphanedTitle, err
}

type bookMeta struct {
	Title       string
	Authors     []string
	Language    string
	ISBN        string
	Description string
	Cover       []byte
}

// extractMetadata pulls embedded metadata for supported formats (currently
// EPUB only) and falls back to a filename-derived title whenever embedded
// metadata is unavailable, unparseable, or missing a title.
func extractMetadata(path, ext string) bookMeta {
	fallbackTitle := filenameTitle(path)

	if ext != ".epub" {
		return bookMeta{Title: fallbackTitle}
	}

	m, err := epub.ReadMetadata(path)
	if err != nil {
		slog.Warn("read embedded metadata failed", "path", path, "error", err)
		return bookMeta{Title: fallbackTitle}
	}

	title := m.Title
	if title == "" {
		title = fallbackTitle
	}

	return bookMeta{
		Title:       title,
		Authors:     m.Authors,
		Language:    m.Language,
		ISBN:        m.ISBN,
		Description: m.Description,
		Cover:       m.Cover,
	}
}

func filenameTitle(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// leadingArticles are stripped from a title to derive its sort form, longest
// first so "An Ideal Husband" doesn't lose only its "A".
//
// English only. Guessing articles across languages ("Der", "La", "El")
// mis-files any title that legitimately starts with one of those words, and
// the collection is mostly English.
var leadingArticles = []string{"the ", "an ", "a "}

// sortTitle derives the form a title files under: one leading article
// removed and the rest case folded, so "The Hobbit" sorts under H and
// "apple book" sorts among the A's rather than after every capitalised
// title.
//
// Punctuation and leading digits are deliberately left alone — "'Salem's
// Lot" and "1984" file under "'" and "1", which is the "before A" bucket a
// reader scanning alphabetically expects.
func sortTitle(title string) string {
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

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
