// testdata/cp1251.fb2 is a hand-made fixture, not a live capture: a UTF-8
// FB2 document run through `iconv -f UTF-8 -t CP1251`, whose XML
// declaration was then rewritten to encoding="windows-1251" by hand — iconv
// transcodes content and leaves the declaration saying utf-8. It carries no
// BOM. Those are the two places such a fixture goes wrong, and the point of
// committing real cp1251 bytes is that a string transcoded inside the test
// would not prove encoding/xml ever sees them
package fb2

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"

	"library/internal/cover"
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
	if got.ISBN != "9780306406157" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "9780306406157")
	}
	if string(got.Cover) != string(coverBytes) {
		t.Errorf("Cover = %q, want %q", got.Cover, coverBytes)
	}
}

// The test the whole encoding decision exists for, against real cp1251
// bytes rather than a string transcoded in the test: encoding/xml refuses
// bytes that are not valid UTF-8 whatever Strict says, so a legacy Russian
// collection's honestly labelled files parse only if the declaration is
// honoured
func TestReadMetadataDecodesWindows1251(t *testing.T) {
	got, err := ReadMetadata(filepath.Join("testdata", "cp1251.fb2"))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Title != "Мастер и Маргарита" {
		t.Errorf("Title = %q, want %q", got.Title, "Мастер и Маргарита")
	}
	if len(got.Authors) != 1 || got.Authors[0] != "Михаил Афанасьевич Булгаков" {
		t.Errorf("Authors = %v, want [Михаил Афанасьевич Булгаков]", got.Authors)
	}
	if got.Publisher != "Художественная литература" {
		t.Errorf("Publisher = %q, want %q", got.Publisher, "Художественная литература")
	}
	if got.Description != "Роман о дьяволе в Москве." {
		t.Errorf("Description = %q, want %q", got.Description, "Роман о дьяволе в Москве.")
	}
}

// The other half of honouring the label, and the cost of it: a UTF-8 file
// that declares windows-1251 now reads its own bytes through the cp1251
// table and gets mojibake. That is the trade the decision makes — the cost
// falls on a file that misdescribes itself, and a mojibake title is one
// edit away from right where a parse failure is a book nobody finds
func TestReadMetadataMislabelledUTF8DegradesToMojibake(t *testing.T) {
	path := buildTestFB2(t, fmt.Sprintf(testFB2Template, "windows-1251", "Книга", base64.StdEncoding.EncodeToString([]byte("x"))))

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Title == "" {
		t.Fatal("Title is empty: a lying label must cost the title, not the book")
	}
	if got.Title == "Книга" {
		t.Error("Title survived intact: the declared charset was not applied")
	}
}

// The .fb2.zip cap sits beneath the charset decoder, so a cp1251 archive
// past maxZipDocumentBytes is still refused as too large rather than
// reported as malformed
func TestReadMetadataZipCapStillAppliesUnderADecoder(t *testing.T) {
	padding := strings.Repeat("<!-- комментарий -->\n", 8192)
	doc := `<?xml version="1.0" encoding="windows-1251"?>` + "\n" + padding +
		`<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description><title-info><book-title>Капризная</book-title></title-info></description>
</FictionBook>`
	cp1251, err := charmap.Windows1251.NewEncoder().String(doc)
	if err != nil {
		t.Fatalf("encode the document as cp1251: %v", err)
	}

	_, err = readCappedDocument(strings.NewReader(cp1251), 4096)
	if !errors.Is(err, errDocumentTooLarge) {
		t.Fatalf("readMetadata error = %v, want errDocumentTooLarge", err)
	}
}

// The cap the decoder makes necessary. encoding/xml holds a whole decoded
// text node, and cp1251 Cyrillic doubles in UTF-8, so a document that fits
// under the cap as archive bytes can exceed it as the bytes actually held.
// Capping only the read would hand an untrusted archive twice the budget
// maxZipDocumentBytes names
func TestReadMetadataCapsWhatTheDecoderProduces(t *testing.T) {
	// Cyrillic only, so every byte read becomes two bytes held: sized to
	// sit under the cap encoded and over it decoded
	const cap = 4096
	body := strings.Repeat("я", cap*3/4)
	doc := `<?xml version="1.0" encoding="windows-1251"?>` +
		`<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">` +
		`<description><title-info><book-title>` + body + `</book-title></title-info></description>` +
		`</FictionBook>`
	cp1251, err := charmap.Windows1251.NewEncoder().String(doc)
	if err != nil {
		t.Fatalf("encode the document as cp1251: %v", err)
	}
	if len(cp1251) >= cap {
		t.Fatalf("fixture is %d encoded bytes, want it under the %d-byte cap so only the decoded cap can fire", len(cp1251), cap)
	}

	if _, err := readCappedDocument(strings.NewReader(cp1251), cap); !errors.Is(err, errDocumentTooLarge) {
		t.Fatalf("readMetadata error = %v, want errDocumentTooLarge", err)
	}
}

func TestReadMetadataNormalisesISBN(t *testing.T) {
	doc := `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description>
    <title-info><book-title>Punctuated ISBN</book-title></title-info>
    <publish-info><isbn>978-5-17-118366-1</isbn></publish-info>
  </description>
</FictionBook>`
	got, err := ReadMetadata(buildTestFB2(t, doc))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ISBN != "9785171183661" {
		t.Errorf("ISBN = %q, want %q", got.ISBN, "9785171183661")
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

// fb2WithBinaries renders a document whose description names cover.jpg as
// the cover and whose <binary> elements are binaries, in order, each given
// as (id, base64 content)
func fb2WithBinaries(title string, binaries [][2]string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
  <description>
    <title-info>
      <book-title>`)
	sb.WriteString(title)
	sb.WriteString(`</book-title>
      <coverpage>
        <image l:href="#cover.jpg"/>
      </coverpage>
    </title-info>
  </description>
  <body></body>
`)
	for _, b := range binaries {
		sb.WriteString(`  <binary id="` + b[0] + `" content-type="image/jpeg">` + b[1] + "</binary>\n")
	}
	sb.WriteString("</FictionBook>\n")
	return sb.String()
}

// An illustrated book carries hundreds of <binary> elements and one of them
// is the cover. Decoding them all through a struct held every illustration
// in memory to find that one; the token walk skips the rest, so the heap
// cost of reading a title out of a 300 MB book is bounded by the largest
// single element the tokeniser passes over, not the document. The bound is
// taken relative to a one-illustration document rather than as an absolute
// figure: TotalAlloc counts every doubling of the tokeniser's buffer, and
// the race detector build doubles that again, so a fixed number sized off
// one build fails on the other
func TestReadMetadataSkipsNonCoverBinariesWithoutHoldingThem(t *testing.T) {
	const illustrations = 16
	illustration := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x7f}, 1<<20))
	coverBytes := []byte("the real cover")
	coverBinary := [2]string{"cover.jpg", base64.StdEncoding.EncodeToString(coverBytes)}

	document := func(count int) string {
		binaries := make([][2]string, 0, count+1)
		for i := 0; i < count; i++ {
			binaries = append(binaries, [2]string{fmt.Sprintf("illustration-%d.jpg", i), illustration})
		}
		return fb2WithBinaries("Illustrated", append(binaries, coverBinary))
	}
	measure := func(path string) (Metadata, uint64) {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		got, err := ReadMetadata(path)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("ReadMetadata: %v", err)
		}
		return got, after.TotalAlloc - before.TotalAlloc
	}

	_, oneCost := measure(buildTestFB2(t, document(1)))
	got, manyCost := measure(buildTestFB2(t, document(illustrations)))
	if !bytes.Equal(got.Cover, coverBytes) {
		t.Errorf("Cover = %q, want %q", got.Cover, coverBytes)
	}
	// The struct decode accumulated every binary's content as a string, so
	// sixteen illustrations cost at least sixteen times one. The walk's
	// buffer is reused across them, so they cost about the same as one.
	t.Logf("ReadMetadata allocated %d bytes over one illustration, %d over %d", oneCost, manyCost, illustrations)
	if manyCost > 2*oneCost {
		t.Errorf("%d illustrations allocated %d bytes against %d for one, want the walk bounded by one", illustrations, manyCost, oneCost)
	}
}

// A cover past the cap is dropped, not held and then refused: nothing after
// the cap is read, and the text metadata comes back intact
func TestReadMetadataDropsCoverBinaryOverTheCap(t *testing.T) {
	oversized := base64.StdEncoding.EncodeToString(make([]byte, cover.MaxCoverBytes+1))
	path := buildTestFB2(t, fb2WithBinaries("Capped", [][2]string{{"cover.jpg", oversized}}))

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Cover != nil {
		t.Errorf("Cover has %d bytes, want nil for a binary over the cap", len(got.Cover))
	}
	if got.Title != "Capped" {
		t.Errorf("Title = %q, want the text metadata intact", got.Title)
	}
}

// FB2 places <description> first and <binary> last, and the walk relies on
// it: the cover's id has to be known before its binary is reached. A
// document ordered the other way round is invalid and gets no cover, the
// same outcome as a coverpage naming an id that does not exist
func TestReadMetadataBinaryBeforeDescriptionIsNotACover(t *testing.T) {
	doc := `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
  <binary id="cover.jpg" content-type="image/jpeg">` + base64.StdEncoding.EncodeToString([]byte("early")) + `</binary>
  <description>
    <title-info>
      <book-title>Inverted</book-title>
      <coverpage><image l:href="#cover.jpg"/></coverpage>
    </title-info>
  </description>
  <body></body>
</FictionBook>`
	got, err := ReadMetadata(buildTestFB2(t, doc))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Cover != nil {
		t.Errorf("Cover = %q, want nil for a binary placed before the description", got.Cover)
	}
	if got.Title != "Inverted" {
		t.Errorf("Title = %q, want %q", got.Title, "Inverted")
	}
}

// The .fb2.zip document cap is the second line of defence, and where it
// lands decides what survives it. Past the description it costs the cover
// only: the text metadata was read and is kept. Inside the description
// there is nothing to keep, so it is an error. Exercised through a small
// cappedReader rather than an archive the size of maxZipDocumentBytes
func TestReadMetadataDocumentCap(t *testing.T) {
	doc := fb2WithBinaries("Capped Document", [][2]string{
		{"illustration.jpg", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 4096))},
		{"cover.jpg", base64.StdEncoding.EncodeToString([]byte("cover"))},
	})
	descriptionEnd := strings.Index(doc, "</description>") + len("</description>")

	t.Run("past the description keeps the text metadata", func(t *testing.T) {
		got, err := readCappedDocument(strings.NewReader(doc), int64(descriptionEnd+512))
		if err != nil {
			t.Fatalf("readMetadata: %v", err)
		}
		if got.Title != "Capped Document" {
			t.Errorf("Title = %q, want %q", got.Title, "Capped Document")
		}
		if got.Cover != nil {
			t.Errorf("Cover = %q, want nil for a cover past the cap", got.Cover)
		}
	})

	t.Run("inside the description is an error", func(t *testing.T) {
		_, err := readCappedDocument(strings.NewReader(doc), int64(descriptionEnd-20))
		if !errors.Is(err, errDocumentTooLarge) {
			t.Fatalf("readMetadata error = %v, want errDocumentTooLarge", err)
		}
	})
}

// With no cover named, reading stops at the end of the description: a
// document that is malformed after that point still yields its metadata
func TestReadMetadataStopsAfterDescriptionWhenNoCoverIsNamed(t *testing.T) {
	doc := `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description>
    <title-info><book-title>Coverless</book-title></title-info>
  </description>
  <body><p>unterminated`
	got, err := ReadMetadata(buildTestFB2(t, doc))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.Title != "Coverless" {
		t.Errorf("Title = %q, want %q", got.Title, "Coverless")
	}
}

// Exactly at the cap is admitted, as it is by internal/epub's reader and by
// cover.Store: the base64 cap is padded to a whole quantum so the three do
// not disagree by a byte about which covers exist
func TestReadMetadataKeepsCoverBinaryExactlyAtTheCap(t *testing.T) {
	want := bytes.Repeat([]byte{0x5a}, cover.MaxCoverBytes)
	path := buildTestFB2(t, fb2WithBinaries("At Cap", [][2]string{{"cover.jpg", base64.StdEncoding.EncodeToString(want)}}))

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !bytes.Equal(got.Cover, want) {
		t.Errorf("Cover has %d bytes, want %d intact", len(got.Cover), len(want))
	}
}

// The cap has to act while the copy fills, not after it. The tokeniser hands
// the whole node over as one token, so copying it, stripping it and appending
// it before consulting the cap cost three more copies of the node — a 711 KiB
// archive measured at a gigabyte. What the walk cannot avoid is the
// tokeniser's own buffer for the node, and TotalAlloc counts every doubling
// of it, so the bound is taken relative to that: the same node skipped as a
// non-cover binary costs the tokeniser alone, and reading it as the cover
// may add only the capped copy (with its own doublings), never the node
func TestReadMetadataOverCapCoverBinaryCostsOnlyTheCappedCopy(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(make([]byte, 32<<20))
	small := base64.StdEncoding.EncodeToString([]byte("small cover"))
	asCover := buildTestFB2(t, fb2WithBinaries("Over Cap", [][2]string{{"cover.jpg", encoded}}))
	skipped := buildTestFB2(t, fb2WithBinaries("Skipped", [][2]string{{"other.jpg", encoded}, {"cover.jpg", small}}))

	measure := func(path string) (Metadata, uint64) {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		got, err := ReadMetadata(path)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("ReadMetadata %s: %v", path, err)
		}
		return got, after.TotalAlloc - before.TotalAlloc
	}

	got, coverCost := measure(asCover)
	if got.Cover != nil {
		t.Errorf("Cover has %d bytes, want nil for a binary over the cap", len(got.Cover))
	}
	got, skipCost := measure(skipped)
	if string(got.Cover) != "small cover" {
		t.Errorf("Cover = %q, want the small cover after the skipped binary", got.Cover)
	}

	t.Logf("node %d bytes: as cover %d bytes allocated, skipped %d", len(encoded), coverCost, skipCost)
	if coverCost > skipCost+3*maxCoverBase64Bytes {
		t.Errorf("reading the node as the cover cost %d bytes over skipping it, want under %d (the capped copy and its growth)", coverCost-skipCost, 3*maxCoverBase64Bytes)
	}
}
