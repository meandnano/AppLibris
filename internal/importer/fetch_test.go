package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// testFetcher builds the production Fetcher and hands it the in-memory
// server's own transport, so the redirect policy and header handling under
// test are the ones NewFetcher sets
func testFetcher(t *testing.T, maxSize int64, handler http.HandlerFunc) *Fetcher {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	f := NewFetcher(maxSize)
	useFetchTransport(f, server.Client().Transport)
	return f
}

func fetchAll(t *testing.T, d Download) string {
	t.Helper()
	defer d.Body.Close()
	b, err := io.ReadAll(d.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestFetchReturnsTheBodyUnread(t *testing.T) {
	var gotAgent string
	f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/epub+zip")
		io.WriteString(w, "book bytes")
	})

	d, err := f.Fetch(context.Background(), "https://books.test/dune.epub")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := fetchAll(t, d); got != "book bytes" {
		t.Errorf("body = %q, want the whole response", got)
	}
	if d.HTML {
		t.Error("HTML = true for an epub response")
	}
	if gotAgent != fetchUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotAgent, fetchUserAgent)
	}
}

func TestFetchFlagsAnHTMLResponse(t *testing.T) {
	cases := map[string]bool{
		"text/html; charset=utf-8": true,
		"TEXT/HTML":                true,
		"application/xhtml+xml":    false,
		"application/octet-stream": false,
		"":                         false,
	}
	for contentType, want := range cases {
		t.Run(contentType, func(t *testing.T) {
			f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Content-Type"] = []string{contentType}
				io.WriteString(w, "<html></html>")
			})
			d, err := f.Fetch(context.Background(), "https://books.test/page")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			d.Body.Close()
			if d.HTML != want {
				t.Errorf("HTML = %v, want %v", d.HTML, want)
			}
		})
	}
}

func TestFetchNamesTheDownload(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		disposition string
		want        string
	}{
		{"disposition filename", "/get?id=7", `attachment; filename="Dune.epub"`, "Dune.epub"},
		{"disposition filename*", "/get", `attachment; filename*=UTF-8''%D0%94%D1%8E%D0%BD%D0%B0.fb2`, "Дюна.fb2"},
		{"filename* over filename", "/get", `attachment; filename="plain.epub"; filename*=UTF-8''fancy%20name.epub`, "fancy name.epub"},
		{"disposition without a filename", "/books/Dune.epub", "attachment", "Dune.epub"},
		{"malformed disposition", "/books/Dune.epub", `attachment; filename="unterminated`, "Dune.epub"},
		{"unescaped path segment", "/books/The%20Left%20Hand.epub", "", "The Left Hand.epub"},
		{"query is not the name", "/books/dune.epub?token=secret", "", "dune.epub"},
		{"trailing slash", "/books/", "", ""},
		{"root", "/", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
				if tc.disposition != "" {
					w.Header().Set("Content-Disposition", tc.disposition)
				}
				io.WriteString(w, "x")
			})
			d, err := f.Fetch(context.Background(), "https://books.test"+tc.path)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			d.Body.Close()
			if d.Name != tc.want {
				t.Errorf("Name = %q, want %q", d.Name, tc.want)
			}
		})
	}
}

// The name is the final URL's, since the link a person pasted is often an
// opaque download endpoint and the CDN it bounces to names the file
func TestFetchNamesTheDownloadAfterTheLastRedirect(t *testing.T) {
	f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "books.test" {
			http.Redirect(w, r, "https://cdn.test/files/Dune.epub", http.StatusFound)
			return
		}
		io.WriteString(w, "book")
	})
	d, err := f.Fetch(context.Background(), "https://books.test/download/42")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := fetchAll(t, d); got != "book" {
		t.Errorf("body = %q, want the CDN's", got)
	}
	if d.Name != "Dune.epub" {
		t.Errorf("Name = %q, want the final URL's segment", d.Name)
	}
}

func TestFetchRefusesANonOKStatus(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError, http.StatusNoContent} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			})
			_, err := f.Fetch(context.Background(), "https://books.test/dune.epub")
			var status *StatusError
			if !errors.As(err, &status) {
				t.Fatalf("Fetch error = %v, want a *StatusError", err)
			}
			if status.Code != code {
				t.Errorf("Code = %d, want %d", status.Code, code)
			}
		})
	}
}

// The handler declares a length past the cap and then holds the body back
// until the request is abandoned, so a Fetch that read a byte of it would
// never return
func TestFetchRefusesADeclaredLengthPastTheCapWithoutReadingTheBody(t *testing.T) {
	const maxSize = 1024
	f := testFetcher(t, maxSize, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(maxSize+1))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	d, err := f.Fetch(context.Background(), "https://books.test/huge.epub")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Fetch error = %v, want ErrTooLarge", err)
	}
	if d.Body != nil {
		t.Error("Body is set on a refused download")
	}
}

func TestFetchAcceptsADeclaredLengthAtTheCap(t *testing.T) {
	const maxSize = 16
	f := testFetcher(t, maxSize, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", maxSize))
	})
	d, err := f.Fetch(context.Background(), "https://books.test/exact.epub")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := fetchAll(t, d); len(got) != maxSize {
		t.Errorf("body length = %d, want %d", len(got), maxSize)
	}
}

func TestFetchStopsAfterFiveRedirects(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		http.Redirect(w, r, fmt.Sprintf("https://books.test/hop/%d", n), http.StatusFound)
	})
	_, err := f.Fetch(context.Background(), "https://books.test/start")
	if err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("Fetch error = %v, want the redirect cap", err)
	}
	if requests != 6 {
		t.Errorf("requests = %d, want the first plus five hops", requests)
	}
}

func TestFetchAllowsFiveRedirects(t *testing.T) {
	f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if n < 5 {
			http.Redirect(w, r, fmt.Sprintf("https://books.test/hop/%d", n+1), http.StatusFound)
			return
		}
		io.WriteString(w, "book")
	})
	d, err := f.Fetch(context.Background(), "https://books.test/hop/0")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := fetchAll(t, d); got != "book" {
		t.Errorf("body = %q, want the fifth hop's", got)
	}
}

func TestFetchRefusesARedirectToAnotherScheme(t *testing.T) {
	for _, target := range []string{"ftp://books.test/dune.epub", "file:///etc/passwd", "gopher://books.test/1"} {
		t.Run(target, func(t *testing.T) {
			f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target)
				w.WriteHeader(http.StatusFound)
			})
			_, err := f.Fetch(context.Background(), "https://books.test/dune.epub")
			if err == nil || !strings.Contains(err.Error(), "is not http or https") {
				t.Fatalf("Fetch error = %v, want the scheme refused", err)
			}
		})
	}
}

// A hop to another host carries neither the previous URL, which can hold a
// signed token, nor a cookie the first host set
func TestFetchSendsNoRefererOrCookieOnARedirectedHop(t *testing.T) {
	var referer, cookie string
	cdnRequests := 0
	f := testFetcher(t, 1<<20, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "books.test" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s3cret", Domain: "test", Path: "/"})
			http.Redirect(w, r, "https://cdn.test/files/dune.epub", http.StatusFound)
			return
		}
		cdnRequests++
		referer = r.Header.Get("Referer")
		cookie = r.Header.Get("Cookie")
		io.WriteString(w, "book")
	})
	d, err := f.Fetch(context.Background(), "https://books.test/download?token=abc")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	d.Body.Close()
	if cdnRequests != 1 {
		t.Fatalf("cdn requests = %d, want 1", cdnRequests)
	}
	if referer != "" {
		t.Errorf("Referer = %q, want none", referer)
	}
	if cookie != "" {
		t.Errorf("Cookie = %q, want none", cookie)
	}
}

// The guard is the real one here, which is why the server listens on a
// socket: the in-memory transport never dials, and the guard hangs on the
// dial
func TestFetchRefusesALoopbackAddress(t *testing.T) {
	requests := 0
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		io.WriteString(w, "book")
	}))
	server.Start()

	_, err := NewFetcher(1<<20).Fetch(context.Background(), server.URL+"/dune.epub")
	if err == nil || !strings.Contains(err.Error(), "is loopback") {
		t.Fatalf("Fetch error = %v, want the loopback address refused", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 — the dial must be refused before any bytes move", requests)
	}
}

// https reaches the guard only because DialTLSContext is unset, so the
// transport connects through DialContext and then wraps
func TestFetchRefusesAnHTTPSLoopbackAddress(t *testing.T) {
	requests := 0
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		io.WriteString(w, "book")
	}))
	server.StartTLS()

	_, err := NewFetcher(1<<20).Fetch(context.Background(), server.URL+"/dune.epub")
	if err == nil || !strings.Contains(err.Error(), "is loopback") {
		t.Fatalf("Fetch error = %v, want the loopback address refused", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 — the dial is refused before the TLS handshake", requests)
	}
}

// A public link that redirects inward is the SSRF the redirect policy
// alone does not stop, since it allows a hop to any host. The guard runs
// again on the hop's dial, which is why the target is refused with no
// request reaching it
func TestFetchRefusesARedirectToAPrivateAddress(t *testing.T) {
	inner := 0
	internal := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner++
		io.WriteString(w, "secret")
	}))
	internal.Start()

	for _, tt := range []struct{ target, want string }{
		{internal.URL + "/admin", "is loopback"},
		{"http://169.254.169.254/latest/meta-data/", "is link-local"},
		{"http://10.0.0.1/", "is private"},
		{"http://100.100.100.100/", "reserved range"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			public := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, tt.target, http.StatusFound)
			}))
			public.Start()

			f := NewFetcher(1 << 20)
			dialHostAt(f, "books.test:80", public.Listener.Addr().String())
			_, err := f.Fetch(context.Background(), "http://books.test/dune.epub")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Fetch error = %v, want the hop refused with %q", err, tt.want)
			}
		})
	}
	if inner != 0 {
		t.Errorf("the internal server saw %d requests, want 0", inner)
	}
}
