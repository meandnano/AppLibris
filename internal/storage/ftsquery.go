package storage

import (
	"strings"
	"unicode"
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
// Input consisting entirely of digits, hyphens and spaces, 10 or 13 digits
// once separators are stripped (a trailing X permitted, ISBN-10's check
// character), is treated as an ISBN rather than run through the normal
// per-word path: internal/epub normalizes a stored ISBN to bare digits,
// but internal/fb2 does not, so the isbn column can hold either
// "9780857059985" or "978-0-85705-998-5" depending on which parser found
// it. Matching a query shaped either way against storage shaped either
// way needs both sides normalized the same way — see syncBookFTSTx for
// the index-side half of this.
//
// Input with no non-whitespace content returns "", which callers treat as
// "no search" (the full list) rather than a query that matches nothing.
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

// normalizeIfISBNShaped reports whether input, stripped of hyphens and
// spaces, is 10 or 13 characters of digits with an optional trailing X —
// the same shape internal/epub's own bare-ISBN detection accepts — and if
// so returns that stripped, upper-cased form.
func normalizeIfISBNShaped(input string) (string, bool) {
	stripped := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(input))
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
