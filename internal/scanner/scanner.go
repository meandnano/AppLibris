// Package scanner walks the library directory and keeps the storage index
// in sync with it, per DESIGN.md's scanner and metadata-source rules.
package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	Missing   int
	Pruned    int
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
// logged and counted rather than aborting the sweep — and so is a
// directory WalkDir can't read: its subtree is skipped, not the rest of
// the library. Only a failure on libraryDir itself (missing, unmounted) is
// fatal, since that must not look like an empty library.
//
// After a clean walk, Scan reconciles book_files rows that weren't seen:
// under a subtree that was itself walked cleanly, a row not seen this
// sweep is marked missing (or, if already marked, left alone); a row seen
// again has any mark cleared. Rows past missingGrace since being marked
// are then deleted, along with any book that leaves with no locations —
// see reconcileMissing for the guards that keep an unmounted volume or a
// skipped subtree from ever being read as "these files are gone."
func Scan(ctx context.Context, db *storage.DB, libraryDir, coversDir string, missingGrace time.Duration) (Result, error) {
	var result Result
	seen := make(map[string]bool)
	var skippedDirs []string

	err := filepath.WalkDir(libraryDir, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			// a directory we can't read costs us its subtree, not the sweep —
			// anything else (including an error on the root itself) is fatal;
			// d is nil when the error comes from os.Lstat on the root itself
			if d != nil && d.IsDir() && walkPath != libraryDir {
				slog.Warn("skipping unreadable directory", "path", walkPath, "error", err)
				result.Errors++
				skippedDirs = append(skippedDirs, relSlash(libraryDir, walkPath))
				return fs.SkipDir
			}
			return err
		}
		if d.IsDir() || !supportedExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}

		result.Scanned++
		if rel := relSlash(libraryDir, walkPath); rel != "" {
			seen[rel] = true
		}
		if err := scanFile(ctx, db, libraryDir, walkPath, coversDir, &result); err != nil {
			slog.Warn("scan file failed", "path", walkPath, "error", err)
			result.Errors++
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("walk %s: %w", libraryDir, err)
	}

	reconcileMissing(ctx, db, libraryDir, skippedDirs, seen, missingGrace, &result)

	return result, nil
}

// relSlash returns path relative to libraryDir, slash-separated — the form
// book_files.file_path is stored in. Empty on error, which callers treat
// as "don't touch this path" rather than a hard failure; scanFile
// independently recomputes and reports the same failure through the
// per-file error path, so nothing is silently dropped.
func relSlash(libraryDir, path string) string {
	rel, err := filepath.Rel(libraryDir, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// reconcileMissing marks, clears, and prunes book_files rows once a walk
// completes. Nothing here runs if the walk found no files at all — a
// successfully-mounted-but-empty directory is indistinguishable from a
// volume that mounted empty, and the cost of getting that wrong is the
// whole index, so an apparently-empty library prunes nothing and only
// warns.
//
// skippedDirs are the directories this sweep couldn't read (see Scan); a
// book_files row under one of them is left alone entirely, whether or not
// it was previously marked, since not being able to look isn't evidence
// one way or the other.
func reconcileMissing(ctx context.Context, db *storage.DB, libraryDir string, skippedDirs []string, seen map[string]bool, missingGrace time.Duration, result *Result) {
	if result.Scanned == 0 {
		slog.Warn("library appeared empty, skipping missing-file reconciliation", "library_dir", libraryDir)
		return
	}

	all, err := db.ListFilesUnder(ctx, "")
	if err != nil {
		slog.Warn("list files for missing-file reconciliation failed", "error", err)
		return
	}

	var toMark, toClear []int64
	for _, f := range all {
		if underAny(f.FilePath, skippedDirs) {
			continue
		}
		if seen[f.FilePath] {
			if f.MissingSince.Valid {
				toClear = append(toClear, f.ID)
			}
			continue
		}
		if f.MissingSince.Valid {
			continue // already marked; no need to re-confirm every sweep
		}

		// Not seen this sweep, in a subtree we did read successfully — but
		// os.Lstat directly on the path, not just absence from the walk,
		// is what decides "gone" versus "couldn't tell": ErrNotExist marks
		// it, anything else (EACCES, EIO, a timeout) only warns.
		absPath := filepath.Join(libraryDir, filepath.FromSlash(f.FilePath))
		if _, statErr := os.Lstat(absPath); statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				toMark = append(toMark, f.ID)
			} else {
				slog.Warn("could not confirm missing file", "path", absPath, "error", statErr)
			}
		}
	}

	now := time.Now()
	if len(toMark) > 0 {
		if err := db.SetFilesMissing(ctx, toMark, now); err != nil {
			slog.Warn("mark missing files failed", "error", err)
		} else {
			result.Missing = len(toMark)
		}
	}
	if len(toClear) > 0 {
		if err := db.ClearFilesMissing(ctx, toClear); err != nil {
			// toClear rows are confirmed present this sweep but may still
			// carry a stale, past-grace missing_since if the clear didn't
			// take — pruning now, blind to that, could delete a file that's
			// sitting right there on disk. Skip pruning entirely rather than
			// risk it; the next successful sweep clears them and resumes.
			slog.Warn("clear missing files failed, skipping prune this sweep", "error", err)
			return
		}
	}

	files, books, err := db.PruneMissingFiles(ctx, now.Add(-missingGrace), skippedDirs)
	if err != nil {
		slog.Warn("prune missing files failed", "error", err)
		return
	}
	result.Pruned = files
	if files > 0 || books > 0 {
		slog.Info("pruned missing files", "files", files, "books", books)
	}
}

// underAny reports whether relPath is nested under any of prefixes.
func underAny(relPath string, prefixes []string) bool {
	for _, p := range prefixes {
		if relPath == p || strings.HasPrefix(relPath, p+"/") {
			return true
		}
	}
	return false
}

func scanFile(ctx context.Context, db *storage.DB, libraryDir, path, coversDir string, result *Result) error {
	// stored relative to libraryDir (slash-separated) so the index survives
	// the library being mounted at a different absolute path — dev's
	// ./library versus the container's /library, say; anything that needs
	// to touch the filesystem below still uses the absolute path
	rel, err := filepath.Rel(libraryDir, path)
	if err != nil {
		return fmt.Errorf("relativize: %w", err)
	}
	rel = filepath.ToSlash(rel)

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	mtime := info.ModTime()

	bf, err := db.FindFileByPath(ctx, rel)
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
		orphanedID, orphanedTitle, err := createBook(ctx, db, path, rel, hash, coversDir, size, mtime)
		if err != nil {
			return fmt.Errorf("create book: %w", err)
		}
		logOrphan(path, orphanedID, orphanedTitle, result)
		result.New++
		return nil
	}

	// known content at a path with no (or a stale) book_files row: a move,
	// a rename, or an additional location for byte-identical content
	_, orphanedID, orphanedTitle, err := db.ReassignFileAndPruneOrphan(ctx, book.ID, rel, size, mtime)
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

// createBook reads metadata from the file at the absolute path and stores
// the book under rel, its path relative to the library root.
func createBook(ctx context.Context, db *storage.DB, path, rel, hash, coversDir string, size int64, mtime time.Time) (orphanedID int64, orphanedTitle string, err error) {
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
	_, orphanedID, orphanedTitle, err = db.CreateBookWithFile(ctx, book, meta.Authors, rel, size, mtime)
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
