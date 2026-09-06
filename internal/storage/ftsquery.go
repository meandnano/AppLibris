package storage

import (
	"strings"
	"unicode"
)

const (
	// maxISBNDigits is ISBN-13's length. A partial query carrying more
	// digits than a whole ISBN has is not an ISBN being typed, whatever its
	// punctuation.
	maxISBNDigits = 13
	// minPartialISBNHyphens is what keeps "1984-2001" a title query. A
	// hyphenated ISBN has at least four groups — prefix, registration
	// group, registrant, publication, check digit — so by the time one is
	// far enough through being typed to be worth prefix-matching it carries
	// two separators, while a two-group numeric query is a date range, a
	// year pair or a volume number far more often than an identifier.
	minPartialISBNHyphens = 2
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
// qualify. internal/epub normalizes a stored ISBN to bare digits but
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
// its bare-digit form if so. Two shapes qualify:
//
//   - a complete ISBN however punctuated, which is what a paste produces:
//     exactly 10 or 13 characters once hyphens and spaces are stripped, a
//     trailing X permitted;
//   - a hyphenated ISBN partway through being typed: digits and hyphens
//     only, at least minPartialISBNHyphens of them, no more than
//     maxISBNDigits digits in total.
//
// The complete shape is tried first, since it is the only one that accepts
// the check character and the space-separated form.
func normalizeIfISBNShaped(input string) (string, bool) {
	input = strings.TrimSpace(input)
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
	stripped := strings.NewReplacer("-", "", " ", "").Replace(input)
	if len(stripped) != 10 && len(stripped) != 13 {
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
// results don't go empty between the first character and the last.
//
// A space is deliberately not accepted here, where the complete shape does
// strip one: the space is the token separator for the entire rest of the
// search box, so reading "1984 2001" as one seventeen-digit number would
// break a legitimate two-term query to serve an input nobody produces — a
// person typing an ISBN by hand types the hyphens printed on the book. A
// trailing hyphen is accepted, being what the same person has on screen
// halfway between two groups.
func partialISBNShaped(input string) (string, bool) {
	var digits strings.Builder
	hyphens := 0
	for _, r := range input {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '-':
			hyphens++
		default:
			return "", false
		}
	}
	if hyphens < minPartialISBNHyphens || digits.Len() == 0 || digits.Len() > maxISBNDigits {
		return "", false
	}
	return digits.String(), true
}
