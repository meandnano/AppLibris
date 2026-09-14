package importer

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"library/internal/scanner"
	"sync"
	"unicode"
)

// maxStemBytes bounds the part of a library name that comes from the
// upload. It leaves room under every filesystem's own 255-byte limit for
// the suffix, a collision marker and the .part the copy is written through,
// and it is generous enough that no title anyone actually has is cut.
const maxStemBytes = 200

// partSuffix is what a copy in progress is called. Neither the scanner nor
// the watcher acts on a name ending in it — scanner.MatchedSuffix answers
// "" — so a half-written file is never indexed, and publish is what gives
// it a name the library holds.
const partSuffix = ".part"

// maxNameAttempts bounds the name search. Reaching it means two hundred
// files already share one name, which is a library nobody has; the bound is
// there so a filesystem lying about O_EXCL or link cannot spin.
const maxNameAttempts = 200

// errNameExhausted is what the name search gives up with.
var errNameExhausted = errors.New("importer: too many files already share that name")

// noHardLinks is logged at most once per process. A filesystem without hard
// links is a property of the deployment, not of the import, so a line per
// imported book would say the same thing forever.
var noHardLinks sync.Once

// libraryStem derives the part of a library filename that comes from the
// upload, without the suffix.
//
// original is a client-supplied string and is treated as one: only a base
// name survives (on either separator, since a browser on Windows offers a
// backslash path), control characters and the separators themselves go,
// a colon goes because it names a stream on some filesystems and a drive on
// others, whitespace collapses to single spaces, and leading dots go so an
// upload cannot write a hidden file. What is left is cut to maxStemBytes on
// a rune boundary.
//
// title is the fallback when nothing survives, and it is sanitised the same
// way rather than trusted: it comes out of an uploaded file's metadata,
// which is no more the app's own text than the filename is. id is the last
// resort, and cannot be empty.
func libraryStem(original, title, id, suffix string) string {
	if stem := sanitizeStem(stripSuffix(original, suffix)); stem != "" {
		return stem
	}
	if stem := sanitizeStem(title); stem != "" {
		return stem
	}
	return id
}

// stripSuffix removes the extension name already carries, so the sniffed
// suffix replaces it rather than being appended to it: an FB2 offered as
// book.epub is written book.fb2, and the preview says so.
//
// A supported suffix is matched whole through scanner.MatchedSuffix, which
// is the one place that decides what this app calls a book file — .fb2.zip
// is two extensions and filepath.Ext sees only the last, and a second copy
// of that list here would be a second answer to drift from it. The sniffed
// suffix is tried after it, for the name a person offered under an
// extension the scanner does not know.
//
// Anything else loses one extension, which is what a name like Dune.txt
// deserves; a name whose only dot leads it keeps everything, since that dot
// is stripped as a hidden-file marker rather than read as an extension.
func stripSuffix(name, suffix string) string {
	base := name
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if s := scanner.MatchedSuffix(base); len(s) > 0 && len(s) < len(base) {
		return base[:len(base)-len(s)]
	}
	if len(suffix) < len(base) && strings.EqualFold(base[len(base)-len(suffix):], suffix) {
		return base[:len(base)-len(suffix)]
	}
	if ext := filepath.Ext(base); len(ext) < len(base) {
		return base[:len(base)-len(ext)]
	}
	return base
}

func sanitizeStem(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '/', r == '\\', r == ':':
			return -1
		case r == unicode.ReplacementChar:
			return -1
		case unicode.IsControl(r):
			return ' '
		default:
			return r
		}
	}, strings.ToValidUTF8(raw, ""))

	cleaned = strings.Join(strings.Fields(cleaned), " ")
	cleaned = strings.TrimLeft(cleaned, ".")
	cleaned = strings.TrimSpace(cleaned)

	if len(cleaned) > maxStemBytes {
		cleaned = strings.ToValidUTF8(cleaned[:maxStemBytes], "")
	}
	// A name ending in a dot or a space is legal here and refused by
	// Windows, which is where a mounted library is read from often enough
	// to be worth not creating one.
	return strings.TrimRight(cleaned, ". ")
}

// libraryName is the name stem takes with suffix on it at attempt n, which
// is 1 for the plain name and counts up through the " (2)", " (3)" markers.
//
// One derivation, used by the claim and by both publish paths, so the name
// a copy is written under and the names it is offered to cannot drift.
// What the preview shows is attempt 1: a name is only contested at the
// moment it is claimed, and promising "Dune (2).epub" for a file imported
// before anything collided would be wrong more often than right.
func libraryName(stem, suffix string, n int) string {
	if n > 1 {
		return stem + " (" + strconv.Itoa(n) + ")" + suffix
	}
	return stem + suffix
}

// claimPart opens the temporary file a copy is written to, and reports its
// path.
//
// The name only has to be free, not final: publish decides what the file
// ends up called, so a claim that took "Dune.epub.part" may well publish as
// "Dune (2).epub". It is named after a candidate anyway, rather than after
// the stage id, because a person looking into the library mid-copy should
// be able to see what is arriving.
func claimPart(dir, stem, suffix string) (*os.File, string, error) {
	for n := 1; n <= maxNameAttempts; n++ {
		path := filepath.Join(dir, libraryName(stem, suffix, n)+partSuffix)

		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		switch {
		case errors.Is(err, fs.ErrExist):
			continue
		case err != nil:
			return nil, "", err
		}
		return f, path, nil
	}
	return nil, "", errNameExhausted
}

// publish gives the completed part file its library name and reports the
// name it got. It is the only thing in this package that creates a
// supported suffix in the library directory.
//
// os.Link is the test-and-set, and that is the whole point of it: it fails
// fs.ErrExist rather than replacing, where os.Rename silently destroys
// whatever is at the name. docs/notes/design.md's rule for the library
// directory is that writes only ever create new paths, and the seconds a
// large copy takes are long enough for something else to have created this
// one — a person dropping a file into the pile they manage by hand is the
// ordinary case, not an exotic one.
//
// A name taken since the claim costs a link attempt and nothing else: the
// part file already holds the bytes, so the next candidate is linked from
// the same data rather than copied again.
func publish(dir, part, stem, suffix string) (string, error) {
	for n := 1; n <= maxNameAttempts; n++ {
		name := libraryName(stem, suffix, n)

		err := os.Link(part, filepath.Join(dir, name))
		switch {
		case errors.Is(err, fs.ErrExist):
			continue
		case err == nil:
			// The part is now a second name for bytes the library already
			// holds under the first, so unlinking it publishes nothing and
			// loses nothing.
			os.Remove(part)
			return name, nil
		}

		// Not every filesystem has hard links: exFAT, some SMB mounts and
		// a few volume drivers refuse. Refusing to import there would
		// break a working deployment over a window that only opens when
		// something else writes the same name mid-copy, so it falls back
		// to the replacing primitive and says so once.
		noHardLinks.Do(func() {
			slog.Warn("the library filesystem does not support hard links, so an import is published by rename, which replaces whatever is at the name it lands on",
				"dir", dir, "error", err)
		})
		return renamePublish(dir, part, stem, suffix)
	}
	return "", errNameExhausted
}

// renamePublish is publish without the atomic test-and-set, for a
// filesystem that offers none. The Lstat narrows the window; nothing here
// can close it.
func renamePublish(dir, part, stem, suffix string) (string, error) {
	for n := 1; n <= maxNameAttempts; n++ {
		name := libraryName(stem, suffix, n)
		path := filepath.Join(dir, name)

		switch _, err := os.Lstat(path); {
		case err == nil:
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
		if err := os.Rename(part, path); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errNameExhausted
}
