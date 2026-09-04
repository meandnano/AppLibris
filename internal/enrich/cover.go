package enrich

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// MaxCoverBytes bounds a fetched cover image's response body, checked
// before the bytes ever reach internal/cover.Store's image.Decode — the one
// place handing a decoder an arbitrary remote image could make this feature
// allocate without bound. Comfortably above a typical cover JPEG/PNG.
const MaxCoverBytes = 512 * 1024

// maxCoverRedirects bounds how far a cover URL may bounce before the fetch
// gives up — the same order as net/http's own default, restated because
// setting CheckRedirect replaces that default outright.
const maxCoverRedirects = 5

// coverUserAgent identifies this client to whichever host serves a cover,
// for the same reason internal/openlibrary and internal/googlebooks each
// set one: Open Library's terms ask for a descriptive agent and throttle
// Go's generic default, and covers.openlibrary.org — the host its own
// cover URLs name — is theirs. A block there arrives as an ordinary fetch
// failure, so every Open Library cover would silently go unstored with
// nothing pointing at the cause.
const coverUserAgent = "library/1.0 (+https://github.com/meandnano/AppLibris)"

// coverSchemeAllowed reports whether a URL is one a cover fetch may follow.
func coverSchemeAllowed(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// CheckCoverRedirect is the CheckRedirect a client passed to FetchCover
// should carry. Without it the scheme check below guards only the first
// URL, and every hop after it — chosen by whatever host answered, not by
// the provider whose response named the cover — is followed unexamined.
func CheckCoverRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxCoverRedirects {
		return fmt.Errorf("stopped after %d redirects", maxCoverRedirects)
	}
	if !coverSchemeAllowed(req.URL.Scheme) {
		return fmt.Errorf("cover redirect scheme %q is not http or https", req.URL.Scheme)
	}
	return nil
}

// FetchCover downloads the image at rawURL through client, capped at
// MaxCoverBytes read before any decoding is attempted anywhere. rawURL ==
// "" (no cover known) returns no bytes and no error. A non-200 response, a
// transport failure, or a body over the cap are all errors: the caller's
// own answer (the rest of a provider's Metadata) must never be blocked on
// this, so a caller logs and otherwise ignores this error rather than
// treating it as a ByISBN/Search failure.
func FetchCover(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	if rawURL == "" {
		return nil, nil
	}

	// The URL comes out of a third party's response body, so the scheme is
	// checked rather than assumed: http and https are the only ones a cover
	// can legitimately use, and refusing the rest keeps a hostile or
	// mangled response from turning this into a fetch of file:// or any
	// other scheme a future http.Client transport might be taught.
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse cover url: %w", err)
	}
	if !coverSchemeAllowed(parsed.Scheme) {
		return nil, fmt.Errorf("cover url scheme %q is not http or https", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build cover request: %w", err)
	}
	req.Header.Set("User-Agent", coverUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cover request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cover request: unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxCoverBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read cover body: %w", err)
	}
	if len(data) > MaxCoverBytes {
		return nil, fmt.Errorf("cover response exceeds %d bytes", MaxCoverBytes)
	}
	return data, nil
}
