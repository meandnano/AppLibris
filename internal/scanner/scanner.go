// Package scanner walks the library directory and keeps the storage index
// in sync with it, per docs/notes/scanner.md's scanner and metadata-source rules.
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
	"library/internal/fb2"
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
	// Unconfirmed counts rows absent this sweep whose top-level directory
	// yielded no files, so they were marked missing but not trusted for
	// deletion: their absence may be an offline sub-mount
	Unconfirmed int
	// UnconfirmedDirs breaks Unconfirmed down by top-level directory.
	// Reported rather than logged here because the level the line deserves
	// depends on whether the same directory was unconfirmed last sweep,
	// which a stateless Scan cannot know and its one caller already tracks
	UnconfirmedDirs   map[string]int
	CoversRegenerated int
	Errors            int
}

// supportedSuffixes are matched as literal filename suffixes rather than
// via filepath.Ext, since a .fb2.zip archive is two extensions and Ext
// would only ever see the last one (".zip").
var supportedSuffixes = []string{".epub", ".fb2.zip", ".fb2"}

// matchedSuffix returns whichever of supportedSuffixes name ends with,
// case-insensitively, or "" if none matches.
func matchedSuffix(name string) string {
	lower := strings.ToLower(name)
	for _, s := range supportedSuffixes {
		if strings.HasSuffix(lower, s) {
			return s
		}
	}
	return ""
}

// bookFormat maps a matched suffix to the value stored in books.format.
// How a book is packaged on disk (.fb2 vs. a .fb2.zip archive) isn't
// something the format badge in the UI should surface, so both map to
// "fb2".
func bookFormat(suffix string) string {
	switch suffix {
	case ".fb2", ".fb2.zip":
		return "fb2"
	default:
		return strings.TrimPrefix(suffix, ".")
	}
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
// fatal, since that must not look like an empty library. A symlinked
// subdirectory is not followed either, and nor is one whose target cannot
// be stat'd at all; both are logged and counted the same way — see the
// walk callback for why following one was rejected. A symlinked *file* is
// indexed as any other, since every read of it goes through the link.
// ctx cancellation is checked before the walk starts and on every entry
// the walk visits; either stops Scan immediately and returns ctx.Err()
// (wrapped), visiting no further entries and skipping reconciliation
// entirely — a cancelled sweep must never be read as "here is what the
// library currently looks like."
//
// After a clean walk, Scan reconciles book_files rows that weren't seen:
// under a subtree that was itself walked cleanly, a row not seen this
// sweep and reconfirmed absent (via os.Lstat, every sweep, whether or not
// already marked) is marked missing; a row seen again has any mark
// cleared. A row is only actually deleted once it's both past missingGrace
// and reconfirmed absent this exact sweep — along with any book that
// leaves with no locations — see reconcileMissing for the full set of
// guards that keep an unmounted volume, a skipped subtree, or a stale
// confirmation from ever being read as "this file is gone."
func Scan(ctx context.Context, db *storage.DB, libraryDir, coversDir string, missingGrace time.Duration) (Result, error) {
	var result Result
	if err := ctx.Err(); err != nil {
		return result, err
	}

	seen := make(map[string]bool)
	var skippedDirs, linkedDirs []string

	err := filepath.WalkDir(libraryDir, func(walkPath string, d fs.DirEntry, err error) error {
		// Checked first, ahead of everything below: a per-file error (this
		// callback's own err parameter) is worth a Warn and a continued
		// walk, but cancellation must stop the walk outright — WalkDir
		// only does that for a non-nil, non-SkipDir/SkipAll return, so
		// ctx.Err() has to be surfaced as the callback's return value
		// itself, not folded into the per-file error counting below.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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
		// WalkDir never follows a link, so a symlinked directory arrives
		// here as a non-directory entry whose name has no supported
		// suffix and the filter below would drop it without a word.
		// Following it is the alternative that was rejected: it needs a
		// (dev, ino) cycle guard, and a link pointing outside the library
		// indexes files whose relative file_path cannot express where they
		// are. So it stays unfollowed and says so — counted as an error so
		// the sweep summary carries it, and repeated every sweep as
		// pressure toward a bind mount instead
		if d.Type()&fs.ModeSymlink != 0 {
			info, statErr := os.Stat(walkPath)
			switch {
			case statErr == nil && info.IsDir():
				attrs := []any{"path", walkPath}
				if target, linkErr := os.Readlink(walkPath); linkErr == nil {
					attrs = append(attrs, "target", target)
				}
				slog.Warn("symlinked directory is not followed", attrs...)
				result.Errors++
				// Recorded so reconciliation can refuse to prune the rows
				// under it. Their Lstat resolves every component but the
				// leaf, so it succeeds through the link and, under the rule
				// above, would otherwise mark them and then delete them
				// after the grace period — deleting a book whose file is
				// sitting there, readable through the link the walk
				// declined to follow. Not skippedDirs: those rows are left
				// entirely alone, where these deserve the annotation.
				if rel := relSlash(libraryDir, walkPath); rel != "" {
					linkedDirs = append(linkedDirs, rel)
				}
				return nil
			case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
				// A cycle (ELOOP), a target that may not be stat'd
				// (EACCES): the link says nothing about whether books sit
				// behind it, and an unknown is not evidence — the posture
				// missing-file reconciliation takes toward a
				// non-ErrNotExist Lstat. Reported rather than guessed at,
				// where falling through would drop it on the strength of
				// its name alone
				slog.Warn("could not resolve symlink", "path", walkPath, "error", statErr)
				result.Errors++
				return nil
			}
			// resolving to nothing or to a file takes the ordinary route
		}
		if d.IsDir() || matchedSuffix(d.Name()) == "" {
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

	reconcileMissing(ctx, db, libraryDir, skippedDirs, linkedDirs, seen, missingGrace, &result)

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
// book_files row at or under one of them is left alone entirely, whether
// or not it was previously marked, since not being able to look isn't
// evidence one way or the other.
//
// The same posture extends, for the prune alone, to a top-level directory
// whose subtree yielded no files this sweep, whether it read as empty or
// is gone: that is what an offline sub-mount looks like — a second bind
// mount, an NFS share in a subfolder, an Unraid disk mid-rebuild — and
// WalkDir reads it cleanly, so nothing lands in skippedDirs while every
// row under it fails os.Lstat with ErrNotExist, the very signal the two
// phases below trust. Rows under such a directory are still marked: the
// mark is reversible, gives the detail page its annotation, and is what
// keeps internal/sender off a path that is not there — an unmarked dead
// row sorts first in ListBookFiles and fails every send of its book, which
// a renamed top-level folder would otherwise do to every book in it. They
// are never pruned, however old the mark; they are counted in
// Result.Unconfirmed and broken down in Result.UnconfirmedDirs, which
// cmd/server logs — there rather than here because a directory unconfirmed
// for the fortieth sweep running is not news, and only the caller keeps the
// previous sweep's set to tell the two apart.
// The test is the top-level directory rather than the row's own because
// "some ancestor of the row has a seen file under it" reduces to exactly
// that (a file under a nested directory is under its top-level ancestor
// too), so a reorganisation that empties a/novels/ while a/ keeps files
// still prunes, where a disk mounted at a/ going offline leaves nothing
// under a/ at any depth. A mount nested one level down beside a populated
// sibling is therefore not protected. The cost is that the last book file
// deleted from a top-level directory stays marked missing until the
// directory gains a file again, or until a person forgets the row from the
// book's page (storage.ForgetMissingFile) — a phantom card is recoverable,
// a pruned book's manual edits and provenance are not. A renamed top-level
// folder pays the same cost on every book in it, a live Moved row beside a
// dead one, since the old name never regains a file. A
// root-level row has no top-level directory and is covered by the
// Scanned == 0 guard alone, the root case of the same rule kept for its
// specific message
//
// The same refusal covers a directory the walk declined to follow because
// it is a symlink. It yielded no files at any depth, exactly as an offline
// sub-mount does, but the top-level test misses it wherever the link is
// nested under a directory that still holds books — and its rows' Lstat
// resolves through the link and succeeds, so the rule below would mark
// them and then delete them, destroying a book whose file is sitting there
// and readable.
//
// Every unseen, non-excluded row is re-checked with os.Lstat this same
// sweep — including one already marked missing from an earlier sweep.
// Deletion eligibility (past missingGrace) is necessary but never
// sufficient on its own: only a row this exact sweep's Lstat answers for,
// with fs.ErrNotExist or with success, is ever handed to PruneMissingFiles,
// so a path whose failure mode changes while it waits out its grace period
// (say, from ErrNotExist to EACCES) can never be deleted on the strength of
// a confirmation that's since gone stale.
func reconcileMissing(ctx context.Context, db *storage.DB, libraryDir string, skippedDirs, linkedDirs []string, seen map[string]bool, missingGrace time.Duration, result *Result) {
	if result.Scanned == 0 {
		slog.Warn("library appeared empty, skipping missing-file reconciliation", "library_dir", libraryDir)
		return
	}

	all, err := db.ListFilesUnder(ctx, "")
	if err != nil {
		slog.Warn("list files for missing-file reconciliation failed", "error", err)
		return
	}

	now := time.Now()
	cutoff := now.Add(-missingGrace)
	populated := populatedTopLevelDirs(seen)
	unconfirmed := make(map[string]int)

	var toMark, toClear, toPrune []int64
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

		// Not seen this sweep, in a subtree we did read successfully. An
		// os.Lstat error other than ErrNotExist is the only "couldn't tell"
		// here, and it's checked every sweep regardless of whether the row
		// is already marked, precisely so a stale confirmation can never
		// carry a row all the way to deletion on its own. A *successful*
		// Lstat is not an unknown: the walk read that directory cleanly and
		// did not report this exact byte sequence as a name, so either the
		// filesystem matched the recorded spelling to a file the walk
		// recorded under another one (a case-only rename on SMB or macOS),
		// or the file arrived between the walk and this check, which the
		// next sweep sees and clears. Neither leaves the row live under a
		// spelling the walk disagrees with, so it falls through to marking
		// like an absence does — and the two guards below are what keep that
		// safe where the success is legitimately ambiguous, a real directory
		// since replaced by a symlink resolving every component but the leaf.
		absPath := filepath.Join(libraryDir, filepath.FromSlash(f.FilePath))
		if _, statErr := os.Lstat(absPath); statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			slog.Warn("could not confirm missing file", "path", absPath, "error", statErr)
			continue
		}

		// A root-level row has no top-level directory and is covered by
		// the Scanned == 0 guard instead
		unconfirmedDir := false
		if top := topLevelDir(f.FilePath); top != "" && !populated[top] {
			unconfirmed[top]++
			unconfirmedDir = true
		} else if dir := matchingPrefix(f.FilePath, linkedDirs); dir != "" {
			// A directory the walk declined to follow because it is now a
			// symlink yielded no files at any depth, exactly as an offline
			// sub-mount does, but the top-level test above misses it when
			// the link is nested under a directory that still holds books.
			// Its rows' Lstat succeeds through the link, so without this
			// they would be marked and then deleted after the grace period,
			// destroying a book whose file is sitting there and readable.
			unconfirmed[dir]++
			unconfirmedDir = true
		}
		switch {
		case !f.MissingSince.Valid:
			toMark = append(toMark, f.ID)
		case unconfirmedDir:
			// Marked, but a directory that yielded no files is not evidence
			// its books are gone, so the prune is refused whatever the
			// mark's age
		case f.MissingSince.Time.Before(cutoff):
			toPrune = append(toPrune, f.ID)
		}
	}

	if len(unconfirmed) > 0 {
		for _, n := range unconfirmed {
			result.Unconfirmed += n
		}
		result.UnconfirmedDirs = unconfirmed
	}
	if len(toMark) > 0 {
		if err := db.SetFilesMissing(ctx, toMark, now); err != nil {
			slog.Warn("mark missing files failed", "error", err)
		} else {
			result.Missing = len(toMark)
		}
	}
	if len(toClear) > 0 {
		if err := db.ClearFilesMissing(ctx, toClear); err != nil {
			slog.Warn("clear missing files failed", "error", err)
		}
	}
	if len(toPrune) == 0 {
		return
	}

	files, books, err := db.PruneMissingFiles(ctx, toPrune)
	if err != nil {
		slog.Warn("prune missing files failed", "error", err)
		return
	}
	result.Pruned = files
	if files > 0 || books > 0 {
		slog.Info("pruned missing files", "files", files, "books", books)
	}
}

// topLevelDir returns the first segment of a slash-separated relative
// path, or "" for a path directly under the library root
func topLevelDir(relPath string) string {
	if i := strings.IndexByte(relPath, '/'); i >= 0 {
		return relPath[:i]
	}
	return ""
}

// TopLevelDirHasBooks reports whether the top-level directory under
// libraryDir that relPath sits in exists and holds at least one supported
// book file at any depth. A root-level relPath has no such directory and
// reports true, matching reconcileMissing's own rule that a root-level file
// is not a mount shape.
//
// This is the sweep's unconfirmed-directory test, asked one path at a time,
// for a caller with no walk of its own: internal/sender uses it to decide
// whether an ENOENT under a book's path means the book is gone or the
// volume it sits on is offline, the shape an unmounted mountpoint presenting
// as an empty directory takes. Exported rather than copied there, because
// two statements of one rule about the same directory drift and this is the
// one with tests.
//
// A directory that is not there is (false, nil). Any other walk error is
// returned rather than folded into false: an unknown is not evidence, and
// the caller has a third answer for it.
func TopLevelDirHasBooks(libraryDir, relPath string) (bool, error) {
	top := topLevelDir(relPath)
	if top == "" {
		return true, nil
	}

	found := false
	err := filepath.WalkDir(filepath.Join(libraryDir, filepath.FromSlash(top)), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && matchedSuffix(d.Name()) != "" {
			found = true
			// The answer is "at least one", so the first match ends the
			// walk: on a populated directory this costs a handful of stats
			// rather than a traversal of the whole subtree.
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return found, nil
}

// populatedTopLevelDirs is the set of top-level directories with at least
// one seen file under them, at any depth
func populatedTopLevelDirs(seen map[string]bool) map[string]bool {
	populated := make(map[string]bool)
	for p := range seen {
		if top := topLevelDir(p); top != "" {
			populated[top] = true
		}
	}
	return populated
}

// underAny reports whether relPath is nested under any of prefixes.
func underAny(relPath string, prefixes []string) bool {
	return matchingPrefix(relPath, prefixes) != ""
}

// matchingPrefix returns whichever of prefixes relPath is nested under, or
// "" if none is. Callers that need to name the directory in a log line or a
// per-directory count take this; underAny is the boolean form.
func matchingPrefix(relPath string, prefixes []string) string {
	for _, p := range prefixes {
		if relPath == p || strings.HasPrefix(relPath, p+"/") {
			return p
		}
	}
	return ""
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
		book := &storage.Book{
			ID:          bf.BookID,
			ContentHash: bf.BookContentHash,
			CoverPath:   bf.BookCoverPath,
			CoverRetry:  bf.BookCoverRetry,
		}
		maybeRegenerateCover(ctx, db, book, path, coversDir, result)
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
		maybeRegenerateCover(ctx, db, book, path, coversDir, result)
		result.Unchanged++
		return nil
	}

	if book == nil {
		orphanedID, orphanedTitle, inherited, err := createBook(ctx, db, path, rel, hash, coversDir, size, mtime)
		if err != nil {
			return fmt.Errorf("create book: %w", err)
		}
		logOrphan(path, orphanedID, orphanedTitle, inherited, result)
		result.New++
		return nil
	}

	maybeRegenerateCover(ctx, db, book, path, coversDir, result)

	// known content at a path with no (or a stale) book_files row: a move,
	// a rename, or an additional location for byte-identical content.
	// Nothing is inherited here: the book this can orphan is a different
	// book that happened to lose its last copy, not this one under new
	// bytes, so there is nothing of its owner's to carry across.
	_, orphanedID, orphanedTitle, err := db.ReassignFileAndPruneOrphan(ctx, book.ID, rel, size, mtime)
	if err != nil {
		return fmt.Errorf("attach file location: %w", err)
	}
	logOrphan(path, orphanedID, orphanedTitle, nil, result)
	result.Moved++
	return nil
}

// coverFileDefinitelyGone reports whether the stored thumbnail is known to
// be unusable: nothing recorded, absent, or empty. maybeRegenerateCover's
// own stat is skipped when cover_retry is set, so a clear reached by that
// route would otherwise have no evidence about the file at all.
//
// Only fs.ErrNotExist counts as absent. Any other stat failure — an EACCES
// or EIO on COVERS_DIR — leaves it unknown, and forgetting a cover on an
// unknown is the same mistake as forgetting one on an unreadable
// provenance: it says nothing about whether the file is there. That is the
// posture maybeRegenerateCover's own stat takes, and the one missing-file
// reconciliation takes toward an ambiguous Lstat.
func coverFileDefinitelyGone(path string) bool {
	if path == "" {
		return true
	}
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return info.Size() == 0
	case errors.Is(err, fs.ErrNotExist):
		return true
	default:
		slog.Warn("inspect cover failed", "path", path, "error", err)
		return false
	}
}

// forgetUnregenerableCover handles a book whose stored cover is unusable and
// whose source file has no embedded cover to rebuild it from. When a
// provider supplied that cover there is nothing to regenerate and nothing to
// warn about on every sweep forever, so the field is forgotten instead:
// cover_path and its provenance go, which puts the cover back in
// enrichment's missing set so the Fetch button repairs it, and leaves the
// grid showing its honest "no cover" box rather than an <img> pointing at a
// file that is gone.
//
// The provider test is that a field_sources row *exists*, not that it names
// a provider rather than "embedded": setEmbeddedFieldSourcesTx never writes
// a cover row, so a cover the scanner extracted has no provenance at all,
// and comparing against "embedded" would match nothing while reading as
// correct.
//
// readErr is whatever readEmbeddedCover reported. It is not merely phrasing
// for the warning: a non-nil one stops this function before it can forget
// anything, per the first branch.
func forgetUnregenerableCover(ctx context.Context, db *storage.DB, book *storage.Book, sourcePath string, readErr error) {
	// A read *error* establishes nothing. len(coverBytes) == 0 means the
	// file holds no cover; a failure to open or parse it means the question
	// was never answered, and clearing on that would discard a regenerable
	// cover permanently — once cover_path is empty this function is never
	// reached again, so the embedded original is not recovered even when the
	// read starts working. The only repair left would be Fetch, which brings
	// back the provider's image rather than the book's own, and COVERS_DIR
	// stops being disposable for that book by a different door.
	//
	// Same standard the provenance read below holds itself to, and the one
	// missing-file reconciliation holds for an ambiguous Lstat.
	if readErr != nil {
		slog.Warn("regenerate cover failed", "path", sourcePath, "error", readErr)
		return
	}

	sources, err := db.FieldSourcesForBook(ctx, book.ID)
	if err != nil {
		// A storage error says nothing about where the cover came from, and
		// guessing either way is how a scanner-extracted cover gets silently
		// discarded. Leave it for the next sweep — the same posture
		// missing-file reconciliation takes toward an ambiguous Lstat.
		slog.Warn("read cover provenance failed", "book_id", book.ID, "error", err)
		return
	}
	_, fromProvider := sources[storage.FieldCover]

	// Confirmed unusable before clearing, not assumed. The stat in
	// maybeRegenerateCover is skipped entirely when cover_retry is set, so
	// without this a book carrying that marker beside a provider row could
	// have a present, perfectly good cover thrown away.
	//
	// It covers the books that reach here, which is not every book in that
	// state: one whose file *does* still hold an embedded cover never
	// arrives, because re-extraction succeeds and UpdateBookCoverPath
	// overwrites the path — orphaning the provider's file rather than
	// forgetting it. That overwrite predates this branch and is at least
	// self-consistent now that the same write drops the provenance row, so
	// it is left alone rather than half-fixed here.
	//
	// The pairing should not occur at all — updateBookColumnTx clears the
	// marker whenever it writes a path — but the invariant lives in another
	// package and nothing here would notice it breaking.
	if fromProvider && coverFileDefinitelyGone(book.CoverPath) {
		// The observed path is passed through so the write can refuse a
		// cover that arrived while this sweep was parsing.
		cleared, err := db.ClearProviderCover(ctx, book.ID, book.CoverPath, time.Now())
		if err != nil {
			slog.Warn("clear provider cover failed", "book_id", book.ID, "error", err)
			return
		}
		if cleared {
			slog.Info("provider cover forgotten", "book_id", book.ID, "cover_path", book.CoverPath)
		} else {
			// Refused because the path moved under this sweep — correct, and
			// a no-op, but an operator asking why an eligible-looking book
			// was left alone has nothing to read otherwise.
			slog.Debug("provider cover unchanged", "book_id", book.ID, "observed_path", book.CoverPath)
		}
		return
	}

	slog.Warn("regenerate cover failed", "path", sourcePath, "error", "embedded cover is missing")
}

func maybeRegenerateCover(ctx context.Context, db *storage.DB, book *storage.Book, sourcePath, coversDir string, result *Result) {
	if book == nil || (book.CoverPath == "" && !book.CoverRetry) {
		return
	}

	if !book.CoverRetry {
		info, err := os.Stat(book.CoverPath)
		if err == nil {
			if info.Size() > 0 {
				return
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("inspect cover failed", "path", book.CoverPath, "error", err)
			return
		}
	}

	coverBytes, err := readEmbeddedCover(sourcePath, matchedSuffix(sourcePath))
	if err != nil || len(coverBytes) == 0 {
		// Re-extraction produced nothing usable, by one of two routes the
		// callee separates: the file holds no cover (len == 0), or the
		// question was never answered (err). Only the first can justify
		// forgetting anything.
		//
		// It is tried before provenance is consulted at all, because "there
		// is a provider row" never implied "there is nothing to
		// re-extract": a book can carry an embedded cover *and* a provider
		// row, if cover.Store failed when it was first seen (leaving
		// cover_retry set and no cover row, since setEmbeddedFieldSourcesTx
		// never writes one) and enrichment then supplied a cover of its own.
		forgetUnregenerableCover(ctx, db, book, sourcePath, err)
		return
	}

	coverPath, err := cover.Store(coversDir, book.ContentHash, coverBytes)
	if err != nil {
		if errors.Is(err, cover.ErrUnsupportedCover) {
			recordUnusableCover(ctx, db, book, sourcePath, err)
			return
		}
		slog.Warn("regenerate cover failed", "path", sourcePath, "error", err)
		return
	}
	if err := db.UpdateBookCoverPath(ctx, book.ID, coverPath); err != nil {
		slog.Warn("update regenerated cover path failed", "path", sourcePath, "error", err)
		return
	}

	result.CoversRegenerated++
	slog.Info("cover regenerated", "path", sourcePath, "cover_path", coverPath)
}

// recordUnusableCover handles an embedded cover that cover.Store refused
// for what it is rather than for where it was going. The retry marker and
// the missing-file check both exist to bring a cover back, and there is
// nothing to bring back: the same bytes fail the same way on every sweep,
// and each attempt re-parses the whole book to find that out. So the book
// is put in the state one with no embedded cover has, which the first guard
// in maybeRegenerateCover passes by from then on.
//
// Two states arrive here. A book indexed before decode failures were told
// apart from I/O ones carries cover_retry from that first store, and this is
// what finally clears it. And a book whose provider cover has gone from
// disk, with an embedded original that cannot replace it, loses the provider
// row too — the same forgetting forgetUnregenerableCover does when there is
// no embedded cover at all, reached by a different door.
//
// The write is guarded on the cover_path this sweep observed, so a cover an
// enrichment run wrote while the book was being parsed is left alone.
//
// And it is refused outright when a stored cover is present: with cover_retry
// set, maybeRegenerateCover skips its stat, so a book carrying the marker
// beside a provider path arrives here with no evidence about that file at
// all, and blanking it would throw away a perfectly good cover on the
// strength of the embedded one being undecodable. That pairing should not
// occur — every write of a path clears the marker — but the invariant lives
// in another package, and this is the same "confirmed gone, not assumed"
// posture forgetUnregenerableCover takes for the same reason
func recordUnusableCover(ctx context.Context, db *storage.DB, book *storage.Book, sourcePath string, storeErr error) {
	if book.CoverPath != "" && !coverFileDefinitelyGone(book.CoverPath) {
		slog.Warn("embedded cover unusable but stored cover present", "book_id", book.ID, "cover_path", book.CoverPath, "error", storeErr)
		return
	}
	recorded, err := db.RecordUnusableCover(ctx, book.ID, book.CoverPath, time.Now())
	if err != nil {
		slog.Warn("record unusable cover failed", "path", sourcePath, "error", err)
		return
	}
	if recorded {
		slog.Info("embedded cover unusable", "path", sourcePath, "error", storeErr)
	} else {
		slog.Debug("cover unchanged", "book_id", book.ID, "observed_path", book.CoverPath)
	}
}

// readEmbeddedCover returns just the embedded cover bytes for path,
// dispatching by suffix the same way extractMetadata does. This used to be
// an unconditional epub.ReadMetadata call regardless of format, which meant
// an FB2 book's cover could never actually be regenerated once its stored
// file went missing or empty — it would try to parse the FB2 document as
// an EPUB zip and fail every time.
func readEmbeddedCover(path, suffix string) ([]byte, error) {
	switch suffix {
	case ".epub":
		m, err := epub.ReadMetadata(path)
		return m.Cover, err
	case ".fb2", ".fb2.zip":
		m, err := fb2.ReadMetadata(path)
		return m.Cover, err
	default:
		return nil, fmt.Errorf("unsupported suffix %q", suffix)
	}
}

// logOrphan records a book deleted because a path reassignment left it
// with zero locations — reachable both when the path's new content
// matches an existing book (ReassignFileAndPruneOrphan) and when it
// doesn't (CreateBookWithFile): upsertBookFileTx reassigns a path
// unconditionally either way, so either path can orphan whoever owned it
// before. orphanedID is 0 when nothing was orphaned.
//
// inherited names the fields the replacement carried over from the book it
// replaced, empty for every caller that cannot inherit. It is logged because
// a value appearing on a book whose file was just rewritten is otherwise
// unexplained: the page shows an edited title with no marker beside it,
// which is exactly what a hand-edited value looks like, and only this line
// says the edit was made against different bytes.
func logOrphan(path string, orphanedID int64, orphanedTitle string, inherited []storage.MetadataField, result *Result) {
	if orphanedID == 0 {
		return
	}
	attrs := []any{"path", path, "orphaned_book_id", orphanedID, "orphaned_title", orphanedTitle}
	if len(inherited) > 0 {
		attrs = append(attrs, "inherited", inherited)
	}
	slog.Info("book orphaned", attrs...)
	result.Orphaned++
}

// createBook reads metadata from the file at the absolute path and stores
// the book under rel, its path relative to the library root.
func createBook(ctx context.Context, db *storage.DB, path, rel, hash, coversDir string, size int64, mtime time.Time) (orphanedID int64, orphanedTitle string, inherited []storage.MetadataField, err error) {
	suffix := matchedSuffix(path)
	meta := extractMetadata(path, suffix)

	var coverPath string
	var coverRetry bool
	if len(meta.Cover) > 0 {
		p, err := cover.Store(coversDir, hash, meta.Cover)
		switch {
		case err == nil:
			coverPath = p
		case errors.Is(err, cover.ErrUnsupportedCover):
			// The image itself is the problem — a format nothing decodes,
			// a corrupt file, one past the caps — so a retry would re-parse
			// the book to reach the same refusal, on every sweep, forever.
			// Recorded as no cover instead: the state a book without an
			// embedded cover has, which is never retried. cover_retry is
			// only for a store that failed on I/O
			slog.Info("embedded cover unusable", "path", path, "error", err)
		default:
			slog.Warn("store cover failed", "path", path, "error", err)
			coverRetry = true
		}
	}

	book := storage.Book{
		ContentHash:   hash,
		Title:         meta.Title,
		SortTitle:     sortTitle(meta.Title),
		Publisher:     meta.Publisher,
		PublishedDate: meta.PublishedDate,
		Language:      meta.Language,
		ISBN:          meta.ISBN,
		Description:   meta.Description,
		CoverPath:     coverPath,
		CoverRetry:    coverRetry,
		Format:        bookFormat(suffix),
	}
	_, orphanedID, orphanedTitle, inherited, err = db.CreateBookWithFile(ctx, book, meta.Authors, rel, size, mtime)
	return orphanedID, orphanedTitle, inherited, err
}

type bookMeta struct {
	Title         string
	Authors       []string
	Language      string
	ISBN          string
	Description   string
	Publisher     string
	PublishedDate string
	Cover         []byte
}

// extractMetadata pulls embedded metadata for supported formats and falls
// back to a filename-derived title whenever embedded metadata is
// unavailable, unparseable, or missing a title. epub.Metadata and
// fb2.Metadata are structurally identical but deliberately distinct types
// (no shared interface): there are exactly two implementations, neither is
// chosen at runtime, and an interface here would buy nothing a switch
// doesn't already give.
func extractMetadata(path, suffix string) bookMeta {
	fallbackTitle := filenameTitle(path, suffix)

	switch suffix {
	case ".epub":
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
			Title:         title,
			Authors:       m.Authors,
			Language:      m.Language,
			ISBN:          m.ISBN,
			Description:   m.Description,
			Publisher:     m.Publisher,
			PublishedDate: m.PublishedDate,
			Cover:         m.Cover,
		}
	case ".fb2", ".fb2.zip":
		m, err := fb2.ReadMetadata(path)
		if err != nil {
			slog.Warn("read embedded metadata failed", "path", path, "error", err)
			return bookMeta{Title: fallbackTitle}
		}
		title := m.Title
		if title == "" {
			title = fallbackTitle
		}
		return bookMeta{
			Title:         title,
			Authors:       m.Authors,
			Language:      m.Language,
			ISBN:          m.ISBN,
			Description:   m.Description,
			Publisher:     m.Publisher,
			PublishedDate: m.PublishedDate,
			Cover:         m.Cover,
		}
	default:
		return bookMeta{Title: fallbackTitle}
	}
}

// filenameTitle strips suffix (the matched suffix from matchedSuffix, e.g.
// ".fb2.zip") from path's base name, case-insensitively — filepath.Ext
// would only strip ".zip" from "book.fb2.zip", leaving the fallback title
// "book.fb2", extension and all. Case-insensitive because suffix is always
// the canonical lowercase form while the actual filename may not be.
func filenameTitle(path, suffix string) string {
	base := filepath.Base(path)
	if len(suffix) <= len(base) && strings.EqualFold(base[len(base)-len(suffix):], suffix) {
		return base[:len(base)-len(suffix)]
	}
	return base
}

// sortTitle derives the form a title files under: one leading article
// removed and the rest case folded, so "The Hobbit" sorts under H and
// "apple book" sorts among the A's rather than after every capitalised
// title.
//
// Punctuation and leading digits are deliberately left alone — "'Salem's
// Lot" and "1984" file under "'" and "1", which is the "before A" bucket a
// reader scanning alphabetically expects.
func sortTitle(title string) string {
	return storage.SortTitle(title)
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
