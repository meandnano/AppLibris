package enrich

import (
	"strings"
	"unicode"
)

// normalizeForMatch reduces a title or an author name to comparable
// tokens: case folded, every non-alphanumeric rune treated as a separator,
// empties dropped. Punctuation is a separator rather than something to
// strip, so "Twenty-One" and "Twenty One" agree, and a subtitle's colon
// does not fuse the words either side of it.
func normalizeForMatch(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// containsRun reports whether needle appears in haystack as a contiguous
// run of whole tokens. This is the containment test rather than
// strings.Contains because a substring match is wrong in both directions:
// "it" is inside "italy" as characters and is not a token of it, while
// "the hobbit" really is a run of "the hobbit 75th anniversary edition".
// An empty needle matches nothing — an untitled side cannot corroborate
// anything, and reporting true would make the gate pass on silence.
func containsRun(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slicesEqual(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// titlesMatch reports whether two titles plausibly name the same book:
// equal once normalised, or one a contiguous token run of the other. The
// asymmetric case is the common one — a provider's title carries a
// subtitle, series marker or edition note the file's does not, or the
// reverse.
func titlesMatch(a, b string) bool {
	at, bt := normalizeForMatch(a), normalizeForMatch(b)
	if len(at) == 0 || len(bt) == 0 {
		return false
	}
	return containsRun(at, bt) || containsRun(bt, at)
}

// authorsOverlap reports whether any name on one side equals a name on the
// other, once normalised. Whole-name equality rather than a surname or
// token overlap: "King" appearing in both lists says much less than
// "stephen king" does, and this predicate's only job is to catch an answer
// about a different author entirely.
func authorsOverlap(a, b []string) bool {
	seen := map[string]bool{}
	for _, name := range a {
		if key := strings.Join(normalizeForMatch(name), " "); key != "" {
			seen[key] = true
		}
	}
	for _, name := range b {
		if key := strings.Join(normalizeForMatch(name), " "); key != "" && seen[key] {
			return true
		}
	}
	return false
}

// plausibleMatch reports whether answer is plausibly about the same book as
// the one described by title and authors. It gates Search-sourced answers
// only: an ISBN names one edition, so a ByISBN answer needs no such test.
//
// The rule the whole gate rests on, and the one that is worse than useless
// if inverted: **a title match is required, and author overlap is a veto,
// never a pass.** Resolve calls Search(ctx, book.Title, authors) and both
// providers bind the author into the query, so an author match only
// confirms the provider honoured a constraint this package supplied — it
// says nothing about which of that author's sixty books the ranking put
// first. "Titles match or authors match" would accept any Stephen King
// novel for any Stephen King file.
//
// Author overlap is required only when both sides actually have authors: a
// book with none cannot contradict an answer, and an answer with none —
// common on Open Library edition records, where authorship belongs to the
// work — is silence rather than disagreement.
//
// It rejects most filename-titled books, which is the intended outcome
// rather than a shortfall. Nothing available can establish that a book
// stored as "01 - Fellowship" is The Fellowship of the Ring, and an empty
// field is recoverable — a person can fill it, a later run can try again —
// where a plausible-looking wrong answer is written, provenanced under the
// provider's name, and never reconsidered.
func plausibleMatch(title string, authors []string, answer Metadata) bool {
	if !titlesMatch(title, answer.Title) {
		return false
	}
	if len(authors) > 0 && len(answer.Authors) > 0 && !authorsOverlap(authors, answer.Authors) {
		return false
	}
	return true
}
