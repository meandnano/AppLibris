package storage

import (
	"database/sql"
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
