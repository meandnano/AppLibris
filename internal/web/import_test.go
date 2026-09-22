package web

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"library/internal/importer"
	"library/internal/service"
	"library/internal/storage"
	"library/internal/storage/storagetest"
)

const importContainerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

const importOPFTemplate = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
  </metadata>
  <manifest>%s</manifest>
</package>`

const importCoverManifestItem = `<item id="cover-image" href="cover.png" media-type="image/png" properties="cover-image"/>`

// importSolidPNG builds a small valid PNG, mirroring internal/cover's
// helper of the same shape — a cover here only has to decode.
func importSolidPNG(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 20, 30))
	for y := range 30 {
		for x := range 20 {
			img.Set(x, y, color.RGBA{R: 0x66, G: 0x44, B: 0x22, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func importEPUB(t *testing.T, title, author string, padding int) []byte {
	t.Helper()
	return importEPUBWithCover(t, title, author, padding, nil)
}

// importEPUBWithCover declares a cover in the manifest and writes coverBytes
// at it verbatim, so a caller can put something that is not an image there —
// the shape an upload uses to try to choose what the preview route serves.
func importEPUBWithCover(t *testing.T, title, author string, padding int, coverBytes []byte) []byte {
	t.Helper()

	manifest := ""
	if coverBytes != nil {
		manifest = importCoverManifestItem
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": importContainerXML,
		"OEBPS/content.opf":      fmt.Sprintf(importOPFTemplate, title, author, manifest),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		w.Write([]byte(content))
	}
	if coverBytes != nil {
		w, err := zw.Create("OEBPS/cover.png")
		if err != nil {
			t.Fatalf("create cover.png in zip: %v", err)
		}
		w.Write(coverBytes)
	}
	if padding > 0 {
		// Stored rather than deflated, so padding bytes reach the archive
		// one for one: a run of 'x' compresses to nothing, and a test that
		// needs a fixture of a given size would silently get a tiny one.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "OEBPS/pad.bin", Method: zip.Store})
		if err != nil {
			t.Fatalf("create pad.bin in zip: %v", err)
		}
		w.Write(bytes.Repeat([]byte{'x'}, padding))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

// newImportHandler builds the whole route table over a real Stager, since
// what these tests are about is the transport's behaviour against the
// importer's actual answers rather than against a stand-in for them.
func newImportHandler(t *testing.T, maxSize int64) (http.Handler, *storage.DB, string) {
	t.Helper()
	return newImportHandlerWritable(t, maxSize, true)
}

func newImportHandlerWritable(t *testing.T, maxSize int64, writable bool, opts ...service.Option) (http.Handler, *storage.DB, string) {
	t.Helper()

	db := storagetest.Open(t)

	root := t.TempDir()
	libraryDir := filepath.Join(root, "library")
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}
	coversDir := filepath.Join(root, "covers")
	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		t.Fatalf("mkdir covers: %v", err)
	}

	// No Stager is what a library the process cannot write looks like:
	// cmd/server builds one only when its probe succeeds.
	var stager *importer.Stager
	if writable {
		var err error
		stager, err = importer.New(db, importer.Options{
			LibraryDir: libraryDir,
			CoversDir:  coversDir,
			TempDir:    filepath.Join(root, "staging"),
			MaxSize:    maxSize,
		})
		if err != nil {
			t.Fatalf("importer.New: %v", err)
		}
	}

	svc := service.New(db, append([]service.Option{service.WithImporter(stager)}, opts...)...)
	return Routes(svc, coversDir, false, false), db, libraryDir
}

func uploadRequest(t *testing.T, filename string, content []byte, hx bool) *http.Request {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(content)
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/import/file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	return req
}

func upload(t *testing.T, handler http.Handler, filename string, content []byte, hx bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, filename, content, hx))
	return rec
}

// stagedID pulls the id out of a rendered preview, which is how a test
// follows the flow the way a browser does rather than by reaching into the
// importer.
func stagedID(t *testing.T, body string) string {
	t.Helper()
	const marker = `action="/import/`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no staged import in the response:\n%s", body)
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		t.Fatalf("malformed confirm action in the response:\n%s", body)
	}
	return rest[:j]
}

func post(handler http.Handler, path string, hx bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestImportUploadPreviewsTheFile(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	rec := upload(t, handler, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 0), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Dune", "Frank Herbert", "epub", "Dune.epub", "Import", "Discard"} {
		if !strings.Contains(body, want) {
			t.Errorf("the preview does not mention %q:\n%s", want, body)
		}
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "HX-Request") || !strings.Contains(got, "HX-History-Restore-Request") {
		t.Errorf("Vary = %q, want both htmx headers named", got)
	}
}

func TestImportUploadAnswersAFragmentToHTMXAndAPageOtherwise(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)
	book := importEPUB(t, "Dune", "Frank Herbert", 0)

	fragment := upload(t, handler, "Dune.epub", book, true).Body.String()
	if strings.Contains(fragment, "<!doctype html>") {
		t.Errorf("an htmx upload got a whole page:\n%s", fragment)
	}

	// Without htmx the upload redirects to the preview's own URL, so a
	// reload re-reads the stage instead of re-sending the file.
	rec := upload(t, handler, "Dune-2.epub", importEPUB(t, "Dune II", "Frank Herbert", 16), false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/import/") {
		t.Fatalf("Location = %q, want the preview's URL", location)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, location, nil))
	if !strings.Contains(page.Body.String(), "<!doctype html>") {
		t.Errorf("a plain GET of the preview got a fragment:\n%s", page.Body.String())
	}
	if got := page.Header().Get("Vary"); !strings.Contains(got, "HX-Request") {
		t.Errorf("Vary = %q, want HX-Request named", got)
	}
}

func TestImportUploadRefusalsSayWhy(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		content  []byte
		want     string
	}{
		{name: "too large", filename: "Dune.epub", content: bytes.Repeat([]byte("PK\x03\x04"), 4096), want: "larger than"},
		{name: "not a book", filename: "notes.txt", content: []byte("just some text"), want: "not an EPUB or FB2 file"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			handler, _, _ := newImportHandler(t, 1024)

			// The panel comes back carrying the upload form's hx-status:422,
			// which is what lets htmx swap the refusal in past noSwap's 4xx
			fragment := upload(t, handler, tt.filename, tt.content, true)
			if fragment.Code != http.StatusUnprocessableEntity {
				t.Errorf("htmx status = %d, want 422", fragment.Code)
			}
			if !strings.Contains(fragment.Body.String(), `hx-status:422="swap:outerHTML"`) {
				t.Errorf("the refused panel's form does not opt its 422 into swapping:\n%s", fragment.Body.String())
			}
			if !strings.Contains(fragment.Body.String(), tt.want) {
				t.Errorf("the refusal does not say %q:\n%s", tt.want, fragment.Body.String())
			}

			// The full page is a real rejection and says so.
			page := upload(t, handler, tt.filename, tt.content, false)
			if page.Code != http.StatusUnprocessableEntity {
				t.Errorf("full-page status = %d, want 422", page.Code)
			}
			if !strings.Contains(page.Body.String(), tt.want) {
				t.Errorf("the refusal does not say %q:\n%s", tt.want, page.Body.String())
			}
		})
	}
}

func TestImportConfirmLandsOnTheBook(t *testing.T) {
	handler, db, libraryDir := newImportHandler(t, 1<<20)

	id := stagedID(t, upload(t, handler, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 0), true).Body.String())

	rec := post(handler, "/import/"+id+"/confirm", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	redirect := rec.Header().Get("HX-Redirect")
	if !strings.HasPrefix(redirect, "/books/") {
		t.Fatalf("HX-Redirect = %q, want a book URL", redirect)
	}

	if _, err := os.Stat(filepath.Join(libraryDir, "Dune.epub")); err != nil {
		t.Errorf("the library file is not there: %v", err)
	}
	if count, err := db.CountBooks(t.Context()); err != nil || count != 1 {
		t.Errorf("CountBooks = %d, %v; want 1", count, err)
	}
}

func TestImportConfirmRedirectsWithoutHTMX(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	location := upload(t, handler, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 0), false).Header().Get("Location")
	id := strings.TrimPrefix(location, "/import/")

	rec := post(handler, "/import/"+id+"/confirm", false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/books/") {
		t.Errorf("Location = %q, want a book URL", got)
	}
}

// Each import form carries its own request timeout and its own double-submit
// guard: htmx abandons a request after sixty seconds unless the element says
// otherwise, and an upload's window scales with MAX_IMPORT_SIZE while a
// confirm's is derived from importer.IndexTimeout. The dimmed button is
// appearance only — it still answers Enter — so hx-disable is what actually
// refuses the second press, and htmx queues rather than drops it.
//
// Every assertion is scoped to one form's opening tag. Three forms render
// into one preview panel and two of them carry hx-disable, so a check
// against the whole body passes on a neighbour's attribute.
func TestImportFormsBoundTheirOwnRequests(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import", nil))
	uploadPage := rec.Body.String()

	preview := upload(t, handler, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 0), true).Body.String()

	for _, tt := range []struct {
		class string
		body  string
		want  []string
	}{
		{"import__form", uploadPage, []string{`hx-config="timeout:0"`, `hx-disable="find button"`}},
		{"import__confirm-form", preview, []string{`hx-config="timeout:0"`, `hx-disable="find button"`}},
		// Discard writes nothing and answers at once, so it needs no window
		// of its own — only the guard against a second press.
		{"import__discard-form", preview, []string{`hx-disable="find button"`}},
	} {
		tag := openTag(t, tt.body, tt.class)
		for _, want := range tt.want {
			if !strings.Contains(tag, want) {
				t.Errorf("the %s form is missing %s: %s", tt.class, want, tag)
			}
		}
	}
}

// openTag returns the opening tag of the one element carrying class, so an
// assertion about one form cannot be satisfied by another rendered beside it.
func openTag(t *testing.T, body, class string) string {
	t.Helper()

	at := strings.Index(body, `class="`+class+`"`)
	if at < 0 {
		t.Fatalf("no element with class %q:\n%s", class, body)
	}
	// From the tag's own `<`, so an assertion that a tag carries no attribute
	// sees the attributes written before class as well as after it
	start := strings.LastIndex(body[:at], "<")
	if start < 0 {
		t.Fatalf("the tag carrying class %q has no opening bracket:\n%s", class, body)
	}
	end := strings.Index(body[start:], ">")
	if end < 0 {
		t.Fatalf("the tag carrying class %q is unterminated:\n%s", class, body)
	}
	return body[start : start+end]
}

func TestImportPreviewShowsEachVerdictAndOnlyItsButtons(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)
	book := importEPUB(t, "Dune", "Frank Herbert", 0)

	// New: Import is offered and nothing is flagged.
	first := upload(t, handler, "Dune.epub", book, true).Body.String()
	if !strings.Contains(first, "/confirm") {
		t.Errorf("a new book was not offered an Import button:\n%s", first)
	}
	// A refused confirm answers 422, which noSwap drops unless the confirm
	// form itself opts it in
	if tag := openTag(t, first, "import__confirm-form"); !strings.Contains(tag, `hx-status:422="swap:outerHTML"`) {
		t.Errorf("the confirm form does not opt its 422 into swapping: %s", tag)
	}
	if strings.Contains(first, "already in the library") {
		t.Errorf("a new book was flagged as a duplicate:\n%s", first)
	}
	post(handler, "/import/"+stagedID(t, first)+"/confirm", true)

	// Exists: no Import button, and a link to the book instead.
	duplicate := upload(t, handler, "Dune-copy.epub", book, true).Body.String()
	if strings.Contains(duplicate, "/confirm") {
		t.Errorf("a byte-identical duplicate was offered an Import button:\n%s", duplicate)
	}
	if !strings.Contains(duplicate, "already in the library") || !strings.Contains(duplicate, `href="/books/`) {
		t.Errorf("the duplicate verdict does not link to the book:\n%s", duplicate)
	}
	if !strings.Contains(duplicate, "/discard") {
		t.Errorf("the duplicate verdict offers no Discard:\n%s", duplicate)
	}

	// Title match: Import is offered, under a warning.
	edition := upload(t, handler, "Dune-2.epub", importEPUB(t, "Dune", "F. Herbert", 64), true).Body.String()
	if !strings.Contains(edition, "/confirm") {
		t.Errorf("a second edition was not offered an Import button:\n%s", edition)
	}
	if !strings.Contains(edition, "already holds a book called Dune") {
		t.Errorf("the title-match warning is missing:\n%s", edition)
	}
}

func TestImportDiscardPutsTheFormBack(t *testing.T) {
	handler, _, libraryDir := newImportHandler(t, 1<<20)

	id := stagedID(t, upload(t, handler, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 0), true).Body.String())

	rec := post(handler, "/import/"+id+"/discard", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `type="file"`) {
		t.Errorf("a discard did not put the file input back:\n%s", rec.Body.String())
	}

	entries, err := os.ReadDir(libraryDir)
	if err != nil {
		t.Fatalf("read library: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a discarded import reached the library: %v", entries)
	}
}

// A stage that is gone is not a missing page: the person is looking at a
// URL that was right a moment ago, and the input they need is on it.
func TestImportPreviewOfAnExpiredStageExplainsItself(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import/nosuchstage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("the page does not explain the expiry:\n%s", rec.Body.String())
	}
}

func TestImportConfirmOfAnExpiredStageSaysSo(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	rec := post(handler, "/import/nosuchstage/confirm", true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("the refusal does not say the stage expired:\n%s", rec.Body.String())
	}
}

func TestImportIsDisabledWhenTheLibraryIsReadOnly(t *testing.T) {
	handler, _, _ := newImportHandlerWritable(t, 1<<20, false)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "read-only") {
		t.Errorf("the page does not explain why import is off:\n%s", body)
	}
	if strings.Contains(body, `type="file"`) {
		t.Errorf("a disabled page still offers the file input:\n%s", body)
	}
	// The nav link is what the flag actually withholds.
	if strings.Contains(body, `href="/import"`) {
		t.Errorf("the masthead links to a page that can only say no:\n%s", body)
	}

	if got := upload(t, handler, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 0), true); !strings.Contains(got.Body.String(), "read-only") {
		t.Errorf("an upload to a read-only library was not refused:\n%s", got.Body.String())
	}
}

func TestImportNavLinkAppearsWhenImportIsEnabled(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	for _, path := range []string{"/", "/history", "/import"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		if !strings.Contains(body, "Import") {
			t.Errorf("%s does not offer the Import nav entry:\n%s", path, body)
		}
	}
}

func TestEveryImportPOSTRefusesACrossSiteRequest(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	for _, path := range []string{"/import/file", "/import/url", "/import/anything/confirm", "/import/anything/discard"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s from a cross-site page = %d, want 403", path, rec.Code)
		}
	}
}

func TestImportCoverIsServedWithTheTypeADecoderDecided(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	art := importSolidPNG(t)
	id := stagedID(t, upload(t, handler, "Dune.epub", importEPUBWithCover(t, "Dune", "Frank Herbert", 0, art), true).Body.String())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import/"+id+"/cover", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want %q", got, "image/png")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), art) {
		t.Errorf("the route served %d bytes, want the %d embedded", rec.Body.Len(), len(art))
	}
}

func TestImportCoverIs404ForABookWithout(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	id := stagedID(t, upload(t, handler, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 0), true).Body.String())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import/"+id+"/cover", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a book with no cover", rec.Code)
	}
}

// An EPUB manifest can point cover-image at anything in the archive, and
// internal/epub hands those bytes over without deciding they are an image.
// If the route sniffed them, an HTML document would come back as text/html
// from this app's own origin — which is same-origin, the thing sameSiteOnly
// admits. It must never be served at all.
func TestImportCoverRefusesBytesThatAreNotAnImage(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	html := []byte(`<!doctype html><script>fetch("/recipients/remove",{method:"POST"})</script>`)
	body := upload(t, handler, "Dune.epub", importEPUBWithCover(t, "Dune", "Frank Herbert", 0, html), true).Body.String()
	id := stagedID(t, body)

	if strings.Contains(body, "/cover") {
		t.Errorf("the preview offers a cover it should have dropped:\n%s", body)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import/"+id+"/cover", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q: the route served an HTML document from this origin", got)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("<script")) {
		t.Error("the route served the uploaded script back")
	}
}

// The window is sized here rather than only inside extendReadDeadline
// because httptest's recorder has no SetReadDeadline, so that call is a
// no-op under every handler test in this package.
func TestUploadWindowScalesFromTheCapAboveItsFloor(t *testing.T) {
	cases := []struct {
		bytes int64
		want  time.Duration
	}{
		{bytes: 0, want: minUploadWindow},
		{bytes: 1 << 20, want: minUploadWindow},
		{bytes: 29 << 20, want: minUploadWindow}, // still under the floor
		{bytes: 64 << 20, want: 64 * time.Second},
		{bytes: 512 << 20, want: 512 * time.Second},
	}
	for _, tt := range cases {
		if got := uploadWindow(tt.bytes); got != tt.want {
			t.Errorf("uploadWindow(%d) = %s, want %s", tt.bytes, got, tt.want)
		}
	}
	if uploadWindow(64<<20) < minUploadWindow {
		t.Error("the window may only ever lengthen what cmd/server's ReadTimeout already allows")
	}
}

func TestImportFailureLineNamesTheCap(t *testing.T) {
	got := importFailureLine(importer.ErrTooLarge, 64<<20)
	if !strings.Contains(got, humanSize(64<<20)) {
		t.Errorf("importFailureLine = %q, want it to name the cap", got)
	}
	if importFailureLine(io.EOF, 0) != "" {
		t.Errorf("importFailureLine explained an error it cannot describe")
	}
}

// A refusal is decided while the body is still arriving, so the property
// worth pinning is that it survives a real socket: httptest.NewRecorder has
// none and cannot show it either way. It passes with the drain in
// renderImportRejection removed as well, and that is expected rather than a
// weak test — see that function for why the remainder a drain can consume
// is too small to have cost anything.
func TestAnOverCapUploadStillReadsItsRefusalBack(t *testing.T) {
	const cap = 32 << 10

	handler, _, _ := newImportHandler(t, cap)
	server := httptest.NewTestServer(t, handler)
	server.Start()

	// Over the importer's cap and under MaxBytesReader's, which is the
	// case the sentence exists for and the one a drain can fix.
	book := importEPUB(t, "Dune", "Frank Herbert", cap)
	if len(book) <= cap || int64(len(book)) > cap+multipartOverhead {
		t.Fatalf("the fixture is %d bytes; the test needs one between %d and %d", len(book), cap, cap+multipartOverhead)
	}

	req := uploadRequest(t, "Dune.epub", book, true)
	req.URL, _ = url.Parse(server.URL + "/import/file")
	req.RequestURI = ""
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("the refusal never arrived: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the refusal was cut short: %v", err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "larger than") {
		t.Errorf("the response does not name the cap:\n%s", body)
	}
}

// The read-only refusal answers a request whose body is still arriving too,
// which is why the body is bounded above that check rather than below it.
func TestADisabledImportStillReadsItsRefusalBack(t *testing.T) {
	handler, _, _ := newImportHandlerWritable(t, 32<<10, false)
	// A real socket, for the same reason as the test above
	server := httptest.NewTestServer(t, handler)
	server.Start()

	req := uploadRequest(t, "Dune.epub", importEPUB(t, "Dune", "Frank Herbert", 16<<10), true)
	req.URL, _ = url.Parse(server.URL + "/import/file")
	req.RequestURI = ""
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("the refusal never arrived: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the refusal was cut short: %v", err)
	}
	if !strings.Contains(string(body), "read-only") {
		t.Errorf("the response does not explain why import is off:\n%s", body)
	}
}

func TestGetImportNamesBothHTMXHeadersInVary(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import", nil))

	got := rec.Header().Get("Vary")
	if !strings.Contains(got, "HX-Request") || !strings.Contains(got, "HX-History-Restore-Request") {
		t.Errorf("Vary = %q, want both htmx headers named", got)
	}
}

func TestImportFailureLineNamesAFullStagingArea(t *testing.T) {
	if got := importFailureLine(importer.ErrStagingFull, 64<<20); !strings.Contains(got, "discard") {
		t.Errorf("importFailureLine = %q, want it to say what to do about it", got)
	}
}

// slowBody yields its bytes over about `over`, so the handler answers well
// after the write deadline Go installed when the headers arrived.
type slowBody struct {
	data  []byte
	sent  int
	chunk int
	pause time.Duration
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.sent >= len(s.data) {
		return 0, io.EOF
	}
	time.Sleep(s.pause)
	n := min(min(s.chunk, len(p)), len(s.data)-s.sent)
	copy(p, s.data[s.sent:s.sent+n])
	s.sent += n
	return n, nil
}

// Go installs the write deadline once, from the moment the request headers
// were read, and never extends it — so widening only the read window buys a
// large upload the time to arrive and then loses the answer to it. Every
// other test in this package runs against an httptest.Server with no
// WriteTimeout at all, which is exactly why none of them saw this.
func TestASlowUploadStillDeliversItsAnswerUnderAWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler, _, _ := newImportHandler(t, 1<<20)

		// In memory, where the connection's deadlines run on the bubble's
		// clock, so the body can take its time without the test taking it
		server := httptest.NewTestServer(t, handler)
		// Shorter than the body takes to arrive, so the handler renders after
		// it would have expired. Config is read when Client starts serving
		server.Config.WriteTimeout = time.Second
		client := server.Client()

		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormFile("file", "Dune.epub")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		// Padded so the body genuinely outlasts the write deadline below; a
		// small one finishes in milliseconds and proves nothing.
		part.Write(importEPUB(t, "Dune", "Frank Herbert", 64<<10))
		if err := mw.Close(); err != nil {
			t.Fatalf("close multipart writer: %v", err)
		}

		slow := &slowBody{data: body.Bytes(), chunk: 512, pause: 10 * time.Millisecond}
		req, err := http.NewRequest(http.MethodPost, "http://library.test/import/file", slow)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Sec-Fetch-Site", "same-origin")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("the preview never arrived: %v", err)
		}
		defer resp.Body.Close()

		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("the preview was cut short: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(string(answer), "Dune") {
			t.Errorf("the response is not the preview:\n%s", answer)
		}
	})
}

// confirmWindow must outlast what the importer allows a confirm to take, or
// the handler abandons an import the importer is still permitted to be
// making.
func TestConfirmWindowOutlastsTheImportersOwnBudget(t *testing.T) {
	got := confirmWindow(64 << 20)
	if got <= importer.IndexTimeout {
		t.Errorf("confirmWindow = %s, want more than the importer's %s index budget", got, importer.IndexTimeout)
	}
	if got <= uploadWindow(64<<20)+importer.IndexTimeout {
		t.Errorf("confirmWindow = %s, want room to render the response past the copy and the index", got)
	}
}

func TestLibraryPageIsADropTargetWhenImportIsEnabled(t *testing.T) {
	const maxSize = 1 << 20
	handler, _, _ := newImportHandler(t, maxSize)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `data-max-bytes="`+strconv.Itoa(maxSize)+`"`) {
		t.Errorf("the drop target does not carry the import cap:\n%s", body)
	}
	// A plain form, so the drop is the no-JS upload and htmx never takes it
	form := openTag(t, body, "drop__form")
	for _, want := range []string{`method="post"`, `action="/import/file"`, `enctype="multipart/form-data"`} {
		if !strings.Contains(form, want) {
			t.Errorf("the drop form lacks %s: %s", want, form)
		}
	}
	if strings.Contains(form, "hx-") {
		t.Errorf("the drop form carries an htmx attribute: %s", form)
	}
	tooLarge := importFailureLine(importer.ErrTooLarge, maxSize)
	if !strings.Contains(body, html.EscapeString(tooLarge)) {
		t.Errorf("the drop target does not carry the server's own too-large refusal %q:\n%s", tooLarge, body)
	}
	if !strings.Contains(body, dropOneFileLine) {
		t.Errorf("the drop target does not carry the one-file refusal:\n%s", body)
	}
	// The script only ever reveals this markup, so the template's own hidden
	// is what keeps it out of sight — and the hooks are what the script finds
	// it by, so a missing one leaves the first drag throwing instead
	for _, want := range []string{
		`<div class="drop__overlay" data-drop-overlay hidden>`,
		`<p class="drop__prompt" data-drop-state="ready">`,
		`<p class="drop__prompt" data-drop-state="uploading" hidden>`,
		`<div class="drop__errors" role="alert">`,
		`<p class="drop__error" data-drop-refusal="too-large" hidden>`,
		`<p class="drop__error" data-drop-refusal="not-one" hidden>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the drop target is missing %s:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "Or drag a book file onto this page") {
		t.Errorf("the empty library does not mention dropping a file:\n%s", body)
	}
	if !strings.Contains(body, `src="/static/js/drop.js"`) {
		t.Errorf("the library page does not load drop.js:\n%s", body)
	}
}

func TestLibraryPageIsNoDropTargetWhenImportIsDisabled(t *testing.T) {
	handler, _, _ := newImportHandlerWritable(t, 1<<20, false)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if strings.Contains(body, "data-import-drop") {
		t.Errorf("a read-only library renders a drop target:\n%s", body)
	}
	if strings.Contains(body, "Or drag a book file onto this page") {
		t.Errorf("a read-only library offers dropping a file:\n%s", body)
	}
}

// The grid fragment is what a search swaps in, so a target inside it would be
// duplicated or lost on every keystroke
func TestLibraryGridFragmentCarriesNoDropTarget(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	for _, path := range []string{"/", "/?q=dune"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if body := rec.Body.String(); strings.Contains(body, "data-import-drop") {
			t.Errorf("the %s fragment carries the drop target:\n%s", path, body)
		}
	}
}

func TestImportFormPointsAtTheDropTarget(t *testing.T) {
	handler, _, _ := newImportHandler(t, 1<<20)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/import", nil))
	if body := rec.Body.String(); !strings.Contains(body, `drop a file on the <a href="/">library page</a>`) {
		t.Errorf("the import form does not mention dropping on the library page:\n%s", body)
	}
}

// fetchFunc stands in for importer.Fetcher, whose guarded transport refuses
// every address an in-memory or loopback server has
type fetchFunc func(ctx context.Context, rawURL string) (importer.Download, error)

func (f fetchFunc) Fetch(ctx context.Context, rawURL string) (importer.Download, error) {
	return f(ctx, rawURL)
}

// serving answers every link with body under the given name and type
func serving(name string, html bool, body []byte) fetchFunc {
	return func(context.Context, string) (importer.Download, error) {
		return importer.Download{Name: name, HTML: html, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
}

func newLinkHandler(t *testing.T, maxSize int64, fetcher service.LinkFetcher) http.Handler {
	t.Helper()
	handler, _, _ := newImportHandlerWritable(t, maxSize, true, service.WithFetcher(fetcher))
	return handler
}

func linkRequest(link string) *http.Request {
	form := url.Values{"url": {link}}
	req := httptest.NewRequest(http.MethodPost, "/import/url", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func postLink(handler http.Handler, link string) *httptest.ResponseRecorder {
	req := linkRequest(link)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestImportURLRedirectsToThePreview(t *testing.T) {
	handler := newLinkHandler(t, 1<<20, serving("Dune.epub", false, importEPUB(t, "Dune", "Frank Herbert", 0)))

	rec := postLink(handler, "https://books.test/Dune.epub")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303:\n%s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/import/") {
		t.Fatalf("Location = %q, want the preview's URL", location)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, location, nil))
	for _, want := range []string{"Dune", "Frank Herbert", "Dune.epub"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("the preview does not mention %q:\n%s", want, page.Body.String())
		}
	}
}

// Behind RequireFetchMetadata, as cmd/server mounts every route, so a
// script or a plain-HTTP page cannot make the server fetch
func TestImportURLRefusesARequestWithoutFetchMetadata(t *testing.T) {
	calls := 0
	handler := RequireFetchMetadata(newLinkHandler(t, 1<<20, fetchFunc(func(context.Context, string) (importer.Download, error) {
		calls++
		return importer.Download{}, importer.ErrTooLarge
	})))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, linkRequest("https://books.test/Dune.epub"))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if calls != 0 {
		t.Errorf("the link was fetched %d times, want never", calls)
	}
}

func TestImportURLRefusalsSayWhy(t *testing.T) {
	const maxSize = 32 << 10
	cases := []struct {
		name    string
		link    string
		fetcher service.LinkFetcher
		want    string
	}{
		{
			name:    "unsupported link",
			link:    "ftp://books.test/Dune.epub",
			fetcher: serving("Dune.epub", false, nil),
			want:    "That isn&#39;t a supported link.",
		},
		{
			name: "declared too large",
			link: "https://books.test/Dune.epub",
			fetcher: fetchFunc(func(context.Context, string) (importer.Download, error) {
				return importer.Download{}, importer.ErrTooLarge
			}),
			want: "larger than the " + humanSize(maxSize) + " import limit",
		},
		{
			name:    "counted too large",
			link:    "https://books.test/Dune.epub",
			fetcher: serving("Dune.epub", false, bytes.Repeat([]byte("PK\x03\x04"), maxSize)),
			want:    "larger than the " + humanSize(maxSize) + " import limit",
		},
		{
			name: "status",
			link: "https://books.test/Dune.epub",
			fetcher: fetchFunc(func(context.Context, string) (importer.Download, error) {
				return importer.Download{}, &importer.StatusError{Code: http.StatusNotFound}
			}),
			want: "The server answered 404.",
		},
		{
			name:    "web page",
			link:    "https://books.test/dune",
			fetcher: serving("dune", true, []byte("<!doctype html><title>Dune</title>")),
			want:    "That link opens a web page, not a book file.",
		},
		{
			name:    "not a book",
			link:    "https://books.test/notes.txt",
			fetcher: serving("notes.txt", false, []byte("just some text")),
			want:    "That is not an EPUB or FB2 file.",
		},
		{
			// The production fetcher, so the refusal is the guard's own
			// rather than a stand-in's. 127.0.0.1 is refused before any
			// connection, which is why no server is needed
			name:    "private address",
			link:    "http://127.0.0.1:1/Dune.epub",
			fetcher: importer.NewFetcher(maxSize),
			want:    "Couldn&#39;t download that link.",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := postLink(newLinkHandler(t, maxSize, tt.fetcher), tt.link)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("the refusal does not say %q:\n%s", tt.want, rec.Body.String())
			}
		})
	}
}

func TestImportURLGivesUpOnADownloadPastItsWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler := newLinkHandler(t, 1<<20, fetchFunc(func(ctx context.Context, _ string) (importer.Download, error) {
			<-ctx.Done()
			return importer.Download{}, fmt.Errorf("download request failed: %w", ctx.Err())
		}))

		rec := postLink(handler, "https://books.test/Dune.epub")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "The download took too long.") {
			t.Errorf("the refusal does not blame the wait:\n%s", rec.Body.String())
		}
	})
}

func TestImportURLRefusesASecondLinkWhileOneDownloads(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	book := importEPUB(t, "Dune", "Frank Herbert", 0)
	handler := newLinkHandler(t, 1<<20, fetchFunc(func(context.Context, string) (importer.Download, error) {
		close(started)
		<-release
		return importer.Download{Name: "Dune.epub", Body: io.NopCloser(bytes.NewReader(book))}, nil
	}))

	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- postLink(handler, "https://books.test/Dune.epub") }()
	<-started

	rec := postLink(handler, "https://books.test/Other.epub")
	close(release)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Another link is still downloading.") {
		t.Errorf("the refusal does not say a download is running:\n%s", rec.Body.String())
	}
	if got := (<-first).Code; got != http.StatusSeeOther {
		t.Errorf("the first download answered %d, want 303", got)
	}
}

func TestImportURLWhenImportIsDisabledSaysSo(t *testing.T) {
	handler, _, _ := newImportHandlerWritable(t, 1<<20, false, service.WithFetcher(serving("Dune.epub", false, nil)))

	rec := postLink(handler, "https://books.test/Dune.epub")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "read-only") {
		t.Errorf("the refusal does not explain why import is off:\n%s", rec.Body.String())
	}
}

// A query or fragment is where a signed download link keeps its token, and
// the url.Error a refused dial produces prints the whole link
func TestImportURLLogsTheLinkWithoutItsQuery(t *testing.T) {
	logs := captureLog(t)
	handler := newLinkHandler(t, 1<<20, importer.NewFetcher(1<<20))

	postLink(handler, "http://127.0.0.1:1/books/Dune.epub?token=s3cr3t#frag")

	out := logs.String()
	if !strings.Contains(out, "import from link failed") {
		t.Fatalf("the failure was not logged:\n%s", out)
	}
	for _, want := range []string{"scheme=http", "host=127.0.0.1:1", "path=/books/Dune.epub"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"s3cr3t", "frag"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log carries %q from the link:\n%s", leak, out)
		}
	}
}

func TestDownloadWindowOutlastsTheUploadOfTheSameFile(t *testing.T) {
	if got := downloadWindow(64 << 20); got <= uploadWindow(64<<20) {
		t.Errorf("downloadWindow = %s, want room for the connection past the body's own window", got)
	}
}
