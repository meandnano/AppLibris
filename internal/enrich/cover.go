package enrich

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// MaxCoverBytes bounds a fetched cover image's response body, checked
// before the bytes ever reach internal/cover.Store's image.Decode — the one
// place handing a decoder an arbitrary remote image could make this feature
// allocate without bound. Comfortably above a typical cover JPEG/PNG.
const MaxCoverBytes = 512 * 1024

// FetchCover downloads the image at url through client, capped at
// MaxCoverBytes read before any decoding is attempted anywhere. url == ""
// (no cover known) returns no bytes and no error. A non-200 response, a
// transport failure, or a body over the cap are all errors: the caller's
// own answer (the rest of a provider's Metadata) must never be blocked on
// this, so a caller logs and otherwise ignores this error rather than
// treating it as a ByISBN/Search failure.
func FetchCover(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	if url == "" {
		return nil, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build cover request: %w", err)
	}
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
