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
// locations rather than picked apart); genuinely new content becomes a new
// book, with its cover (if any) extracted and stored under coversDir. A
// per-file error is logged and counted rather than aborting the sweep.
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
		bookID, err := createBook(ctx, db, path, hash, coversDir)
		if err != nil {
			return fmt.Errorf("create book: %w", err)
		}
		if _, err := db.UpsertBookFile(ctx, bookID, path, size, mtime); err != nil {
			return fmt.Errorf("attach file location: %w", err)
		}
		result.New++
		return nil
	}

	// known content at a path with no (or a stale) book_files row: a move,
	// a rename, or an additional location for byte-identical content
	if _, err := db.UpsertBookFile(ctx, book.ID, path, size, mtime); err != nil {
		return fmt.Errorf("attach file location: %w", err)
	}
	result.Moved++
	return nil
}

func createBook(ctx context.Context, db *storage.DB, path, hash, coversDir string) (int64, error) {
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
		SortTitle:   meta.Title,
		Language:    meta.Language,
		ISBN:        meta.ISBN,
		Description: meta.Description,
		CoverPath:   coverPath,
		Format:      strings.TrimPrefix(ext, "."),
	}
	return db.CreateBook(ctx, book, meta.Authors)
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
