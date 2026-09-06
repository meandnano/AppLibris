package storage

import (
	"strings"
	"unicode"
)

const (
	// isbn10Length and isbn13Length are what a complete ISBN measures once
	// hyphens and spaces are stripped, ISBN-10's trailing X counted as one
	// of its ten characters.
	isbn10Length = 10
	isbn13Length = 13

	// minPartialISBNHyphens is what keeps "1984-2001" a title query. A
	// hyphenated ISBN has at least four groups — prefix, registration
	// group, registrant, publication, check digit — so by the time one is
	// far enough through being typed to be worth prefix-matching it carries
	// two separators, while a two-group numeric query is a date range, a
	// year pair or a volume number far more often than an identifier.
	minPartialISBNHyphens = 2

	// minPartialISBNDigits keeps a short three-group number — "9-1-1",
	// "1-2-3", both of which title books — a title query, which the hyphen
	// count alone does not. It costs the ISBN path nothing: four digits is
	// what "978-0-" already carries, and no shorter prefix filters a
	// library down to anything worth looking at.
	minPartialISBNDigits = 4
)

// SanitizeFTSQuery turns raw user input into a valid FTS5 MATCH expression.
// Raw input is almost never valid FTS5 query syntax on its own — a stray
// `"`, `(`, `-` or a bare AND makes MATCH return an error — so every
// whitespace-separated token is escaped, quoted and turned into a prefix
// term, then joined with an implicit AND: "har pot" becomes `"har"*
// "pot"*`, matching "Harry Potter" while it's still being typed, in either
// word order. Because every token is quoted, none of FTS5's own operator
// syntax can reach MATCH unescaped, so no input — however adversarial —
// produces an expression FTS5 rejects. Control characters (a raw NUL from
// "?q=%00", say) are stripped before anything else: quoting alone doesn't
// neutralize them, and an embedded NUL inside an otherwise-valid quoted
// string still makes SQLite's FTS5 parser reject it as an unterminated
// string, which is exactly the kind of input error this function exists to
// rule out.
//
// A query about an ISBN is normalized to bare digits instead of taking that
// per-word path — see normalizeIfISBNShaped for the two shapes that
// qualify. internal/epub normalizes a stored ISBN to bare digits, but
// internal/fb2 does not, so the isbn column can hold either "9780857059985"
// or "978-0-85705-998-5" depending on which parser found it. Matching a
// query shaped either way against storage shaped either way needs both
// sides normalized the same way — see syncBookFTSTx for the index-side half
// of this.
//
// An unpunctuated partial ISBN ("978085" on the way to the full one) needs
// neither shape: it is one token of digits, so the per-word path below
// already quotes it into the same prefix term the ISBN path would produce.
//
// Input with no non-whitespace content returns "", which callers treat as
// "no search" (the full list) rather than a query that matches nothing.
// So does input that is entirely control characters, since those are
// stripped above — "no search" there too, not a search for nothing.
func SanitizeFTSQuery(input string) string {
	input = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, input)

	if isbn, ok := normalizeIfISBNShaped(input); ok {
		return `"` + isbn + `"*`
	}

	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}

	terms := make([]string, len(fields))
	for i, f := range fields {
		escaped := strings.ReplaceAll(f, `"`, `""`)
		terms[i] = `"` + escaped + `"*`
	}
	return strings.Join(terms, " ")
}

// normalizeIfISBNShaped reports whether input is a query about an ISBN, and
// its bare-digit form if so: either a complete one however punctuated —
// what a paste produces — or a hyphenated one partway through being typed.
// Each shape is defined by the function that implements it.
//
// Which is tried first is immaterial rather than load-bearing: an input
// both accept is all digits and hyphens, so both return the same string.
// What matters is that the complete shape is tried at all, since it alone
// accepts a trailing X, a space-separated ISBN, and one written with fewer
// hyphens than a partial needs ("978085705-9985").
func normalizeIfISBNShaped(input string) (string, bool) {
	if isbn, ok := completeISBNShaped(input); ok {
		return isbn, true
	}
	return partialISBNShaped(input)
}

// completeISBNShaped accepts a whole ISBN in any punctuation: 10 or 13
// characters of digits with an optional trailing X once hyphens and spaces
// are stripped — the same shape internal/epub's own bare-ISBN detection
// accepts — returned stripped and upper-cased.
func completeISBNShaped(input string) (string, bool) {
	stripped := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(input))
	if len(stripped) != isbn10Length && len(stripped) != isbn13Length {
		return "", false
	}
	for i, r := range stripped {
		if r >= '0' && r <= '9' {
			continue
		}
		if (r == 'x' || r == 'X') && i == len(stripped)-1 {
			continue
		}
		return "", false
	}
	return strings.ToUpper(stripped), true
}

// partialISBNShaped accepts a hyphenated ISBN still being typed, so that the
// results stop going empty from the second hyphen onward. Digits and
// hyphens only, within the bounds the constants above set: no more digits
// than a whole ISBN-13 has, and enough hyphens and digits that the query is
// an identifier rather than a number a title happens to contain. A trailing
// hyphen is accepted, being what is on screen halfway between two groups.
//
// The cost this shape does not avoid is a three-group number long enough to
// clear the digit floor — an ISO-style date, "2026-09-06", is read as an
// ISBN prefix and no longer finds a title carrying it. Telling that from an
// identifier needs the number's meaning rather than its punctuation, and
// the population this exists for types the hyphens printed on a book.
//
// A space is deliberately not accepted here, where the complete shape does
// strip one: the space is the token separator for the entire rest of the
// search box, so a pair of hyphenated groups typed either side of one
// ("1984-85 2000-01") would collapse into a single twelve-digit query
// instead of the two terms the person typed.
func partialISBNShaped(input string) (string, bool) {
	var digits strings.Builder
	hyphens := 0
	for _, r := range strings.TrimSpace(input) {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '-':
			hyphens++
		default:
			return "", false
		}
	}
	if hyphens < minPartialISBNHyphens {
		return "", false
	}
	// The cap only ever refuses fourteen digits and up: a 13-digit query of
	// digits and hyphens strips to a complete ISBN, so completeISBNShaped
	// has already taken it and nothing reaching here carries more than
	// twelve.
	if digits.Len() < minPartialISBNDigits || digits.Len() > isbn13Length {
		return "", false
	}
	return digits.String(), true
}
