package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLibraryStemSanitisesWhatTheClientOffered(t *testing.T) {
	long := strings.Repeat("Пыль", 90) // 8 bytes a repeat, so well past the cap

	cases := []struct {
		name     string
		original string
		title    string
		suffix   string
		want     string
	}{
		{name: "plain name", original: "Dune.epub", suffix: ".epub", want: "Dune"},
		{name: "a path is only its base", original: "../../etc/passwd", suffix: ".epub", want: "passwd"},
		{name: "a windows path too", original: `C:\books\Dune.epub`, suffix: ".epub", want: "Dune"},
		{name: "the sniffed suffix replaces the offered one", original: "book.epub", suffix: ".fb2", want: "book"},
		{name: "a two-part suffix goes whole", original: "book.fb2.zip", suffix: ".fb2.zip", want: "book"},
		{name: "an unknown extension goes too", original: "book.txt", suffix: ".epub", want: "book"},
		{name: "leading dots are not a hidden file", original: ".hidden.epub", suffix: ".epub", want: "hidden"},
		{name: "control characters become spaces", original: "Du\x00ne\tII.epub", suffix: ".epub", want: "Du ne II"},
		{name: "a colon goes", original: "Dune: Messiah.epub", suffix: ".epub", want: "Dune Messiah"},
		{name: "whitespace collapses", original: "  Dune    II  .epub", suffix: ".epub", want: "Dune II"},
		{name: "only dots falls back to the title", original: "....", title: "Dune", suffix: ".epub", want: "Dune"},
		{name: "nothing at all falls back to the id", original: "", title: "", suffix: ".epub", want: "stage-id"},
		{name: "a title that sanitises to nothing falls through too", original: "/", title: "...", suffix: ".epub", want: "stage-id"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := libraryStem(tt.original, tt.title, "stage-id", tt.suffix); got != tt.want {
				t.Errorf("libraryStem(%q, %q) = %q, want %q", tt.original, tt.title, got, tt.want)
			}
		})
	}

	t.Run("a long name is cut on a rune boundary", func(t *testing.T) {
		got := libraryStem(long+".epub", "", "stage-id", ".epub")
		if len(got) > maxStemBytes {
			t.Errorf("libraryStem kept %d bytes, want at most %d", len(got), maxStemBytes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("libraryStem = %q, which is not valid UTF-8", got)
		}
		if !strings.HasPrefix(long, got) {
			t.Errorf("libraryStem = %q, which is not a prefix of the name it cut", got)
		}
	})
}

func TestCreatePartClaimsAFreeNameAndOnlyEverWritesAPart(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dune.epub"), []byte("first"))

	f, name, err := createPart(dir, "Dune", ".epub")
	if err != nil {
		t.Fatalf("createPart: %v", err)
	}
	defer f.Close()

	if name != "Dune (2).epub" {
		t.Errorf("createPart named %q, want %q", name, "Dune (2).epub")
	}
	if _, err := os.Stat(filepath.Join(dir, "Dune (2).epub.part")); err != nil {
		t.Errorf("the claim did not create the .part file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Dune (2).epub")); err == nil {
		t.Error("createPart created the final name, which only the rename may")
	}
}

// Two claims in flight over one name get two names: the Lstat rules out
// what the library already holds, and the O_EXCL rules out what another
// claim is copying into right now.
func TestCreatePartSkipsANameAnotherClaimHolds(t *testing.T) {
	dir := t.TempDir()

	first, firstName, err := createPart(dir, "Dune", ".epub")
	if err != nil {
		t.Fatalf("createPart: %v", err)
	}
	defer first.Close()

	second, secondName, err := createPart(dir, "Dune", ".epub")
	if err != nil {
		t.Fatalf("createPart: %v", err)
	}
	defer second.Close()

	if firstName != "Dune.epub" || secondName != "Dune (2).epub" {
		t.Errorf("createPart named %q then %q, want %q then %q", firstName, secondName, "Dune.epub", "Dune (2).epub")
	}
}
