package fb2

import (
	"archive/zip"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func buildTestFB2(t *testing.T, xmlBody string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.fb2")
	if err := os.WriteFile(path, []byte(xmlBody), 0o644); err != nil {
		t.Fatalf("write fb2 file: %v", err)
	}
	return path
}

// buildTestFB2Zip writes a .fb2.zip archive whose entries are exactly
// entries (name -> content); an empty content isn't written specially, it's
// just an empty file, so an entry that shouldn't have .fb2 content can be
// used to test the "no .fb2 entry" case.
func buildTestFB2Zip(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.fb2.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fb2.zip file: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s in zip: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return path
}

const testFB2Template = `<?xml version="1.0" encoding="%s"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
  <description>
    <title-info>
      <book-title>%s</book-title>
      <author>
        <first-name>Jane</first-name>
        <middle-name>Q</middle-name>
        <last-name>Doe</last-name>
      </author>
      <lang>en</lang>
      <annotation>
        <p>First paragraph.</p>
        <p>Second paragraph.</p>
      </annotation>
      <coverpage>
        <image l:href="#cover.jpg"/>
      </coverpage>
    </title-info>
    <publish-info>
      <publisher>Acme Books</publisher>
      <year>2011</year>
      <isbn>978-0-306-40615-7</isbn>
    </publish-info>
  </description>
  <body></body>
  <binary id="cover.jpg" content-type="image/jpeg">%s</binary>
</FictionBook>`

func TestReadMetadataFullDocument(t *testing.T) {
	// Wrapped across lines, like a real file's base64 <binary> content, to
	// exercise the whitespace-stripping in readCover along the way.
	coverBytes := []byte("fake cover image bytes")
	encoded := base64.StdEncoding.EncodeToString(coverBytes)
	wrapped := encoded[:len(encoded)/2] + "\n" + encoded[len(encoded)/2:]

	path := buildTestFB2(t, fmt.Sprintf(testFB2Template, "utf-8", "Test Book", wrapped))

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Title != "Test Book" {
		t.Errorf("Title = %q, want %q", got.Title, "Test Book")
	}
	if len(got.Authors) != 1 || got.Authors[0] != "Jane Q Doe" {
		t.Errorf("Authors = %v, want [Jane Q Doe]", got.Authors)
	}
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q", got.Language, "en")
	}
	if got.Description != "First paragraph.\n\nSecond paragraph." {
		t.Errorf("Description = %q, want the two paragraphs joined by a blank line", got.Description)
	}
	if got.Publisher != "Acme Books" {
		t.Errorf("Publisher = %q, want %q", got.Publisher, "Acme Books")
	}
	if got.PublishedDate != "2011" {
		t.Errorf("PublishedDate = %q, want %q", got.PublishedDate, "2011")
	}
	if got.ISBN != "978-0-306-40615-7" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "978-0-306-40615-7")
	}
	if string(got.Cover) != string(coverBytes) {
		t.Errorf("Cover = %q, want %q", got.Cover, coverBytes)
	}
}

// The test the whole encoding decision exists for. On a bare xml.Decoder
// (no CharsetReader) this fails outright with "encoding \"windows-1251\"
// declared but Decoder.CharsetReader is nil" — confirmed against master.
// The library's real files are UTF-8 regardless of what they declare, so
// this document's actual bytes are UTF-8 despite the windows-1251 label,
// and must still parse correctly.
func TestReadMetadataDeclaredWindows1251ParsesAsUTF8(t *testing.T) {
	path := buildTestFB2(t, fmt.Sprintf(testFB2Template, "windows-1251", "Книга", base64.StdEncoding.EncodeToString([]byte("x"))))

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Title != "Книга" {
		t.Errorf("Title = %q, want %q", got.Title, "Книга")
	}
}

func TestReadMetadataUnknownEncodingLabelDoesNotError(t *testing.T) {
	path := buildTestFB2(t, fmt.Sprintf(testFB2Template, "totally-made-up-charset", "Test Book", base64.StdEncoding.EncodeToString([]byte("x"))))

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Title != "Test Book" {
		t.Errorf("Title = %q, want %q", got.Title, "Test Book")
	}
}

func TestAuthorNameAssembly(t *testing.T) {
	tests := []struct {
		name string
		a    fb2Author
		want string
	}{
		{"all three", fb2Author{FirstName: "Jane", MiddleName: "Q", LastName: "Doe"}, "Jane Q Doe"},
		{"first and last only", fb2Author{FirstName: "Jane", LastName: "Doe"}, "Jane Doe"},
		{"last only", fb2Author{LastName: "Doe"}, "Doe"},
		{"nickname only, no real name given at all", fb2Author{Nickname: "Pen Name"}, "Pen Name"},
		{"a real name present wins over a nickname", fb2Author{LastName: "Doe", Nickname: "Pen Name"}, "Doe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorName(tt.a); got != tt.want {
				t.Errorf("authorName(%+v) = %q, want %q", tt.a, got, tt.want)
			}
		})
	}
}

const testFB2NicknameOnlyAuthorTemplate = `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description>
    <title-info>
      <book-title>Nickname Only</book-title>
      <author>
        <nickname>Pen Name</nickname>
      </author>
    </title-info>
  </description>
</FictionBook>`

// The FB2 schema explicitly permits a nickname-only author (no real name
// given at all) — this must not be silently dropped.
func TestReadMetadataNicknameOnlyAuthor(t *testing.T) {
	path := buildTestFB2(t, testFB2NicknameOnlyAuthorTemplate)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if len(got.Authors) != 1 || got.Authors[0] != "Pen Name" {
		t.Errorf("Authors = %v, want [Pen Name]", got.Authors)
	}
}

const testFB2AnnotationMarkupTemplate = `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description>
    <title-info>
      <book-title>Annotation Markup</book-title>
      <annotation>
        <p>A <emphasis>great</emphasis> book.</p>
        <p><emphasis>Entire paragraph is emphasized.</emphasis></p>
      </annotation>
    </title-info>
  </description>
</FictionBook>`

// Inline markup inside a <p> (emphasis, strong, ...) must not eat the text
// it wraps — including when it wraps the paragraph's entire content, which
// a naive plain-string field drops outright rather than just losing the
// tag.
func TestReadMetadataAnnotationRetainsTextInsideInlineMarkup(t *testing.T) {
	path := buildTestFB2(t, testFB2AnnotationMarkupTemplate)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	want := "A great book.\n\nEntire paragraph is emphasized."
	if got.Description != want {
		t.Errorf("Description = %q, want %q", got.Description, want)
	}
}

const testFB2NoPublishInfoTemplate = `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description>
    <title-info>
      <book-title>Written Date Only</book-title>
      <date value="2005">2005</date>
    </title-info>
  </description>
</FictionBook>`

func TestFindPublishedDatePrefersPublishInfoYearOverTitleInfoDate(t *testing.T) {
	path := buildTestFB2(t, fmt.Sprintf(testFB2Template, "utf-8", "Test Book", base64.StdEncoding.EncodeToString([]byte("x"))))

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.PublishedDate != "2011" {
		t.Errorf("PublishedDate = %q, want the publish-info/year %q, not title-info/date", got.PublishedDate, "2011")
	}
}

func TestFindPublishedDateFallsBackToTitleInfoDateWhenPublishInfoAbsent(t *testing.T) {
	path := buildTestFB2(t, testFB2NoPublishInfoTemplate)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.PublishedDate != "2005" {
		t.Errorf("PublishedDate = %q, want %q (title-info/date's value attribute)", got.PublishedDate, "2005")
	}
}

const testFB2DanglingCoverTemplate = `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
  <description>
    <title-info>
      <book-title>Dangling Cover</book-title>
      <coverpage>
        <image l:href="#does-not-exist.jpg"/>
      </coverpage>
    </title-info>
  </description>
</FictionBook>`

// The FB2 mirror of epub_test.go's TestReadMetadataCoverDanglingReference.
func TestReadMetadataCoverDanglingReference(t *testing.T) {
	path := buildTestFB2(t, testFB2DanglingCoverTemplate)

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Cover != nil {
		t.Errorf("Cover = %v, want nil (the referenced binary id doesn't exist)", got.Cover)
	}
	if got.Title != "Dangling Cover" {
		t.Errorf("Title = %q, want %q (a dangling cover reference must not invalidate the rest of the metadata)", got.Title, "Dangling Cover")
	}
}

func TestReadMetadataFromZipWithOneEntry(t *testing.T) {
	xmlBody := fmt.Sprintf(testFB2Template, "utf-8", "Zipped Book", base64.StdEncoding.EncodeToString([]byte("x")))
	path := buildTestFB2Zip(t, map[string]string{"book.fb2": xmlBody})

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Title != "Zipped Book" {
		t.Errorf("Title = %q, want %q", got.Title, "Zipped Book")
	}
}

func TestReadMetadataFromZipWithNoEntry(t *testing.T) {
	path := buildTestFB2Zip(t, map[string]string{"readme.txt": "not a book"})

	if _, err := ReadMetadata(path); err == nil {
		t.Fatal("ReadMetadata on a zip with no .fb2 entry: want an error, got nil")
	}
}

func TestReadMetadataFromZipWithTwoEntries(t *testing.T) {
	xmlBody := fmt.Sprintf(testFB2Template, "utf-8", "Book", base64.StdEncoding.EncodeToString([]byte("x")))
	path := buildTestFB2Zip(t, map[string]string{
		"a.fb2": xmlBody,
		"b.fb2": xmlBody,
	})

	if _, err := ReadMetadata(path); err == nil {
		t.Fatal("ReadMetadata on a zip with two .fb2 entries: want an error, got nil (must not guess which book it contains)")
	}
}

func TestReadMetadataNotXML(t *testing.T) {
	path := buildTestFB2(t, "this is not xml at all")

	if _, err := ReadMetadata(path); err == nil {
		t.Fatal("ReadMetadata on non-XML content: want an error, got nil")
	}
}
