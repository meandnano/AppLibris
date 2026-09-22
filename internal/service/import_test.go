package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"library/internal/importer"
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
  <manifest></manifest>
</package>`

func importTestEPUB(t *testing.T, title, author string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": importContainerXML,
		"OEBPS/content.opf":      fmt.Sprintf(importOPFTemplate, title, author),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

// newImportTestService builds a Service over a real Stager, in the shape
// newMetadataTestService uses: what these tests are about is the mapping
// this layer makes from the importer's answers to the transport's, which a
// stand-in importer could not produce.
func newImportTestService(t *testing.T) (*Service, *storage.DB, string) {
	t.Helper()

	db := storagetest.Open(t)

	root := t.TempDir()
	libraryDir := filepath.Join(root, "library")
	coversDir := filepath.Join(root, "covers")
	for _, dir := range []string{libraryDir, coversDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	stager, err := importer.New(db, importer.Options{
		LibraryDir: libraryDir,
		CoversDir:  coversDir,
		TempDir:    filepath.Join(root, "staging"),
		MaxSize:    1 << 20,
	})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	return New(db, WithImporter(stager)), db, libraryDir
}

func TestStageImportShapesThePreview(t *testing.T) {
	svc, _, _ := newImportTestService(t)

	preview, err := svc.StageImport(context.Background(), "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}

	if preview.Title != "Dune" {
		t.Errorf("Title = %q, want %q", preview.Title, "Dune")
	}
	if !slices.Equal(preview.Authors, []string{"Frank Herbert"}) {
		t.Errorf("Authors = %v, want [Frank Herbert]", preview.Authors)
	}
	if preview.Format != "epub" {
		t.Errorf("Format = %q, want %q", preview.Format, "epub")
	}
	if preview.LibraryName != "Dune.epub" {
		t.Errorf("LibraryName = %q, want %q", preview.LibraryName, "Dune.epub")
	}
	if preview.Verdict != importer.VerdictNew {
		t.Errorf("Verdict = %q, want %q", preview.Verdict, importer.VerdictNew)
	}
	if preview.ID == "" {
		t.Error("the preview carries no id, so nothing can confirm it")
	}

	// And it reads back by id, which is what the no-JS path's redirect
	// lands on.
	again, err := svc.StagedImport(context.Background(), preview.ID)
	if err != nil {
		t.Fatalf("StagedImport: %v", err)
	}
	if again == nil || again.Title != "Dune" {
		t.Errorf("StagedImport = %+v, want the staged preview", again)
	}
}

func TestConfirmImportReportsTheBookItIndexed(t *testing.T) {
	ctx := context.Background()
	svc, db, libraryDir := newImportTestService(t)

	preview, err := svc.StageImport(ctx, "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}

	bookID, indexed, err := svc.ConfirmImport(ctx, preview.ID)
	if err != nil {
		t.Fatalf("ConfirmImport: %v", err)
	}
	if !indexed {
		t.Fatal("indexed is false for an import that succeeded")
	}
	if bookID == 0 {
		t.Fatal("ConfirmImport named no book")
	}
	if _, err := os.Stat(filepath.Join(libraryDir, "Dune.epub")); err != nil {
		t.Errorf("the library file is not there: %v", err)
	}
	book, err := db.FindBookByID(ctx, bookID)
	if err != nil || book == nil {
		t.Fatalf("FindBookByID = %+v, %v", book, err)
	}
}

// The one outcome that is neither success nor failure, and the reason this
// method has three results: the bytes are in the library and the index
// write failed, so there is no book to redirect to but nothing to undo
// either. The handler needs those apart, and a nil error is what says "the
// file is safe" — the importer has already logged the failure at Warn.
func TestConfirmImportReportsAnUnindexedFileAsNeitherFailureNorSuccess(t *testing.T) {
	ctx := context.Background()
	svc, db, libraryDir := newImportTestService(t)

	preview, err := svc.StageImport(ctx, "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}
	db.Close()

	bookID, indexed, err := svc.ConfirmImport(ctx, preview.ID)
	if err != nil {
		t.Fatalf("ConfirmImport = %v, want a nil error: the file reached the library", err)
	}
	if indexed {
		t.Error("indexed is true although the index write failed")
	}
	if bookID != 0 {
		t.Errorf("ConfirmImport named book %d, want none: nothing was indexed", bookID)
	}
	if _, err := os.Stat(filepath.Join(libraryDir, "Dune.epub")); err != nil {
		t.Errorf("the library file was discarded over an index error: %v", err)
	}
}

func TestDiscardImportDropsTheStage(t *testing.T) {
	ctx := context.Background()
	svc, _, libraryDir := newImportTestService(t)

	preview, err := svc.StageImport(ctx, "Dune.epub", bytes.NewReader(importTestEPUB(t, "Dune", "Frank Herbert")))
	if err != nil {
		t.Fatalf("StageImport: %v", err)
	}
	if err := svc.DiscardImport(ctx, preview.ID); err != nil {
		t.Fatalf("DiscardImport: %v", err)
	}

	staged, err := svc.StagedImport(ctx, preview.ID)
	if err != nil {
		t.Fatalf("StagedImport: %v", err)
	}
	if staged != nil {
		t.Errorf("StagedImport = %+v, want nil after a discard", staged)
	}
	entries, err := os.ReadDir(libraryDir)
	if err != nil {
		t.Fatalf("read library: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a discarded import reached the library: %v", entries)
	}
}

// Absent is not an error at this layer, the same contract GetBook keeps, so
// the handler turns it into a 404 the same way.
func TestStagedImportOfAnUnknownIDIsNotAnError(t *testing.T) {
	svc, _, _ := newImportTestService(t)

	staged, err := svc.StagedImport(context.Background(), "nosuchstage")
	if err != nil {
		t.Fatalf("StagedImport on an unknown id = %v, want nil", err)
	}
	if staged != nil {
		t.Errorf("StagedImport = %+v, want nil", staged)
	}
}

// A Service built without an importer — every test in this package but
// these, and cmd/server when the library cannot be written — answers every
// import method rather than panicking on a nil field.
func TestEveryImportMethodRefusesWithoutAnImporter(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Open(t)

	svc := New(db)

	if svc.ImportEnabled() {
		t.Error("ImportEnabled is true with no importer")
	}
	if got := svc.MaxImportBytes(); got != 0 {
		t.Errorf("MaxImportBytes = %d, want 0", got)
	}
	if data, contentType := svc.StagedCover("anything"); data != nil || contentType != "" {
		t.Errorf("StagedCover = %d bytes, %q; want nothing", len(data), contentType)
	}

	if _, err := svc.StageImport(ctx, "Dune.epub", strings.NewReader("")); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("StageImport = %v, want ErrImportDisabled", err)
	}
	if _, err := svc.StagedImport(ctx, "anything"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("StagedImport = %v, want ErrImportDisabled", err)
	}
	if _, _, err := svc.ConfirmImport(ctx, "anything"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("ConfirmImport = %v, want ErrImportDisabled", err)
	}
	if err := svc.DiscardImport(ctx, "anything"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("DiscardImport = %v, want ErrImportDisabled", err)
	}
	if _, err := svc.StageURL(ctx, "https://books.test/Dune.epub"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("StageURL = %v, want ErrImportDisabled", err)
	}
}

// A Stager exists exactly when importing is available: cmd/server builds
// one only for a writable library, so there is no disabled Stager to ask.
func TestImportEnabledFollowsWhetherAStagerWasGiven(t *testing.T) {
	db := storagetest.Open(t)

	root := t.TempDir()
	stager, err := importer.New(db, importer.Options{
		LibraryDir: root,
		CoversDir:  root,
		TempDir:    filepath.Join(root, "staging"),
		MaxSize:    64 << 20,
	})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}

	svc := New(db, WithImporter(stager))
	if !svc.ImportEnabled() {
		t.Error("ImportEnabled is false though a Stager was given")
	}
	if got := svc.MaxImportBytes(); got != 64<<20 {
		t.Errorf("MaxImportBytes = %d, want the configured cap", got)
	}
}

// memoryFetcher stands in for importer.Fetcher, whose guarded transport
// cannot be swapped from outside its package. It keeps the parts of Fetch
// this layer reacts to: a body that stops with the request's context, the
// status refusal, the HTML flag and a name from the URL
type memoryFetcher struct {
	client *http.Client
	calls  atomic.Int32
}

func (f *memoryFetcher) Fetch(ctx context.Context, rawURL string) (importer.Download, error) {
	f.calls.Add(1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return importer.Download{}, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return importer.Download{}, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return importer.Download{}, &importer.StatusError{Code: resp.StatusCode}
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return importer.Download{
		Name: path.Base(resp.Request.URL.Path),
		HTML: mediaType == "text/html",
		Body: resp.Body,
	}, nil
}

// newLinkTestService is newImportTestService with a fetcher reaching
// handler on the in-memory network. It answers the staging directory, so a
// test can check a refused download left nothing behind
func newLinkTestService(t *testing.T, handler http.HandlerFunc) (*Service, *memoryFetcher, string) {
	t.Helper()

	db := storagetest.Open(t)
	root := t.TempDir()
	stagingDir := filepath.Join(root, "staging")
	stager, err := importer.New(db, importer.Options{
		LibraryDir: root,
		CoversDir:  root,
		TempDir:    stagingDir,
		MaxSize:    1 << 20,
	})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	fetcher := &memoryFetcher{client: httptest.NewTestServer(t, handler).Client()}
	return New(db, WithImporter(stager), WithFetcher(fetcher)), fetcher, stagingDir
}

func assertStagingEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("staging directory holds %v, want nothing", entries)
	}
}

func TestStageURLPreviewsTheDownloadUnderItsFetchedName(t *testing.T) {
	book := importTestEPUB(t, "Dune", "Frank Herbert")
	svc, _, _ := newLinkTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/epub+zip")
		w.Write(book)
	})

	preview, err := svc.StageURL(context.Background(), "  https://books.test/files/Children%20of%20Dune.epub\n")
	if err != nil {
		t.Fatalf("StageURL: %v", err)
	}
	if preview.Title != "Dune" {
		t.Errorf("Title = %q, want the embedded title", preview.Title)
	}
	if preview.LibraryName != "Children of Dune.epub" {
		t.Errorf("LibraryName = %q, want the fetched name", preview.LibraryName)
	}

	again, err := svc.StagedImport(context.Background(), preview.ID)
	if err != nil || again == nil {
		t.Fatalf("StagedImport = %+v, %v; want the staged preview", again, err)
	}
}

func TestStageURLRefusesAnUnsupportedLinkWithoutFetching(t *testing.T) {
	links := map[string]string{
		"ftp":         "ftp://books.test/Dune.epub",
		"file":        "file:///etc/passwd",
		"javascript":  "javascript:alert(1)",
		"no scheme":   "books.test/Dune.epub",
		"no host":     "https:///Dune.epub",
		"port only":   "http://:8080/Dune.epub",
		"opaque":      "http:books.test",
		"userinfo":    "https://reader:secret@books.test/Dune.epub",
		"user only":   "https://reader@books.test/Dune.epub",
		"unparseable": "https://books.test/%zz",
		"empty":       "",
		"too long":    "https://books.test/" + strings.Repeat("a", 2<<10),
	}
	svc, fetcher, _ := newLinkTestService(t, func(w http.ResponseWriter, r *http.Request) {})

	for name, link := range links {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.StageURL(context.Background(), link); !errors.Is(err, ErrUnsupportedLink) {
				t.Errorf("StageURL(%q) = %v, want ErrUnsupportedLink", link, err)
			}
		})
	}
	if n := fetcher.calls.Load(); n != 0 {
		t.Errorf("the fetcher was asked %d times for links that were refused", n)
	}
}

func TestStageURLWithoutAFetcherIsDisabled(t *testing.T) {
	svc, _, _ := newImportTestService(t)
	if _, err := svc.StageURL(context.Background(), "https://books.test/Dune.epub"); !errors.Is(err, ErrImportDisabled) {
		t.Errorf("StageURL = %v, want ErrImportDisabled", err)
	}
}

func TestStageURLRefusesASecondDownloadWhileOneRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		book := importTestEPUB(t, "Dune", "Frank Herbert")
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		svc, _, _ := newLinkTestService(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow.epub" {
				entered <- struct{}{}
				<-release
			}
			w.Write(book)
		})

		first := make(chan error, 1)
		go func() {
			_, err := svc.StageURL(context.Background(), "https://books.test/slow.epub")
			first <- err
		}()
		<-entered

		if _, err := svc.StageURL(context.Background(), "https://books.test/other.epub"); !errors.Is(err, ErrDownloadBusy) {
			t.Errorf("second StageURL = %v, want ErrDownloadBusy", err)
		}

		close(release)
		if err := <-first; err != nil {
			t.Fatalf("first StageURL: %v", err)
		}
		if _, err := svc.StageURL(context.Background(), "https://books.test/other.epub"); err != nil {
			t.Errorf("StageURL after the first finished = %v, want the slot free again", err)
		}
	})
}

// No Content-Length, so nothing refuses it before the body: the count in
// Stage is what stops it
func TestStageURLStopsAnUndeclaredOverCapBody(t *testing.T) {
	svc, _, stagingDir := newLinkTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for range 20 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})

	if _, err := svc.StageURL(context.Background(), "https://books.test/huge.epub"); !errors.Is(err, importer.ErrTooLarge) {
		t.Fatalf("StageURL = %v, want ErrTooLarge", err)
	}
	assertStagingEmpty(t, stagingDir)
}

func TestStageURLGivesUpOnASlowDripAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, stagingDir := newLinkTestService(t, func(w http.ResponseWriter, r *http.Request) {
			for {
				if _, err := w.Write([]byte("x")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(time.Second):
				}
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		_, err := svc.StageURL(ctx, "https://books.test/drip.epub")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StageURL = %v, want the deadline", err)
		}
		if errors.Is(err, importer.ErrUnsupportedFormat) {
			t.Errorf("StageURL = %v, which blames the truncated bytes for the wait", err)
		}
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Errorf("gave up after %v, want exactly the 30s deadline", elapsed)
		}
		assertStagingEmpty(t, stagingDir)
	})
}

func TestStageURLSaysAWebPageIsOne(t *testing.T) {
	cases := map[string]struct {
		contentType string
		webPage     bool
	}{
		"html":  {"text/html; charset=utf-8", true},
		"plain": {"text/plain", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _, stagingDir := newLinkTestService(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Write([]byte("<!doctype html><title>Download Dune</title>"))
			})

			_, err := svc.StageURL(context.Background(), "https://books.test/dune")
			if !errors.Is(err, importer.ErrUnsupportedFormat) {
				t.Fatalf("StageURL = %v, want ErrUnsupportedFormat", err)
			}
			if got := errors.Is(err, ErrWebPage); got != tc.webPage {
				t.Errorf("errors.Is(err, ErrWebPage) = %v, want %v", got, tc.webPage)
			}
			assertStagingEmpty(t, stagingDir)
		})
	}
}

// Fetch's own refusals reach the caller unwrapped, for the transport to
// name
func TestStageURLPassesAStatusRefusalThrough(t *testing.T) {
	svc, _, _ := newLinkTestService(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	_, err := svc.StageURL(context.Background(), "https://books.test/gone.epub")
	var status *importer.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound {
		t.Fatalf("StageURL = %v, want a 404 StatusError", err)
	}
}
