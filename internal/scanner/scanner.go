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
	"log"
	"os"
	"path/filepath"
	"strings"

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
// cheap check, moved/renamed files update their existing row's location,
// and genuinely new content becomes a new book. A per-file error is logged
// and counted rather than aborting the sweep.
func Scan(ctx context.Context, db *storage.DB, libraryDir string) (Result, error) {
	var result Result

	err := filepath.WalkDir(libraryDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !supportedExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}

		result.Scanned++
		if err := scanFile(ctx, db, path, &result); err != nil {
			log.Printf("scanner: %s: %v", path, err)
			result.Errors++
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("walk %s: %w", libraryDir, err)
	}
	return result, nil
}

func scanFile(ctx context.Context, db *storage.DB, path string, result *Result) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	mtime := info.ModTime()

	existing, err := db.FindBookByPath(ctx, path)
	if err != nil {
		return fmt.Errorf("find by path: %w", err)
	}
	if existing != nil && existing.FileSize == size && existing.ModifiedAt.Equal(mtime) {
		result.Unchanged++
		return nil
	}

	hash, err := hashFile(path)
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}

	if existing != nil && existing.ContentHash == hash {
		if err := db.UpdateBookFileStat(ctx, existing.ID, size, mtime); err != nil {
			return fmt.Errorf("update file stat: %w", err)
		}
		result.Unchanged++
		return nil
	}

	byHash, err := db.FindBookByContentHash(ctx, hash)
	if err != nil {
		return fmt.Errorf("find by content hash: %w", err)
	}
	if byHash != nil {
		if err := db.UpdateBookFileLocation(ctx, byHash.ID, path, size, mtime); err != nil {
			return fmt.Errorf("update file location: %w", err)
		}
		result.Moved++
		return nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	meta := extractMetadata(path, ext)

	book := storage.Book{
		ContentHash: hash,
		Title:       meta.Title,
		SortTitle:   meta.Title,
		Language:    meta.Language,
		ISBN:        meta.ISBN,
		Description: meta.Description,
		FilePath:    path,
		Format:      strings.TrimPrefix(ext, "."),
		FileSize:    size,
		ModifiedAt:  mtime,
	}
	if _, err := db.CreateBook(ctx, book, meta.Authors); err != nil {
		return fmt.Errorf("create book: %w", err)
	}
	result.New++
	return nil
}

type bookMeta struct {
	Title       string
	Authors     []string
	Language    string
	ISBN        string
	Description string
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
		log.Printf("scanner: %s: reading embedded metadata: %v", path, err)
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
