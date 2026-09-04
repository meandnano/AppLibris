// Package googlebooks is an enrich.Provider backed by the Google Books
// Volumes API (https://www.googleapis.com/books/v1/volumes), per DESIGN.md's
// provider choices for the metadata chain. Anonymous use is allowed at a low
// quota; an optional API key raises it.
package googlebooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"library/internal/enrich"
)

// Timeout bounds one lookup end to end, mirroring internal/openlibrary's
// Timeout — enrichment is a background nicety, not something a person is
// waiting on, so a slow provider is skipped rather than waited out.
const Timeout = 8 * time.Second

const baseURL = "https://www.googleapis.com/books/v1"

const providerName = "googlebooks"

// maxErrorBodyBytes bounds how much of an error response body is read into
// the returned error's text.
const maxErrorBodyBytes = 512

// Client looks books up against the Google Books Volumes API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// New returns a Client with Timeout set on its own *http.Client, per
// internal/resend's precedent of never relying on http.DefaultClient, which
// has none at all. apiKey is optional — an empty string makes every request
// anonymous, at Google's lower unauthenticated quota.
func New(apiKey string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: Timeout},
	}
}

// Name identifies this provider in logs and in the provenance
// field_sources records — a stable identifier, not a display string.
func (c *Client) Name() string { return providerName }

// ByISBN looks a book up by isbn, normalised the same way internal/epub
// normalises a stored ISBN (hyphens and spaces stripped, a trailing
// check-digit X upper-cased) — a book found by internal/fb2 stores its ISBN
// with punctuation intact, and normalising here means that lookup key
// reaches Google Books, and comes back out through toMetadata, in the same
// shape internal/epub already stores.
func (c *Client) ByISBN(ctx context.Context, isbn string) (enrich.Metadata, error) {
	normalized := normalizeISBN(isbn)
	if normalized == "" {
		return enrich.Metadata{}, nil
	}
	return c.search(ctx, "isbn:"+normalized)
}

// Search looks a book up by title and, when known, its first author — the
// fallback the resolver uses when a book has no ISBN. The qualifiers follow
// the Volumes API's own documented query syntax (a space between terms,
// which the query string then carries as a "+").
func (c *Client) Search(ctx context.Context, title string, authors []string) (enrich.Metadata, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return enrich.Metadata{}, nil
	}
	q := "intitle:" + title
	if len(authors) > 0 && authors[0] != "" {
		q += " inauthor:" + authors[0]
	}
	return c.search(ctx, q)
}

// volumesResponse is the subset of the Volumes API's response this provider
// reads. A query that matches nothing is a 200 with totalItems 0 and no
// items at all — the ordinary case for an obscure or mistitled book, not an
// error shape.
type volumesResponse struct {
	TotalItems int      `json:"totalItems"`
	Items      []volume `json:"items"`
}

type volume struct {
	VolumeInfo volumeInfo `json:"volumeInfo"`
}

type volumeInfo struct {
	Title               string               `json:"title"`
	Authors             []string             `json:"authors"`
	Publisher           string               `json:"publisher"`
	PublishedDate       string               `json:"publishedDate"`
	Description         string               `json:"description"`
	IndustryIdentifiers []industryIdentifier `json:"industryIdentifiers"`
	Language            string               `json:"language"`
}

type industryIdentifier struct {
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

// search issues one GET against /volumes with q and turns the response into
// Metadata, implementing the four network cases DESIGN.md draws a hard line
// around: a 200 with no items and a defensive 404 are both "no match", nil
// error; a 429, any 5xx, or a transport/timeout failure are errors the retry
// decorator (internal/enrich) and the resolver's skip-and-continue both
// expect to see as such.
func (c *Client) search(ctx context.Context, q string) (enrich.Metadata, error) {
	query := url.Values{"q": {q}, "maxResults": {"1"}}
	if c.apiKey != "" {
		query.Set("key", c.apiKey)
	}
	reqURL := c.baseURL + "/volumes?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return enrich.Metadata{}, fmt.Errorf("googlebooks: build request: %w", c.redactKey(err))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return enrich.Metadata{}, fmt.Errorf("googlebooks: request failed: %w", c.redactKey(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return enrich.Metadata{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return enrich.Metadata{}, fmt.Errorf("googlebooks: unexpected status %d: %s", resp.StatusCode, c.redactKeyBytes(body))
	}

	var parsed volumesResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return enrich.Metadata{}, fmt.Errorf("googlebooks: parse response: %w", err)
	}
	if len(parsed.Items) == 0 {
		return enrich.Metadata{}, nil
	}

	return toMetadata(parsed.Items[0]), nil
}

// redactKey scrubs a configured API key out of an error's text — request
// build and transport errors (*url.Error, most commonly) embed the full
// request URL, and the key must never reach a log line through one.
func (c *Client) redactKey(err error) error {
	if c.apiKey == "" || err == nil {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), c.apiKey, "REDACTED"))
}

func (c *Client) redactKeyBytes(body []byte) string {
	if c.apiKey == "" {
		return string(body)
	}
	return strings.ReplaceAll(string(body), c.apiKey, "REDACTED")
}

func toMetadata(v volume) enrich.Metadata {
	info := v.VolumeInfo
	return enrich.Metadata{
		Title:         strings.TrimSpace(info.Title),
		Authors:       info.Authors,
		Publisher:     info.Publisher,
		PublishedDate: info.PublishedDate,
		Language:      info.Language,
		ISBN:          bestISBN(info.IndustryIdentifiers),
		Description:   info.Description,
	}
}

// bestISBN prefers the volume's ISBN-13 identifier, falling back to its
// ISBN-10 one — unlike internal/openlibrary's bestISBN, the Volumes API
// labels each identifier's type explicitly, so there is no need to guess
// the edition from its length.
func bestISBN(ids []industryIdentifier) string {
	var isbn10 string
	for _, id := range ids {
		switch id.Type {
		case "ISBN_13":
			return normalizeISBN(id.Identifier)
		case "ISBN_10":
			if isbn10 == "" {
				isbn10 = normalizeISBN(id.Identifier)
			}
		}
	}
	return isbn10
}

// normalizeISBN mirrors internal/epub's own unexported normalizeISBN (and
// internal/openlibrary's copy of the same rule): strips hyphens and spaces
// and upper-cases a trailing check-digit X. Duplicated rather than
// imported, for the same reason internal/openlibrary's copy is — the rule
// is small, stable, and each caller applies it to a different shape of
// input.
func normalizeISBN(raw string) string {
	v := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(raw))
	if v == "" {
		return ""
	}
	if last := v[len(v)-1]; last == 'x' {
		v = v[:len(v)-1] + "X"
	}
	return v
}
