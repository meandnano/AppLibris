package importer

import (
	"errors"
	"io/fs"
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

// The copy in progress carries no supported suffix, so neither a sweep nor
// the watcher can act on it, and only publish creates the final name.
func TestClaimPartOnlyEverOpensAPartFile(t *testing.T) {
	dir := t.TempDir()

	f, part, err := claimPart(dir, "Dune", ".epub")
	if err != nil {
		t.Fatalf("claimPart: %v", err)
	}
	defer f.Close()

	if got := filepath.Base(part); got != "Dune.epub.part" {
		t.Errorf("claimPart opened %q, want %q", got, "Dune.epub.part")
	}
	if _, err := os.Stat(filepath.Join(dir, "Dune.epub")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("claimPart created the final name, which only publish may")
	}
}

// Two claims in flight get two part files, so one copy can never write into
// another's.
func TestClaimPartSkipsAPartAnotherCopyHolds(t *testing.T) {
	dir := t.TempDir()

	first, firstPart, err := claimPart(dir, "Dune", ".epub")
	if err != nil {
		t.Fatalf("claimPart: %v", err)
	}
	defer first.Close()

	second, secondPart, err := claimPart(dir, "Dune", ".epub")
	if err != nil {
		t.Fatalf("claimPart: %v", err)
	}
	defer second.Close()

	if filepath.Base(firstPart) != "Dune.epub.part" || filepath.Base(secondPart) != "Dune (2).epub.part" {
		t.Errorf("claimPart opened %q then %q, want Dune.epub.part then Dune (2).epub.part",
			filepath.Base(firstPart), filepath.Base(secondPart))
	}
}

func TestPublishSidestepsANameTheLibraryHolds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dune.epub"), []byte("an earlier book"))
	part := writeFile(t, filepath.Join(dir, "Dune.epub.part"), []byte("the new one"))

	name, err := publish(dir, part, "Dune", ".epub")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if name != "Dune (2).epub" {
		t.Errorf("publish named %q, want %q", name, "Dune (2).epub")
	}
	if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("publish left the part file behind: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "Dune.epub")); got != "an earlier book" {
		t.Errorf("the book already there now reads %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "Dune (2).epub")); got != "the new one" {
		t.Errorf("the published file reads %q", got)
	}
}

// The whole reason publish links rather than renames: a name that appears
// between the claim and the publish must cost the import its first choice,
// never cost the library the file that took it. os.Rename would replace it
// without a word.
func TestPublishNeverReplacesAFileThatAppearedDuringTheCopy(t *testing.T) {
	dir := t.TempDir()

	// The claim happens against an empty directory, exactly as a confirm's
	// does.
	f, part, err := claimPart(dir, "Dune", ".epub")
	if err != nil {
		t.Fatalf("claimPart: %v", err)
	}
	if _, err := f.WriteString("the import"); err != nil {
		t.Fatalf("write the part: %v", err)
	}
	f.Close()

	// Somebody drops a file of that name into the library while the copy
	// is running — the pile is one a person manages by hand, so this is
	// the ordinary case rather than an exotic one.
	writeFile(t, filepath.Join(dir, "Dune.epub"), []byte("dropped in by hand"))

	name, err := publish(dir, part, "Dune", ".epub")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if name != "Dune (2).epub" {
		t.Errorf("publish named %q, want it to have stepped aside to %q", name, "Dune (2).epub")
	}
	if got := readFile(t, filepath.Join(dir, "Dune.epub")); got != "dropped in by hand" {
		t.Errorf("the hand-dropped file now reads %q: publishing replaced it", got)
	}
}

func TestLibraryNameCountsUpFromThePlainName(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{n: 1, want: "Dune.epub"},
		{n: 2, want: "Dune (2).epub"},
		{n: 10, want: "Dune (10).epub"},
	}
	for _, tt := range cases {
		if got := libraryName("Dune", ".epub", tt.n); got != tt.want {
			t.Errorf("libraryName(n=%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
