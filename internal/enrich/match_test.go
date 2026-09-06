package enrich

import "testing"

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

		// A run need not be a prefix. Google Books routinely answers with a
		// series-prefixed title, and the run is then delimited on its left
		// and reaches the end on its right.
		{"delimited suffix run", "The Fellowship of the Ring", "The Lord of the Rings: The Fellowship of the Ring", true},
		{"delimited interior run", "The Two Towers", "Tolkien: The Two Towers: A Reader's Guide", true},
		{"undelimited suffix run", "Ring", "The Fellowship of the Ring", false},

		// Decomposed text compares equal to composed text: macOS filenames
		// are NFD, and filenameTitle turns them into titles for exactly the
		// no-ISBN books that reach the search path.
		{"NFD against NFC", "Cafe\u0301 Society", "Café Society", true},
		{"diacritics folded", "Cafe Society", "Café Society", true},
		{"decomposed mid-word", "Cafe\u0301s", "Cafés", true},
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

// containsDelimitedRun's own contract, pinned where it is stated rather
// than only through titlesMatch: an empty needle matches nothing, since
// reporting true would make the gate pass on silence.
func TestContainsDelimitedRunEmptyNeedle(t *testing.T) {
	haystack := tokenize("The Hobbit")
	if containsDelimitedRun(haystack, nil) {
		t.Error("an empty needle matched")
	}
	if containsDelimitedRun(haystack, tokenize("")) {
		t.Error("a needle of pure punctuation matched")
	}
	if containsDelimitedRun(nil, haystack) {
		t.Error("a needle longer than the haystack matched")
	}
}
