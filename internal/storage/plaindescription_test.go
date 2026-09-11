package storage

import (
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
