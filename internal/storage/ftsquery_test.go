package storage

import (
	"database/sql"
	"strings"
	"testing"
)

// assertValidFTS5Expression drives got through a real, standalone FTS5
// table's MATCH clause. The property under test is "never produces an
// expression FTS5 rejects" — not any particular string shape — so this
// proves it directly rather than asserting exact output, which the
// sanitizer's exact escaping scheme shouldn't need to be pinned to.
func assertValidFTS5Expression(t *testing.T, got string) {
	t.Helper()
	if got == "" {
		return // the blank query is never sent to MATCH at all
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(body)`); err != nil {
		t.Fatalf("create scratch fts table: %v", err)
	}
	if _, err := db.Query(`SELECT * FROM t WHERE t MATCH ?`, got); err != nil {
		t.Errorf("SanitizeFTSQuery produced %q, which FTS5 rejects: %v", got, err)
	}
}

func TestSanitizeFTSQueryNeverProducesAnInvalidExpression(t *testing.T) {
	inputs := []string{
		`"`,
		`""`,
		`quoted "phrase" here`,
		"AND",
		"OR",
		"NOT",
		"foo AND bar",
		"(",
		")",
		"(foo)",
		"-",
		"-foo",
		"*",
		"foo*",
		"   ",
		"\t\n",
		"",
		`embedded"quote`,
		`multiple""quotes""here`,
		"NEAR(foo, bar)",
		"col:foo",
		"a b c d e f g",
		"\x00",
		"hel\x00lo",
		"\x00\x00\x00",
		"\x01\x02\x1f",
		"-",
		"--",
		"978-0-",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got := SanitizeFTSQuery(in)
			assertValidFTS5Expression(t, got)
		})
	}
}

func TestSanitizeFTSQueryBlankCases(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n\t"} {
		if got := SanitizeFTSQuery(in); got != "" {
			t.Errorf("SanitizeFTSQuery(%q) = %q, want blank", in, got)
		}
	}
}

func TestSanitizeFTSQueryProducesAndedPrefixTerms(t *testing.T) {
	got := SanitizeFTSQuery("har pot")
	want := `"har"* "pot"*`
	if got != want {
		t.Errorf("SanitizeFTSQuery(har pot) = %q, want %q", got, want)
	}
}

func TestSanitizeFTSQueryDoublesEmbeddedQuotes(t *testing.T) {
	got := SanitizeFTSQuery(`say"hi`)
	want := `"say""hi"*`
	if got != want {
		t.Errorf("SanitizeFTSQuery = %q, want %q", got, want)
	}
}

func TestSanitizeFTSQueryStripsControlCharacters(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"\x00", ""},              // nothing left once NUL is stripped: blank, not an error
		{"hel\x00lo", `"hello"*`}, // NUL stripped from the middle of a token, not just dropped whole
		{"\x00\x00\x00", ""},
		{"a\x01b\x02c", `"abc"*`}, // any C0 control character, not just NUL
	}
	for _, c := range cases {
		if got := SanitizeFTSQuery(c.in); got != c.want {
			t.Errorf("SanitizeFTSQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeFTSQueryNormalizesISBNShapedInput(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"9780857059985", `"9780857059985"*`},
		{"978-0-85705-998-5", `"9780857059985"*`},
		{"978 0 85705 998 5", `"9780857059985"*`},
		{"0-306-40615-2", `"0306406152"*`},
		{"030640615X", `"030640615X"*`},
		{"0-306-40615-x", `"030640615X"*`}, // lower-case check character upper-cased

		// Partway through typing a hyphenated one: two hyphens is the point
		// from which the ISBN path takes over, so that the results don't go
		// empty between the first character and the last.
		{"978-0-85705", `"978085705"*`},
		{"978-0-8", `"97808"*`},
		{"978-0-", `"9780"*`}, // a trailing hyphen is what is on screen between two groups
	}
	for _, c := range cases {
		if got := SanitizeFTSQuery(c.in); got != c.want {
			t.Errorf("SanitizeFTSQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeFTSQueryDoesNotTreatOrdinaryNumbersAsISBNs(t *testing.T) {
	// None of these is a complete ISBN or a hyphenated one being typed, so
	// they fall through to the ordinary per-word path.
	cases := []struct {
		in, want string
	}{
		{"1984", `"1984"*`},
		{"12345678901234", `"12345678901234"*`},
		{"1984-2001", `"1984-2001"*`},                             // a date range, kept a title query by the two-hyphen rule
		{"978085", `"978085"*`},                                   // an unpunctuated partial already prefix-matches as one token
		{"Twenty-One Balloons", `"Twenty-One"* "Balloons"*`},      // a hyphenated title
		{"978-0-85705-998-5-1-2-3", `"978-0-85705-998-5-1-2-3"*`}, // fourteen digits, over the cap
		{"ISBN-978-0", `"ISBN-978-0"*`},                           // letters, so not digits and hyphens
	}
	for _, c := range cases {
		if got := SanitizeFTSQuery(c.in); got != c.want {
			t.Errorf("SanitizeFTSQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeFTSQueryMatchesEveryStateOfATypedHyphenatedISBN asserts the
// sequence rather than any one input: the defect this covers was that the
// results went empty partway through typing and filled back in on the last
// character, which no single query can show.
func TestSanitizeFTSQueryMatchesEveryStateOfATypedHyphenatedISBN(t *testing.T) {
	const (
		typed   = "978-0-85705-998-5"
		indexed = "9780857059985"
	)
	// From the second hyphen onward — before that the query is one or two
	// groups, which stays a per-word query on purpose.
	for i := len("978-0-"); i <= len(typed); i++ {
		prefix := typed[:i]
		digits := strings.Map(func(r rune) rune {
			if r == '-' {
				return -1
			}
			return r
		}, prefix)

		got := SanitizeFTSQuery(prefix)
		want := `"` + digits + `"*`
		if got != want {
			t.Errorf("SanitizeFTSQuery(%q) = %q, want %q", prefix, got, want)
		}
		if !strings.HasPrefix(indexed, digits) {
			t.Errorf("typing %q yielded %q, which is not a prefix of the indexed token %q", prefix, digits, indexed)
		}
	}
}
