package enrich

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// subtitleDelimiters are the punctuation a title uses to hang a subtitle,
// series marker or edition note off itself. They are what separates the two
// cases this file has to tell apart: "The Hobbit: 75th Anniversary Edition"
// extends a title *after a delimiter* and is the same book, while "Dune
// Messiah" extends it with nothing but a space and is a different one.
//
// A hyphen counts only when the separator also contains a space — " - "
// reads as a dash, "Twenty-One" as one compound word.
const subtitleDelimiters = ":;,()[]{}—–/|"

// leadingArticles are dropped from the front of a title before comparing,
// so "Hobbit" and "The Hobbit" agree. One article, English only — the same
// rule and the same reason as storage.SortTitle, which normalises the sort
// column; duplicated rather than imported because that function returns a
// sort key rather than tokens, and the shared thing is the rule, not the
// code.
var leadingArticles = map[string]bool{"the": true, "a": true, "an": true}

// token is one word of a title plus whether a subtitle delimiter stood
// between it and the word before. The flag is the whole point of tokenising
// by hand rather than calling strings.Fields: dropping punctuation throws
// away the one signal that distinguishes a subtitle from more title.
type token struct {
	text string
	// afterDelimiter reports whether a subtitle delimiter separated this
	// token from the previous one. Always false for the first token, which
	// has an edge in front of it rather than a separator.
	afterDelimiter bool
}

// foldForMatch reduces a string to lowercase, diacritic-free form. It
// normalises to NFD and drops combining marks, so decomposed text (macOS
// filenames, which internal/scanner's filenameTitle turns into titles for
// exactly the sparse books that reach Search) compares equal to the
// composed text a provider returns. Folding diacritics away rather than
// preserving them matches books_fts's own `remove_diacritics 2`, so search
// and matching agree about what counts as the same word.
func foldForMatch(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isBoundary reports whether a run of separator runes reads as a subtitle
// delimiter rather than an ordinary space.
func isBoundary(sep string) bool {
	if strings.ContainsAny(sep, subtitleDelimiters) {
		return true
	}
	return strings.ContainsRune(sep, '-') && strings.ContainsAny(sep, " \t\n")
}

// tokenize splits a title into comparable words, recording where subtitle
// delimiters fell. Leading articles are handled by articleVariants rather
// than here, since which form is wanted depends on what it is compared to.
func tokenize(s string) []token {
	folded := foldForMatch(s)

	var (
		tokens  []token
		current strings.Builder
		sep     strings.Builder
	)
	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, token{
			text:           current.String(),
			afterDelimiter: len(tokens) > 0 && isBoundary(sep.String()),
		})
		current.Reset()
		sep.Reset()
	}
	for _, r := range folded {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
			continue
		}
		flush()
		sep.WriteRune(r)
	}
	flush()

	return tokens
}

// articleVariants returns the forms of a title worth comparing: as written,
// and — when it starts with an article — without it.
//
// Both are needed rather than just the stripped one, because an article is
// only redundant at the *edge* of a title. "The Fellowship of the Ring"
// appears inside "The Lord of the Rings: The Fellowship of the Ring" with
// its article intact, so stripping only the outer title would leave the two
// unable to line up.
func articleVariants(tokens []token) [][]token {
	if len(tokens) > 1 && leadingArticles[tokens[0].text] {
		stripped := make([]token, len(tokens)-1)
		copy(stripped, tokens[1:])
		stripped[0].afterDelimiter = false
		return [][]token{tokens, stripped}
	}
	return [][]token{tokens}
}

func sameText(a, b []token) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].text != b[i].text {
			return false
		}
	}
	return true
}

// containsDelimitedRun reports whether needle appears in haystack as a
// contiguous run of whole tokens that is *delimited* on both sides — each
// side either reaching the end of the title or standing behind a subtitle
// delimiter.
//
// Whole tokens rather than a substring, because "it" is inside "italy" as
// characters and is not a word of it. Delimited rather than merely
// contiguous, because that is the difference between a subtitle and a
// sequel: "The Hobbit" is delimited inside "The Hobbit: 75th Anniversary
// Edition" and undelimited inside "The Hobbit Companion", and only the
// first is the same book.
//
// An empty needle matches nothing. Reporting true would make the gate pass
// on silence, which is the opposite of what an untitled side should buy.
func containsDelimitedRun(haystack, needle []token) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if !sameText(haystack[i:i+len(needle)], needle) {
			continue
		}
		leftOK := i == 0 || haystack[i].afterDelimiter
		rightOK := i+len(needle) == len(haystack) || haystack[i+len(needle)].afterDelimiter
		if leftOK && rightOK {
			return true
		}
	}
	return false
}

// titlesMatch reports whether two titles plausibly name the same book:
// equal once folded and stripped of a leading article, or one a delimited
// token run of the other.
//
// The rule it must not weaken to is bare containment. "Dune" is contained
// in "Dune Messiah", "Foundation" in "Foundation and Empire" — and the
// author veto below cannot catch either, because a sequel shares its
// author. One-word and series titles are common among precisely the sparse,
// no-ISBN books that reach the search path, so a containment rule would
// wrongly enrich exactly the population the gate was written for.
func titlesMatch(a, b string) bool {
	at, bt := tokenize(a), tokenize(b)
	if len(at) == 0 || len(bt) == 0 {
		return false
	}
	for _, x := range articleVariants(at) {
		for _, y := range articleVariants(bt) {
			if sameText(x, y) || containsDelimitedRun(x, y) || containsDelimitedRun(y, x) {
				return true
			}
		}
	}
	return false
}

// authorsOverlap reports whether any name on one side equals a name on the
// other, once folded. Whole-name equality rather than a surname or token
// overlap: "King" appearing in both lists says much less than "stephen
// king" does, and this predicate's only job is to catch an answer about a
// different author entirely.
func authorsOverlap(a, b []string) bool {
	seen := map[string]bool{}
	for _, name := range a {
		if key := nameKey(name); key != "" {
			seen[key] = true
		}
	}
	for _, name := range b {
		if key := nameKey(name); key != "" && seen[key] {
			return true
		}
	}
	return false
}

func nameKey(name string) string {
	parts := tokenize(name)
	words := make([]string, 0, len(parts))
	for _, p := range parts {
		words = append(words, p.text)
	}
	return strings.Join(words, " ")
}

// Rejection reasons, reported by plausibleMatch so the log can tell the two
// apart. An author veto shows two titles that look like a fine match and
// says nothing about why it was refused, which is exactly the record a bad
// match has to be diagnosable from.
const (
	reasonTitleMismatch = "title_mismatch"
	reasonAuthorVeto    = "author_veto"
)

// plausibleMatch reports whether answer is plausibly about the same book as
// the one described by title and authors, and why not when it is not. It
// gates Search-sourced answers only: an ISBN names one edition, so a ByISBN
// answer needs no such test.
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
// work — is silence rather than disagreement. Note what this means for a
// sequel, and why titlesMatch has to carry the weight: for "Dune" against
// "Dune Messiah" the author *agrees*, so the veto never fires.
//
// It rejects most filename-titled books, which is the intended outcome
// rather than a shortfall. Nothing available can establish that a book
// stored as "01 - Fellowship" is The Fellowship of the Ring, and an empty
// field is recoverable — a person can fill it, a later run can try again —
// where a plausible-looking wrong answer is written, provenanced under the
// provider's name, and never reconsidered.
func plausibleMatch(title string, authors []string, answer Metadata) (bool, string) {
	if !titlesMatch(title, answer.Title) {
		return false, reasonTitleMismatch
	}
	if len(authors) > 0 && len(answer.Authors) > 0 && !authorsOverlap(authors, answer.Authors) {
		return false, reasonAuthorVeto
	}
	return true, ""
}
