package enrich

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// subtitleDelimiters are the punctuation a title uses to hang a subtitle,
// series marker or edition note off itself. They are what separates the two
// cases this file has to tell apart: "The Hobbit: 75th Anniversary Edition"
// extends a title *after a delimiter* and is the same book, while "Dune
// Messiah" extends it with nothing but a space and is a different one.
const subtitleDelimiters = ":;,()[]{}—–/|"

// spacedDelimiters do the same job, but only when the separator also
// carries whitespace — they all have a second, word-internal meaning that a
// bare occurrence usually intends. " - " is a dash and "Twenty-One" a
// compound; ". " ends a segment and "J.R.R." does not.
//
// The period earns its place from the population this path serves: Russian
// editions, which docs/notes/enrichment.md names as the reason the title search exists,
// conventionally write "Series. Title" — without it "Властелин колец" could
// never match "Властелин колец. Братство кольца", where the equivalent
// colon form already matches. The cost is an abbreviation splitting a
// title, so "No" matches "Dr. No" and "Dalloway" matches "Mrs. Dalloway".
// Both are the over-match this design already accepts elsewhere (a
// delimited segment matching a whole title is the same rule that makes
// "The Hobbit" match "The Hobbit, or There and Back Again"), the author
// veto still applies, and maxSegments still refuses a period-separated
// contents list. Recorded rather than left implicit, so a reader can tell
// this was decided.
const spacedDelimiters = "-.?!"

// maxSegments bounds how many delimited parts an answer may have and still
// match on one of them. Two — the title, and at most one subtitle, series
// marker or edition note.
//
// It is what stops a collected edition matching any of its contents: split
// "Shakespeare: Hamlet, Othello, Macbeth" on its delimiters and "Hamlet" is
// a whole segment of it, so a rule that asked only for a delimited segment
// would hand a Hamlet the omnibus's publisher, date, description and cover.
// The author veto cannot help, for the same reason it cannot help with a
// sequel: a collection shares its author. A contents list is not a subtitle,
// and counting segments is what tells them apart.
const maxSegments = 2

// maxTitleTokens bounds the work one comparison can do. The matcher is
// quadratic in token count and runs on a provider's *raw* title, before
// sanitizeValue caps anything — a provider client bounds only the whole
// response, at megabytes. A real title is a few dozen words, so anything
// past this is not a title and is refused rather than compared, which costs
// a false negative on nothing that exists.
const maxTitleTokens = 64

// leadingArticles may be dropped from the front of any segment before
// comparing it, so "Hobbit" and "The Hobbit" agree. One article, English
// only — the same rule and the same reason as storage.SortTitle, which
// normalises the sort column; duplicated rather than imported because that
// function returns a sort key rather than tokens, and the shared thing is
// the rule, not the code.
var leadingArticles = map[string]bool{"the": true, "a": true, "an": true}

// fold reduces a string to a case- and diacritic-insensitive form.
//
// cases.Fold rather than strings.ToLower, because lowercasing is not
// case-folding where it matters: ToLower maps Σ to σ unconditionally, while
// Greek written natively ends a word with ς, so "ΟΔΟΣ" and "οδός" would
// never agree. Folding also settles ß against ss.
//
// NFD then dropping combining marks makes decomposed text compare equal to
// composed text — macOS filenames are NFD, and internal/scanner's
// filenameTitle turns them into titles for exactly the sparse books that
// reach the search path. It also folds diacritics away entirely, matching
// books_fts's own remove_diacritics 2, so search and matching agree about
// what counts as the same word.
func fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(cases.Fold().String(s)) {
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
	return strings.ContainsAny(sep, spacedDelimiters) && strings.ContainsAny(sep, " \t\n")
}

// isWordRune reports whether r continues a word. Marks count: Mn is already
// gone by the time this runs, but Mc and Me — the spacing combining marks
// Indic scripts use for vowel signs — are neither letters nor digits, and
// treating them as separators shreds "किताब" into three one-letter tokens
// that then collide with fragments of unrelated titles.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.In(r, unicode.M)
}

// segmentsOf splits a title into its delimiter-separated parts, each a list
// of comparable words. It reports nil for a title carrying no words at all,
// and for one past maxTitleTokens — see that constant.
func segmentsOf(s string) [][]string {
	var (
		segments [][]string
		current  []string
		word     strings.Builder
		sep      strings.Builder
		total    int
	)
	// flushWord ends the word in hand, starting a new segment first when the
	// separator that preceded it was a subtitle delimiter. It reports false
	// once the title has more words than are worth comparing.
	flushWord := func() bool {
		if word.Len() == 0 {
			return true
		}
		if len(current) > 0 && isBoundary(sep.String()) {
			segments = append(segments, current)
			current = nil
		}
		current = append(current, word.String())
		total++
		word.Reset()
		sep.Reset()
		return total <= maxTitleTokens
	}

	for _, r := range fold(s) {
		if isWordRune(r) {
			word.WriteRune(r)
			continue
		}
		if !flushWord() {
			return nil
		}
		sep.WriteRune(r)
	}
	if !flushWord() {
		return nil
	}
	if len(current) > 0 {
		segments = append(segments, current)
	}
	return segments
}

func sameWords(a, b []string) bool {
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

// withoutArticle drops a leading article, leaving a segment that is nothing
// but an article alone — "The" is not a title.
func withoutArticle(seg []string) []string {
	if len(seg) > 1 && leadingArticles[seg[0]] {
		return seg[1:]
	}
	return seg
}

// segmentsMatch reports whether two segments name the same thing, allowing
// either to carry a leading article the other does not.
//
// Per segment rather than per title, which is the part that is easy to get
// wrong: an article is redundant at the edge of a *segment*, and a segment
// is not always at the edge of its title. "The Fellowship of the Ring"
// appears inside "The Lord of the Rings: The Fellowship of the Ring" with
// its own article intact, so stripping only the outer title leaves an
// article-less stored title — the ordinary shape when a title came from a
// filename — unable to line up.
func segmentsMatch(a, b []string) bool {
	return sameWords(withoutArticle(a), withoutArticle(b))
}

// segmentMatch reports whether a one-segment title names the same book as a
// title of at most maxSegments parts, by matching any one of them.
//
// Any one of them, rather than only the first or last, because maxSegments
// is what does the work: with at most two parts every part already touches
// an end, so a first-or-last test would be a branch no input could take. It
// is the segment *count* that separates a subtitle from a contents list,
// and stating the rule twice would leave one copy untestable.
func segmentMatch(single, whole [][]string) bool {
	if len(single) != 1 || len(whole) == 0 || len(whole) > maxSegments {
		return false
	}
	for _, seg := range whole {
		if segmentsMatch(single[0], seg) {
			return true
		}
	}
	return false
}

// titlesMatch reports whether two titles plausibly name the same book.
//
// The rule it must not weaken to is bare containment. "Dune" is contained
// in "Dune Messiah", "Foundation" in "Foundation and Empire" — and the
// author veto cannot catch either, because a sequel shares its author.
// One-word and series titles are common among precisely the sparse,
// no-ISBN books that reach the search path, so a containment rule would
// wrongly enrich exactly the population the gate was written for.
//
// A known limit, recorded so it is not rediscovered as a surprise: a
// two-segment answer whose second part describes the book rather than
// naming it still matches — "The Hobbit" accepts "The Hobbit: A Study
// Guide", a different book with its own publisher and cover. Telling an
// edition note from a companion volume needs to know what the words mean
// rather than how they are punctuated, and no cheap rule reaches it.
// Three-segment forms of the same shape ("Tolkien: The Hobbit: A Reader's
// Guide") are refused by maxSegments.
func titlesMatch(a, b string) bool {
	as, bs := segmentsOf(a), segmentsOf(b)
	if len(as) == 0 || len(bs) == 0 {
		return false
	}
	if len(as) == len(bs) {
		same := true
		for i := range as {
			if !segmentsMatch(as[i], bs[i]) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return segmentMatch(as, bs) || segmentMatch(bs, as)
}

// nameKey folds an author's name to its comparable form, or "" when the
// name carries no words at all.
func nameKey(name string) string {
	segments := segmentsOf(name)
	words := make([]string, 0, len(segments))
	for _, seg := range segments {
		words = append(words, seg...)
	}
	return strings.Join(words, " ")
}

// usableNames counts the names carrying anything comparable. A list of
// blanks or punctuation is silence, not a claim about authorship, and must
// not be able to veto — see plausibleMatch.
func usableNames(names []string) int {
	n := 0
	for _, name := range names {
		if nameKey(name) != "" {
			n++
		}
	}
	return n
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
// The veto needs *usable* names on both sides, not merely present ones: a
// book with no authors cannot contradict an answer, an answer with none —
// common on Open Library edition records, where authorship belongs to the
// work — is silence rather than disagreement, and a list that folds away to
// nothing is the same silence wearing punctuation. Note what this means for
// a sequel or a collected edition, and why titlesMatch has to carry the
// weight: their author *agrees*, so the veto never fires.
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
	if usableNames(authors) > 0 && usableNames(answer.Authors) > 0 &&
		!authorsOverlap(authors, answer.Authors) {
		return false, reasonAuthorVeto
	}
	return true, ""
}
