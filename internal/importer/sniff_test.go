package importer

import (
	"archive/zip"
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func TestDetectSuffixReadsTheContentAndNotTheName(t *testing.T) {
	dir := t.TempDir()

	var emptyZip bytes.Buffer
	if err := zip.NewWriter(&emptyZip).Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	var twoFB2 bytes.Buffer
	zw := zip.NewWriter(&twoFB2)
	for _, name := range []string{"one.fb2", "two.fb2"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", name, err)
		}
		w.Write(fb2Bytes("T", "A"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	cases := []struct {
		name    string
		content []byte
		want    string
		wantErr bool
	}{
		{name: "epub", content: epubBytes(t, "Dune", "Herbert", 0), want: ".epub"},
		{name: "fb2 in a zip", content: fb2ZipBytes(t, "Dune", "Herbert"), want: ".fb2.zip"},
		{name: "plain fb2", content: fb2Bytes("Dune", "Herbert"), want: ".fb2"},
		{name: "fb2 behind a BOM", content: append([]byte("\xef\xbb\xbf"), fb2Bytes("Dune", "Herbert")...), want: ".fb2"},
		{name: "fb2 with no declaration", content: []byte("\n  <FictionBook><description/></FictionBook>"), want: ".fb2"},
		{name: "a zip that is neither", content: emptyZip.Bytes(), wantErr: true},
		{name: "an archive of two books", content: twoFB2.Bytes(), wantErr: true},
		{name: "a pdf", content: []byte("%PDF-1.7\n1 0 obj\n"), wantErr: true},
		{name: "empty", content: nil, wantErr: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// The staged name carries no suffix at all, which is the state
			// detectSuffix actually runs in and the point of the check:
			// nothing about the name can be what answered.
			path := writeFile(t, filepath.Join(dir, tt.name), tt.content)

			got, err := detectSuffix(path)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedFormat) {
					t.Fatalf("detectSuffix = %q, %v; want ErrUnsupportedFormat", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("detectSuffix: %v", err)
			}
			if got != tt.want {
				t.Errorf("detectSuffix = %q, want %q", got, tt.want)
			}
		})
	}
}

// An EPUB carrying an .fb2 entry is still an EPUB, whichever order the
// central directory happens to list the two in — which is why the whole
// directory is read before either verdict.
func TestDetectSuffixPrefersTheContainerOverAnFB2Entry(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range []struct{ name, content string }{
		{"OEBPS/extra.fb2", "<FictionBook/>"},
		{"META-INF/container.xml", containerXML},
		{"OEBPS/content.opf", "<package/>"},
	} {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatalf("create %s in zip: %v", entry.name, err)
		}
		w.Write([]byte(entry.content))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	path := writeFile(t, filepath.Join(t.TempDir(), "mixed"), buf.Bytes())
	got, err := detectSuffix(path)
	if err != nil {
		t.Fatalf("detectSuffix: %v", err)
	}
	if got != ".epub" {
		t.Errorf("detectSuffix = %q, want %q", got, ".epub")
	}
}
