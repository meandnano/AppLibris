package importer

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"library/internal/netguard"
)

const (
	fetchDialTimeout           = 10 * time.Second
	fetchTLSHandshakeTimeout   = 10 * time.Second
	fetchResponseHeaderTimeout = 15 * time.Second

	// maxFetchRedirects is how many hops are followed. A download link
	// bounces once or twice to a CDN, so more than this is a loop or a
	// host walking the fetch around
	maxFetchRedirects = 5
)

// fetchUserAgent is the agent every outbound client of this app sends, so
// a host that throttles Go's generic default treats a download no worse
// than a cover fetch
const fetchUserAgent = "library/1.0 (+https://github.com/meandnano/AppLibris)"

// Fetcher downloads a book from a link a person pasted. Every connection
// it makes, redirect hops included, goes through netguard, since the app
// has no login and a link is otherwise a request at any address the
// deployment can reach
type Fetcher struct {
	maxSize int64
	client  *http.Client
}

// Download is a response Fetch accepted. Body is unread, so the one count
// of it is Stage's, and the caller closes it
type Download struct {
	// Name is the filename the server offered, else the last segment of the
	// final URL's path, else empty. It is unsanitised: Stage's name
	// derivation is the one sanitiser
	Name string
	// HTML reports a text/html response, so a refusal of its content can
	// say the link opens a web page
	HTML bool
	Body io.ReadCloser
}

// StatusError is a response other than 200. The code is safe to show,
// since only public hosts are ever reached
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("importer: the server answered %d", e.Code)
}

// NewFetcher returns a Fetcher refusing a declared length past maxSize
func NewFetcher(maxSize int64) *Fetcher {
	// Cloned so the TLS and idle-connection defaults survive. Proxy is
	// cleared because through a proxy the guard would check the proxy's
	// address rather than the host the link names, and DialTLSContext stays
	// unset so https dials through the guard too
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = netguard.DialContext(netguard.RefusePrivateAddress, fetchDialTimeout)
	transport.TLSHandshakeTimeout = fetchTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = fetchResponseHeaderTimeout

	// No Timeout and no Jar: the caller's context bounds the whole
	// download, sized from the cap, and a link that needs a session is not
	// a direct link to a file
	return &Fetcher{
		maxSize: maxSize,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: checkFetchRedirect,
		},
	}
}

// checkFetchRedirect allows a hop to another host, since download links
// routinely bounce to a CDN and the guard checks every hop's address
// anyway
func checkFetchRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxFetchRedirects {
		return fmt.Errorf("stopped after %d redirects", maxFetchRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect scheme %q is not http or https", req.URL.Scheme)
	}
	// The previous URL can carry a signed token the next host has no
	// business seeing
	req.Header.Del("Referer")
	return nil
}

// Fetch requests rawURL and answers the response unread, refusing a status
// other than 200 and a declared length past the cap before any body byte
// is read
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Download, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Download{}, fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", fetchUserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return Download{}, fmt.Errorf("download request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return Download{}, &StatusError{Code: resp.StatusCode}
	}
	// A missing length is -1 and passes; Stage's count stops that body at
	// the cap instead
	if resp.ContentLength > f.maxSize {
		resp.Body.Close()
		return Download{}, ErrTooLarge
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return Download{
		Name: downloadName(resp),
		HTML: mediaType == "text/html",
		Body: resp.Body,
	}, nil
}

// downloadName reads the name from the response that answered, so a
// redirect to a CDN names the file by the URL that served it.
// mime.ParseMediaType decodes filename* into filename, preferring it
func downloadName(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if name := params["filename"]; name != "" {
			return name
		}
	}
	p := resp.Request.URL.Path
	return p[strings.LastIndex(p, "/")+1:]
}
