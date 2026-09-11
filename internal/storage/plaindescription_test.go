package storage

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
)

func TestPlainDescription(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain text is untouched", "A plain synopsis.", "A plain synopsis."},
		{"inline tags are dropped", "A <b>bold</b> and <i>italic</i> claim.", "A bold and italic claim."},
		{"br becomes a line break", "First line.<br>Second line.", "First line.\nSecond line."},
		{"self-closing br becomes a line break", "First.<br/>Second.", "First.\nSecond."},
		{"paragraphs are separated by a blank line", "<p>One.</p><p>Two.</p>", "One.\n\nTwo."},
		{"entities are unescaped", "Salt &amp; pepper &mdash; a pair.", "Salt & pepper — a pair."},
		{
			"escaped markup survives as text",
			"Use &lt;b&gt; for bold.",
			"Use <b> for bold.",
		},
		{"a bare less-than is not a tag", "a < b and c > d", "a < b and c > d"},
		{"an unterminated tag is not stripped", "ends with <p", "ends with <p"},
		{"attributes are dropped with their tag", `<a href="http://x/">link</a>`, "link"},
		{"surrounding whitespace is trimmed", "<p>  Trimmed.  </p>", "Trimmed."},
		{"blank lines are capped in plain text too", "a\n\n\n\n\nb", "a\n\nb"},
		{"a padded blank line does not defeat the cap", "a\n  \n  \n\nb", "a\n\nb"},
		{"CRLF is folded", "a\r\n\r\n\r\n\r\nb", "a\n\nb"},
		{"a decoded carriage return is folded, not counted apart", "a&#13;\r\nb", "a\n\nb"},
		{"a reference with no terminator is prose", "Rock &copy roll", "Rock &copy roll"},
		{"an ampersand between words is prose", "AT&T and R&D", "AT&T and R&D"},
		{"a numeric reference is decoded", "curly &#8217; quote", "curly \u2019 quote"},
		{"a hex reference is decoded", "&#x2014; dash", "\u2014 dash"},
		{"a terminated reference naming nothing is HTML's to read", "&notanentity;", "\u00acanentity;"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlainDescription(tt.raw); got != tt.want {
				t.Errorf("PlainDescription(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// trimBlank must be a superset of strings.TrimSpace, never a subset. It was
// briefly a subset — written as strings.Trim over a hand-listed cutset,
// which silently dropped the seventeen runes unicode.IsSpace accepts and
// the cutset did not, U+3000 (the CJK ideographic space) among them. This
// enumerates the whole space rather than sampling it, because sampling is
// what missed those runes the first time.
func TestTrimBlankIsASupersetOfTrimSpace(t *testing.T) {
	var missed []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) && trimBlank(string(r)) != "" {
			missed = append(missed, r)
		}
	}
	if len(missed) > 0 {
		t.Errorf("trimBlank leaves %d runes unicode.IsSpace accepts: %U", len(missed), missed)
	}
	for _, r := range zeroWidth {
		if trimBlank(string(r)) != "" {
			t.Errorf("trimBlank leaves the zero-width rune %U", r)
		}
	}
	// And it must not eat anything that carries ink, at either edge.
	for _, s := range []string{"a", "本", "—", "·", "9"} {
		if got := trimBlank(s + " x " + s); got != s+" x "+s {
			t.Errorf("trimBlank(%q) = %q", s+" x "+s, got)
		}
	}
}

// CapBlankLines is exported for two callers that cannot be allowed to shape a
// description differently: internal/enrich's sanitizeValue for a provider's
// answer, and PlainDescription for what it flattens.
func TestCapBlankLines(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"a single newline is left alone", "one\ntwo", "one\ntwo"},
		{"one blank line is left alone", "one\n\ntwo", "one\n\ntwo"},
		{"a longer run is capped at two", "one\n\n\n\n\ntwo", "one\n\ntwo"},
		{"CRLF is folded first", "one\r\n\r\n\r\ntwo", "one\n\ntwo"},
		{"a lone carriage return becomes a newline", "one\rtwo", "one\ntwo"},
		{"trailing whitespace does not hide a blank line", "one\n \n\t\n\ntwo", "one\n\ntwo"},
		{"text with no breaks is untouched", "one two", "one two"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CapBlankLines(tt.value); got != tt.want {
				t.Errorf("CapBlankLines(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

// capBlankLinesTheObviousWay is CapBlankLines written the way it reads:
// fold the carriage returns, trim each line, then collapse until nothing
// is left to collapse. It exists only as the oracle below.
func capBlankLinesTheObviousWay(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	value = strings.Join(lines, "\n")
	for strings.Contains(value, "\n\n\n") {
		value = strings.ReplaceAll(value, "\n\n\n", "\n\n")
	}
	return value
}

// CapBlankLines is one hand-rolled pass, for the allocation profile: the
// obvious spelling above costs ~40 allocations and 81 MB against 1 and
// 5.6 MB on four megabytes of newlines, which is the width internal/epub's
// package-document bound allows before internal/scanner cuts a description
// to 64 KiB. The two must agree on every input, so the obvious spelling
// stays here as the oracle rather than as a comment claiming they match.
func TestCapBlankLinesMatchesTheObviousSpelling(t *testing.T) {
	alphabet := []string{"\n", "\r", "\r\n", " ", "\t", "a", "本", "\n\n", "  ", "\n \n", "z", "\r\r"}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 400000; i++ {
		var sb strings.Builder
		for n := r.Intn(14); n > 0; n-- {
			sb.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		in := sb.String()
		if got, want := CapBlankLines(in), capBlankLinesTheObviousWay(in); got != want {
			t.Fatalf("CapBlankLines(%q) = %q, the obvious spelling = %q", in, got, want)
		}
	}
}
