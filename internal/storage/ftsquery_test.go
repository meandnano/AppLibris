package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

		// Partway through typing a hyphenated one: two hyphens and four
		// digits is where the ISBN path takes over, so that the results
		// stop going empty from the second hyphen onward.
		{"978-0-85705", `"978085705"*`},
		{"978-0-8", `"97808"*`},
		{"978-0-", `"9780"*`},                      // a trailing hyphen is what is on screen between two groups
		{"978-0-85705-998-5-", `"9780857059985"*`}, // thirteen digits: the complete shape takes it, hyphens and all
		{"0-19-8", `"0198"*`},                      // the keystroke that recovers a two-digit-registrant ISBN-10

		// An accepted cost, recorded rather than discovered: a three-group
		// number past the digit floor reads as an ISBN prefix, so a title
		// carrying an ISO-style date stops being findable by it. Separating
		// the two needs the number's meaning, not its punctuation.
		{"2026-09-06", `"20260906"*`},

		// Whitespace around either shape is trimmed before anything else,
		// including the U+00A0 the complete shape's own Replacer would
		// leave in place.
		{" 978-0-85705 ", `"978085705"*`},
		{"\u00a09780857059985\u00a0", `"9780857059985"*`},
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
		{"978-0-85705-998-51", `"978-0-85705-998-51"*`},           // fourteen digits, one past a whole ISBN-13
		{"978-0-85705-998-5-1-2-3", `"978-0-85705-998-5-1-2-3"*`}, // sixteen, well past it
		{"ISBN-978-0", `"ISBN-978-0"*`},                           // letters, so not digits and hyphens
		{"9-1-1", `"9-1-1"*`},                                     // three groups, under the digit floor
		{"1-2-3", `"1-2-3"*`},
		{"1--", `"1--"*`},                            // one digit: nothing an identifier could be
		{"--", `"--"*`},                              // no digits at all, so never an empty prefix term
		{"---", `"---"*`},                            // and no number of hyphens changes that
		{"978-0-85705 998", `"978-0-85705"* "998"*`}, // a space still separates tokens, hyphens either side of it or not
		{"1984-85 2000-01", `"1984-85"* "2000-01"*`}, // the pair of ranges the no-space rule exists for

		// The digit floor's own cost, on the population this feature
		// serves: an ISBN-10 whose registrant is two digits (0-19 OUP, 0-14
		// Penguin) reaches its second hyphen three digits in, so it waits
		// one keystroke longer than an ISBN-13 does. No threshold separates
		// it from "9-1-1" above, which is the same three digits and two
		// hyphens — but that one would never match its book, where this one
		// matches on the very next character (see the ISBN table).
		{"0-19-", `"0-19-"*`},
		{"0-14-", `"0-14-"*`},
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
// character, which no single query can show. Both halves are here, since
// where the ISBN path takes over is the design and not an accident — the
// one state that still matches nothing, "978-0", is the two-hyphen rule's
// own cost.
func TestSanitizeFTSQueryMatchesEveryStateOfATypedHyphenatedISBN(t *testing.T) {
	const (
		typed   = "978-0-85705-998-5"
		indexed = "9780857059985"
	)
	for i := 1; i <= len(typed); i++ {
		prefix := typed[:i]
		got := SanitizeFTSQuery(prefix)

		if strings.Count(prefix, "-") < 2 {
			want := `"` + prefix + `"*`
			if got != want {
				t.Errorf("SanitizeFTSQuery(%q) = %q, want the per-word %q", prefix, got, want)
			}
			continue
		}

		term := strings.TrimSuffix(strings.TrimPrefix(got, `"`), `"*`)
		if term == got || term == "" {
			t.Errorf("SanitizeFTSQuery(%q) = %q, want a single quoted prefix term", prefix, got)
			continue
		}
		if !strings.HasPrefix(indexed, term) {
			t.Errorf("typing %q produced the term %q, which is not a prefix of the indexed token %q", prefix, term, indexed)
		}
	}
}

func TestSanitizeFTSQueryDropsTokensPastTheTermCap(t *testing.T) {
	fields := make([]string, maxSearchTerms+1)
	for i := range fields {
		fields[i] = fmt.Sprintf("w%02d", i)
	}
	got := SanitizeFTSQuery(strings.Join(fields, " "))

	if n := strings.Count(got, `*`); n != maxSearchTerms {
		t.Errorf("SanitizeFTSQuery(%d tokens) produced %d terms, want %d: %q", len(fields), n, maxSearchTerms, got)
	}
	if last := fields[len(fields)-1]; strings.Contains(got, last) {
		t.Errorf("SanitizeFTSQuery kept %q, the token past the cap: %q", last, got)
	}
	assertValidFTS5Expression(t, got)
}

// The byte cap cuts on a rune boundary, dropping a straddling character
// whole. Deliberately *not* asserted through FTS5: a term ending mid-rune
// is one FTS5 accepts, matching nothing, so a round trip there would pass
// either way. What the boundary buys is a final term that searches for
// something typeable, and a value the page can render — which the
// internal/web half of this asserts on the rendered body.
func TestSanitizeFTSQueryCutsOnARuneBoundary(t *testing.T) {
	filler := strings.Repeat("a", MaxSearchBytes-1)
	got := SanitizeFTSQuery(filler + "é")

	if want := `"` + filler + `"*`; got != want {
		t.Errorf("SanitizeFTSQuery cut mid-rune: got %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Errorf("SanitizeFTSQuery produced invalid UTF-8: %q", got)
	}
}

// A three-byte rune straddles the cap from three different offsets, two of
// which a boundary check that backed off a single byte would still split.
func TestSanitizeFTSQueryCutsAMultibyteRuneWhole(t *testing.T) {
	for pad := 0; pad < 3; pad++ {
		in := strings.Repeat("a", MaxSearchBytes-1-pad) + "→" + strings.Repeat("b", 8)
		got := SanitizeFTSQuery(in)
		if !utf8.ValidString(got) {
			t.Errorf("pad %d: SanitizeFTSQuery produced invalid UTF-8: %q", pad, got)
		}
	}
}

// Both ISBN shapes are far shorter than the cap, so what the cap can still
// do to one is decide whether it is on screen when the cut lands. Trailing
// padding is harmless; leading padding past the cap takes the ISBN with it,
// which is a real cost of cutting the input rather than the terms — and one
// nobody can type, since the padding would have to be pasted ahead of the
// number.
func TestSanitizeFTSQueryISBNPathSurvivesTheCap(t *testing.T) {
	const isbn = "978 0 85705 998 5"
	pad := strings.Repeat(" ", MaxSearchBytes)

	if got, want := SanitizeFTSQuery(isbn+pad), `"9780857059985"*`; got != want {
		t.Errorf("trailing padding past the cap: SanitizeFTSQuery = %q, want %q", got, want)
	}
	if got := SanitizeFTSQuery(pad + isbn); got != "" {
		t.Errorf("leading padding past the cap: SanitizeFTSQuery = %q, want %q (the ISBN is cut away)", got, "")
	}
}

func TestNormalizeSearchQuery(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"short input is returned as it stands", "har pot", "har pot"},
		{"control characters are stripped", "hel\x00lo", "hello"},
		{"nothing but control characters", "\x00\x01\x1f", ""},
		{
			"over the cap, cut to it",
			strings.Repeat("a", MaxSearchBytes+10),
			strings.Repeat("a", MaxSearchBytes),
		},
		{
			// The stripping runs first, so an input whose bytes exceed the
			// cap only because of control characters is not cut at all.
			"control characters do not count toward the cap",
			strings.Repeat("\x01", 100) + strings.Repeat("a", MaxSearchBytes),
			strings.Repeat("a", MaxSearchBytes),
		},
		{
			"a straddling rune is dropped whole",
			strings.Repeat("a", MaxSearchBytes-1) + "é",
			strings.Repeat("a", MaxSearchBytes-1),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeSearchQuery(c.in)
			if got != c.want {
				t.Errorf("NormalizeSearchQuery(%.32q…) = %.32q…, want %.32q…", c.in, got, c.want)
			}
			if again := NormalizeSearchQuery(got); again != got {
				t.Errorf("not idempotent: %.32q… normalizes again to %.32q…", got, again)
			}
			if !utf8.ValidString(got) {
				t.Errorf("NormalizeSearchQuery produced invalid UTF-8: %q", got)
			}
		})
	}
}

// internal/web renders what this returns and internal/service searches what
// SanitizeFTSQuery makes of it, so the two agreeing on the bound is the
// whole point of the transport calling the same function rather than
// clipping to the same number.
func TestSanitizeFTSQueryIsBuiltFromTheNormalizedQuery(t *testing.T) {
	for _, in := range []string{
		"har pot",
		"hel\x00lo",
		strings.Repeat("a", MaxSearchBytes+10),
		strings.Repeat("é", MaxSearchBytes),
		"978 0 85705 998 5",
	} {
		if got, want := SanitizeFTSQuery(in), SanitizeFTSQuery(NormalizeSearchQuery(in)); got != want {
			t.Errorf("SanitizeFTSQuery(%.24q…) = %q, but %q once normalized first", in, got, want)
		}
	}
}

// The reproduction from the 2026-09-07 review: 100,000 single-letter tokens
// were still executing inside CountSearchBooks when a ten-minute test
// timeout fired, holding one of the read pool's eight connections for the
// whole time. All three queries one search request makes are driven here,
// under a deadline, so the failure is a failure rather than a hung suite.
//
// What it does *not* pin is maxSearchTerms, and that is worth knowing
// before trusting it: MaxSearchBytes is applied first and 256 bytes admits
// at most 128 single-letter tokens, so with the term cap lifted to a
// million this still passed in 0.25s. It fails (17s, deadline exceeded)
// only with both caps gone. TestSanitizeFTSQueryDropsTokensPastTheTermCap
// is what pins the term cap; this pins that a query nobody should be able
// to send cannot cost the read pool a connection.
func TestSearchWithAnAbsurdTokenCountCompletesPromptly(t *testing.T) {
	db := openTestDB(t)
	seedSearchableBooks(t, db, 200)

	tokens := make([]string, 100_000)
	for i := range tokens {
		tokens[i] = "a"
	}
	query := SanitizeFTSQuery(strings.Join(tokens, " "))

	// One budget for both the deadline and the assertion, so the message
	// cannot claim a threshold the check does not use. Generous against a
	// measured ~20ms with both caps in place: the failure being caught is
	// seconds-to-minutes, not milliseconds.
	const budget = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	if _, err := db.SearchBooks(ctx, query, BookPage{Limit: 48}); err != nil {
		t.Fatalf("SearchBooks: %v", err)
	}
	if _, err := db.CountSearchBooks(ctx, query); err != nil {
		t.Fatalf("CountSearchBooks: %v", err)
	}
	if _, err := db.MatchedSearchFields(ctx, query); err != nil {
		t.Fatalf("MatchedSearchFields: %v", err)
	}
	if elapsed := time.Since(start); elapsed > budget {
		t.Errorf("one search request's three queries took %v, want under %v", elapsed, budget)
	}
}

// seedSearchableBooks creates n books whose title, authors and description
// all match the letter the query above repeats, so every term the
// expression carries has rows to work over rather than being answered by an
// empty index.
func seedSearchableBooks(t *testing.T, db *DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("a book about apples %03d", i)
		if _, err := db.CreateBook(context.Background(), Book{
			ContentHash: fmt.Sprintf("absurd-%03d", i),
			Title:       title,
			SortTitle:   title,
			Description: "an apple a day, and another apple after that",
			ISBN:        "9780857059985",
			Format:      "epub",
		}, []string{"Anna Applebaum"}); err != nil {
			t.Fatalf("CreateBook %d: %v", i, err)
		}
	}
}
