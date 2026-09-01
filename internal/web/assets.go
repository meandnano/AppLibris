package web

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// noDirFS hides directories from http.FileServer, which would otherwise
// generate a browsable index for any directory lacking an index.html.
// Serving one is not a leak — DESIGN.md binds this to a trusted network —
// but the covers directory listing every content hash in the library is
// surface nobody asked for.
type noDirFS struct{ fs http.FileSystem }

func (f noDirFS) Open(name string) (http.File, error) {
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, fs.ErrNotExist
	}
	return file, nil
}

// staticETags maps an embedded asset's lookup path (e.g.
// "static/css/app.css", the form http.FileServer resolves a request to)
// to a strong ETag derived from its content. embed.FS reports a zero
// ModTime, so http.FileServer emits no Last-Modified and a browser has
// nothing to revalidate against — the stylesheet would otherwise be
// re-sent in full on every page load. Computed once, since the embedded
// content is fixed at build time.
var staticETags = sync.OnceValue(buildStaticETags)

func buildStaticETags() map[string]string {
	etags := make(map[string]string)
	fs.WalkDir(staticFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := staticFS.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		etags[path] = fmt.Sprintf(`"%x"`, sum[:8])
		return nil
	})
	return etags
}

// staticHandler serves the embedded static assets with directory listings
// suppressed and a content-derived ETag so browsers can revalidate rather
// than re-fetching in full. max-age is deliberately short: asset filenames
// are stable across releases, so a long max-age would serve a stale file
// after a deploy with no way to bust it — the ETag is what makes repeat
// loads within that window cheap, not the max-age.
func staticHandler() http.Handler {
	fileServer := http.FileServer(noDirFS{http.FS(staticFS)})
	etags := staticETags()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag, ok := etags[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		fileServer.ServeHTTP(w, r)
	})
}

// coversHandler serves stored cover thumbnails with directory listings
// suppressed and a bounded, non-immutable max-age. cover.Store names each
// file dir/<bookContentHash>.jpg — the *book's* hash, not a hash of the
// resized/JPEG-encoded thumbnail bytes actually served at that path. So the
// URL is stable only as long as the extraction/resize/encode pipeline in
// internal/cover doesn't change: a future resize-quality or encoder change,
// or a regeneration of an existing cover under that pipeline, can overwrite
// different bytes at the same URL. That rules out Cache-Control: immutable,
// which promises the opposite. A day-long max-age still eliminates the
// redundant per-visit revalidation round trip this handler exists to avoid
// (measured at 500 conditional requests per full scroll on a 500-book
// library) while bounding how long a legitimately changed thumbnail can be
// stale, rather than promising it away for a year; http.FileServer's own
// Last-Modified handling (from the file's mtime, untouched by this
// wrapper) is what lets a client revalidate once that window elapses.
func coversHandler(coversDir string) http.Handler {
	fileServer := http.FileServer(noDirFS{http.Dir(coversDir)})
	return http.StripPrefix("/covers/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fileServer.ServeHTTP(w, r)
	}))
}
