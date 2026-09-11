package importer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// maxStemBytes bounds the part of a library name that comes from the
// upload. It leaves room under every filesystem's own 255-byte limit for
// the suffix, a collision marker and the .part the copy is written through,
// and it is generous enough that no title anyone actually has is cut.
const maxStemBytes = 200

// partSuffix is what a copy in progress is called. Neither the scanner nor
// the watcher acts on a name ending in it — matchedSuffix answers "" — so a
// half-written file is never indexed, and the rename that publishes it is
// atomic.
const partSuffix = ".part"

// maxNameAttempts bounds the collision loop. Reaching it means two hundred
// files already share one name, which is a library nobody has; the bound is
// there so a filesystem lying about O_EXCL cannot spin.
const maxNameAttempts = 200

// errNameExhausted is what the collision loop gives up with.
var errNameExhausted = errors.New("importer: too many files already share that name")

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
// A supported suffix is matched whole, because .fb2.zip is two extensions
// and filepath.Ext sees only the last. Anything else loses one extension,
// which is what a name like Dune.txt deserves; a name whose only dot leads
// it keeps everything, since that dot is stripped as a hidden-file marker
// rather than read as an extension.
func stripSuffix(name, suffix string) string {
	base := name
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	for _, s := range []string{".epub", ".fb2.zip", ".fb2", suffix} {
		if len(s) < len(base) && strings.EqualFold(base[len(base)-len(s):], s) {
			return base[:len(base)-len(s)]
		}
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

// libraryName is the name stem takes with suffix on it, and what the
// preview shows. The collision marker createPart may add is not in it: a
// name is only contested at the moment it is claimed, and a preview that
// promised "Dune (2).epub" for a file imported before anything collided
// would be wrong more often than it was right.
func libraryName(stem, suffix string) string {
	return stem + suffix
}

// createPart claims a name under dir and opens the .part file the copy goes
// into, returning it along with the name it will be renamed to.
//
// Both halves of the claim are needed. The Lstat rules out a name the
// library already holds, which has no .part beside it; the O_EXCL rules out
// a name another confirm is copying into right now, which the Lstat cannot
// see. Together they mean two confirms of different bytes offered under one
// name get two files rather than one truncated one.
func createPart(dir, stem, suffix string) (*os.File, string, error) {
	for n := 1; n <= maxNameAttempts; n++ {
		name := libraryName(stem, suffix)
		if n > 1 {
			name = libraryName(stem+" ("+strconv.Itoa(n)+")", suffix)
		}

		switch _, err := os.Lstat(filepath.Join(dir, name)); {
		case err == nil:
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return nil, "", err
		}

		f, err := os.OpenFile(filepath.Join(dir, name+partSuffix), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		switch {
		case errors.Is(err, fs.ErrExist):
			continue
		case err != nil:
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", errNameExhausted
}
