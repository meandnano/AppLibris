package enrich

import (
	"strings"
	"testing"
)

func TestTitlesMatch(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "The Hobbit", "The Hobbit", true},
		{"case differs", "the hobbit", "THE HOBBIT", true},
		{"punctuation differs", "Twenty-One Balloons", "Twenty One Balloons", true},
		{"answer carries a subtitle", "The Hobbit", "The Hobbit: 75th Anniversary Edition", true},
		{"book carries a subtitle", "The Hobbit, or There and Back Again", "The Hobbit", true},
		{"cyrillic, answer longer", "Хоббит", "Хоббит, или Туда и обратно", true},
		{"parenthesised edition note", "The Hobbit", "The Hobbit (75th Anniversary Edition)", true},
		{"spaced hyphen reads as a dash", "The Hobbit", "The Hobbit - Illustrated", true},
		{"leading article dropped", "Hobbit", "The Hobbit", true},
		{"article a dropped", "Wizard of Earthsea", "A Wizard of Earthsea", true},
		{"article an dropped", "Wrinkle in Time", "An Wrinkle in Time", true},

		// The Russian "Series. Title" convention, which DESIGN.md names as
		// the population the title search exists for. Without the period the
		// colon form matched and this one did not.
		{"russian series convention", "Властелин колец", "Властелин колец. Братство кольца", true},
		{"russian series, volume side", "Братство кольца", "Властелин колец. Братство кольца", true},
		{"question mark ends a segment", "Who's Afraid of Virginia Woolf", "Who's Afraid of Virginia Woolf? A Play", true},
		// The cost of the period, accepted and pinned: an abbreviation
		// splits a title. Same over-match class the design accepts
		// elsewhere, and the author veto still applies.
		{"abbreviation splits — accepted cost", "No", "Dr. No", true},
		// An unspaced period does not split, so initials stay one word.
		{"initials are not a boundary", "J.R.R. Tolkien A Biography", "J.R.R. Tolkien A Biography", true},
		// maxSegments still refuses a period-separated contents list.
		{"period-separated contents list", "Гамлет", "Шекспир. Гамлет. Отелло", false},
		{"different books", "The Hobbit", "The Silmarillion", false},
		{"substring but not a token run", "It", "Italy", false},
		{"token run must be contiguous", "Fellowship Ring", "The Fellowship of the Ring", false},
		{"filename-shaped title", "01 - Fellowship", "The Fellowship of the Ring", false},
		{"empty answer title", "The Hobbit", "", false},
		{"empty book title", "", "The Hobbit", false},
		{"both empty", "", "", false},
		{"punctuation only", "The Hobbit", "—", false},

		// A sequel extends a title with nothing but a space, where a
		// subtitle stands behind a delimiter. The author veto cannot catch
		// any of these, because a sequel shares its author — so this rule
		// carries the whole weight, and one-word and series titles are
		// common among exactly the sparse books that reach this path.
		{"sequel, one-word title", "Dune", "Dune Messiah", false},
		{"sequel, multi-word title", "Foundation", "Foundation and Empire", false},
		{"undelimited extension", "The Hobbit", "The Hobbit Companion", false},
		{"bare article", "The", "The Hobbit", false},

		// The match need not be the first segment. Google Books routinely
		// answers with a series-prefixed title, and the book's own name is
		// then the last segment.
		{"series-prefixed answer", "The Fellowship of the Ring", "The Lord of the Rings: The Fellowship of the Ring", true},
		{"undelimited suffix", "Ring", "The Fellowship of the Ring", false},

		// An article is redundant at the edge of a *segment*, not only at
		// the edge of a title — a filename-derived title routinely drops
		// one while the answer keeps it inside a longer title.
		{"article dropped inside a segment", "Fellowship of the Ring", "The Lord of the Rings: The Fellowship of the Ring", true},
		{"article dropped, series-prefixed", "Two Towers", "The Lord of the Rings: The Two Towers", true},

		// A contents list is not a subtitle. Each of these is a whole
		// delimited segment of the answer, and the answer is a different
		// book — a collected edition, whose author agrees, so the veto
		// cannot catch it either.
		{"omnibus, first item", "Hamlet", "Shakespeare: Hamlet, Othello, Macbeth", false},
		{"omnibus, last item", "Macbeth", "Shakespeare: Hamlet, Othello, Macbeth", false},
		{"omnibus, interior item", "Othello", "Shakespeare: Hamlet, Othello, Macbeth", false},
		{"three-segment companion volume", "The Two Towers", "Tolkien: The Two Towers: A Reader's Guide", false},

		// The known limit, pinned so a change to it is deliberate: a
		// two-segment answer whose second part describes rather than names
		// the book still matches. Telling an edition note from a companion
		// volume needs the words' meaning, not their punctuation.
		{"two-segment study guide — accepted, known limit", "The Hobbit", "The Hobbit: A Study Guide", true},

		// Decomposed text compares equal to composed text: macOS filenames
		// are NFD, and filenameTitle turns them into titles for exactly the
		// no-ISBN books that reach the search path.
		{"NFD against NFC", "Cafe\u0301 Society", "Café Society", true},
		{"diacritics folded", "Cafe Society", "Café Society", true},
		{"decomposed mid-word", "Cafe\u0301s", "Cafés", true},
		// Word-medial, against the unaccented spelling — the case that
		// actually pins the combining-mark strip. The cases above fold
		// symmetrically on both sides, so they survive its removal.
		{"mark mid-word against unaccented", "Motorhead", "Motörhead", true},
		{"mark mid-word, accented only one side", "Naive", "Naïve", true},
		// Case folding, not lowercasing: Greek written natively ends a word
		// with a final sigma, which ToLower never produces.
		{"greek final sigma", "ΟΔΟΣ", "οδός", true},
		{"eszett folds to ss", "Strasse", "Straße", true},
		// Indic vowel signs are spacing marks, and splitting on them would
		// shred a title into one-letter fragments.
		{"devanagari stays one token", "किताब", "किताब", true},
	}
	for _, c := range cases {
		if got := titlesMatch(c.a, c.b); got != c.want {
			t.Errorf("%s: titlesMatch(%q, %q) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func TestAuthorsOverlap(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"single match", []string{"J.R.R. Tolkien"}, []string{"J.R.R. Tolkien"}, true},
		{"punctuation and case differ", []string{"j r r tolkien"}, []string{"J.R.R. Tolkien"}, true},
		{"one of several matches", []string{"Neil Gaiman", "Terry Pratchett"}, []string{"Terry Pratchett"}, true},
		{"no overlap", []string{"Neil Gaiman"}, []string{"Terry Pratchett"}, false},
		{"surname alone is not a match", []string{"Tolkien"}, []string{"J.R.R. Tolkien"}, false},
		{"empty left", nil, []string{"J.R.R. Tolkien"}, false},
		{"empty right", []string{"J.R.R. Tolkien"}, nil, false},
		{"both empty", nil, nil, false},
		{"blank names never match each other", []string{"  "}, []string{"—"}, false},
	}
	for _, c := range cases {
		if got := authorsOverlap(c.a, c.b); got != c.want {
			t.Errorf("%s: authorsOverlap(%v, %v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

// The composition, and specifically that author overlap is a veto rather
// than an alternative. The two cases that would flip under an "or" rule are
// named: a title mismatch with overlapping authors must still be rejected
// (any Stephen King novel for any Stephen King file), and a title match
// with disjoint authors must be rejected too.
func TestPlausibleMatch(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		authors []string
		answer  Metadata
		want    bool
		reason  string
	}{
		{
			name:   "title matches, neither side has authors",
			title:  "The Hobbit",
			answer: Metadata{Title: "The Hobbit"},
			want:   true,
		},
		{
			name:    "title matches, authors overlap",
			title:   "The Hobbit",
			authors: []string{"J.R.R. Tolkien"},
			answer:  Metadata{Title: "The Hobbit", Authors: []string{"J.R.R. Tolkien"}},
			want:    true,
		},
		{
			name:    "title matches, answer has no authors — silence, not disagreement",
			title:   "The Hobbit",
			authors: []string{"J.R.R. Tolkien"},
			answer:  Metadata{Title: "The Hobbit"},
			want:    true,
		},
		{
			name:   "title matches, book has no authors",
			title:  "The Hobbit",
			answer: Metadata{Title: "The Hobbit", Authors: []string{"J.R.R. Tolkien"}},
			want:   true,
		},
		{
			name:    "title matches, authors disjoint — the veto",
			title:   "The Hobbit",
			authors: []string{"J.R.R. Tolkien"},
			answer:  Metadata{Title: "The Hobbit", Authors: []string{"Terry Pratchett"}},
			want:    false,
			reason:  reasonAuthorVeto,
		},
		{
			name:    "title mismatch, authors overlap — must not pass",
			title:   "The Shining",
			authors: []string{"Stephen King"},
			answer:  Metadata{Title: "Pet Sematary", Authors: []string{"Stephen King"}},
			want:    false,
			reason:  reasonTitleMismatch,
		},
		{
			name:   "filename title against a real one",
			title:  "01 - Fellowship",
			answer: Metadata{Title: "The Fellowship of the Ring"},
			want:   false,
			reason: reasonTitleMismatch,
		},
		{
			// A list of unusable names is silence wearing punctuation, the
			// same as no list at all — it must not be able to veto a title
			// that matches.
			name:    "book's authors all fold away — silence, not disagreement",
			title:   "The Hobbit",
			authors: []string{"—", "  "},
			answer:  Metadata{Title: "The Hobbit", Authors: []string{"J.R.R. Tolkien"}},
			want:    true,
		},
		{
			name:    "answer's authors all fold away",
			title:   "The Hobbit",
			authors: []string{"J.R.R. Tolkien"},
			answer:  Metadata{Title: "The Hobbit", Authors: []string{"", "  "}},
			want:    true,
		},
		{
			name:   "answer with no title at all",
			title:  "The Hobbit",
			answer: Metadata{Publisher: "Allen & Unwin"},
			want:   false,
			reason: reasonTitleMismatch,
		},
	}
	for _, c := range cases {
		got, reason := plausibleMatch(c.title, c.authors, c.answer)
		if got != c.want {
			t.Errorf("%s: plausibleMatch(%q, %v, %+v) = %v, want %v", c.name, c.title, c.authors, c.answer, got, c.want)
		}
		if got && reason != "" {
			t.Errorf("%s: accepted with reason %q, want empty", c.name, reason)
		}
		if !got && reason != c.reason {
			t.Errorf("%s: reason = %q, want %q", c.name, reason, c.reason)
		}
	}
}

// The two rejection reasons have to stay distinguishable. TestPlausibleMatch
// compares the returned reason against these same constants, so collapsing
// them to one string leaves it green while making the log unable to tell a
// title mismatch from an author veto — which is the whole point of carrying
// a reason at all.
func TestRejectionReasonsAreDistinct(t *testing.T) {
	if reasonTitleMismatch == reasonAuthorVeto {
		t.Fatalf("both reasons are %q; a veto must be distinguishable from a mismatch", reasonTitleMismatch)
	}
	if reasonTitleMismatch == "" || reasonAuthorVeto == "" {
		t.Error("a rejection reason is empty; an accepted match is what reports no reason")
	}
}

func TestMetadataIsEmpty(t *testing.T) {
	if !(Metadata{}).IsEmpty() {
		t.Error("zero Metadata is not reported empty")
	}
	// A cover-only answer is an answer — Resolve's ISBN fallback keys on
	// this, and treating it as empty would search past a cover a catalogue
	// matched on an identifier.
	if (Metadata{CoverURL: "https://covers.example/1.jpg"}).IsEmpty() {
		t.Error("a cover-only answer is reported empty; it is an answer")
	}
	for _, m := range []Metadata{
		{Title: "x"}, {Authors: []string{"x"}}, {Publisher: "x"},
		{PublishedDate: "x"}, {Language: "x"}, {ISBN: "x"}, {Description: "x"},
	} {
		if m.IsEmpty() {
			t.Errorf("%+v reported empty", m)
		}
	}
}

// segmentsOf's own contracts, pinned where they are stated rather than only
// through titlesMatch: a title with no words yields no segments, and one
// past maxTitleTokens is refused outright rather than compared.
func TestSegmentsOfContracts(t *testing.T) {
	if got := segmentsOf(""); got != nil {
		t.Errorf("segmentsOf(\"\") = %v, want nil", got)
	}
	if got := segmentsOf(" — : , "); got != nil {
		t.Errorf("punctuation-only title yielded %v, want nil", got)
	}
	if got := segmentsOf("The Hobbit: 75th Anniversary Edition"); len(got) != 2 {
		t.Errorf("segments = %v, want 2", got)
	}

	// The matcher is quadratic, runs on a provider's raw title, and has no
	// ctx to notice a shutdown — so an absurd title is refused rather than
	// compared. A false negative on nothing that exists.
	long := strings.Repeat("word ", maxTitleTokens+1)
	if got := segmentsOf(long); got != nil {
		t.Errorf("a %d-word title yielded %d segments, want nil", maxTitleTokens+1, len(got))
	}
	if titlesMatch(long, long) {
		t.Error("two identical over-long titles matched; they must be refused")
	}
	if got := segmentsOf(strings.Repeat("word ", maxTitleTokens)); len(got) != 1 {
		t.Errorf("a title exactly at the cap yielded %d segments, want 1", len(got))
	}

	// Spacing combining marks continue a word. Asserted on the token count
	// rather than through titlesMatch, because a title compared against
	// itself shreds identically on both sides and matches either way — the
	// same symmetry that hid the combining-mark strip. What shredding
	// actually costs is one-letter fragments colliding across unrelated
	// titles.
	for _, word := range []string{"किताब", "গীতাঞ্জলি", "தமிழ்"} {
		got := segmentsOf(word)
		if len(got) != 1 || len(got[0]) != 1 {
			t.Errorf("segmentsOf(%q) = %v, want one segment of one word", word, got)
		}
	}
}
