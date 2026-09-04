// Package openlibrary is an enrich.Provider backed by Open Library's public
// Search API (https://openlibrary.org/search.json) — no API key, no
// registration, per DESIGN.md's provider choices for the metadata chain.
package openlibrary

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

// Timeout bounds one lookup end to end. Enrichment is a background nicety,
// not something a person is waiting on, so a slow provider is skipped
// rather than waited out — a few seconds, not internal/resend's
// multi-minute SendTimeout, which has to cover an attachment upload this
// never does.
const Timeout = 8 * time.Second

const baseURL = "https://openlibrary.org"

const providerName = "openlibrary"

// maxErrorBodyBytes bounds how much of an error response body is read into
// the returned error's text — enough for a message, not an unbounded read
// of whatever a misbehaving upstream sends back.
const maxErrorBodyBytes = 512

// Client looks books up against Open Library's Search API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New returns a Client with Timeout set on its own *http.Client, per
// internal/resend's precedent of never relying on http.DefaultClient, which
// has none at all.
func New() *Client {
	return &Client{
		baseURL:    baseURL,
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
// reaches Open Library, and comes back out through toMetadata, in the same
// shape internal/epub already stores.
func (c *Client) ByISBN(ctx context.Context, isbn string) (enrich.Metadata, error) {
	normalized := normalizeISBN(isbn)
	if normalized == "" {
		return enrich.Metadata{}, nil
	}
	return c.search(ctx, url.Values{"isbn": {normalized}, "limit": {"1"}})
}

// Search looks a book up by title and, when known, its first author — the
// fallback the resolver uses when a book has no ISBN.
func (c *Client) Search(ctx context.Context, title string, authors []string) (enrich.Metadata, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return enrich.Metadata{}, nil
	}
	q := url.Values{"title": {title}, "limit": {"1"}}
	if len(authors) > 0 && authors[0] != "" {
		q.Set("author", authors[0])
	}
	return c.search(ctx, q)
}

// searchResponse is the subset of openlibrary.org/search.json's response
// this provider reads. The endpoint has no separate "not found" shape: a
// query that matches nothing is a 200 with an empty Docs, the ordinary case
// for an obscure or mistitled book.
type searchResponse struct {
	Docs []searchDoc `json:"docs"`
}

type searchDoc struct {
	Title            string   `json:"title"`
	AuthorName       []string `json:"author_name"`
	Publisher        []string `json:"publisher"`
	PublishDate      []string `json:"publish_date"`
	FirstPublishYear int      `json:"first_publish_year"`
	Language         []string `json:"language"`
	ISBN             []string `json:"isbn"`
}

// search issues one GET against /search.json with query and turns the
// response into Metadata, implementing the four network cases DESIGN.md
// draws a hard line around: a 200 with no docs and a defensive 404 are both
// "no match", nil error; a 429, any 5xx, or a transport/timeout failure are
// errors the retry decorator (internal/enrich) and the resolver's
// skip-and-continue both expect to see as such.
func (c *Client) search(ctx context.Context, query url.Values) (enrich.Metadata, error) {
	reqURL := c.baseURL + "/search.json?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return enrich.Metadata{}, fmt.Errorf("openlibrary: build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return enrich.Metadata{}, fmt.Errorf("openlibrary: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return enrich.Metadata{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return enrich.Metadata{}, fmt.Errorf("openlibrary: unexpected status %d: %s", resp.StatusCode, body)
	}

	var parsed searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return enrich.Metadata{}, fmt.Errorf("openlibrary: parse response: %w", err)
	}
	if len(parsed.Docs) == 0 {
		return enrich.Metadata{}, nil
	}

	return toMetadata(parsed.Docs[0]), nil
}

func toMetadata(doc searchDoc) enrich.Metadata {
	m := enrich.Metadata{
		Title:   strings.TrimSpace(doc.Title),
		Authors: doc.AuthorName,
		ISBN:    bestISBN(doc.ISBN),
	}
	if len(doc.Publisher) > 0 {
		m.Publisher = doc.Publisher[0]
	}
	switch {
	case len(doc.PublishDate) > 0:
		m.PublishedDate = doc.PublishDate[0]
	case doc.FirstPublishYear > 0:
		m.PublishedDate = fmt.Sprintf("%d", doc.FirstPublishYear)
	}
	if len(doc.Language) > 0 {
		m.Language = doc.Language[0]
	}
	return m
}

// bestISBN picks the search result's ISBN-13 form when one is present,
// falling back to the first ISBN-10-shaped value — search.json's isbn list
// mixes both editions' identifiers with no scheme markers of its own, so
// there's no equivalent of internal/epub's scheme-first preference to apply
// here, only length.
func bestISBN(isbns []string) string {
	var best string
	for _, raw := range isbns {
		n := normalizeISBN(raw)
		switch len(n) {
		case 13:
			return n
		case 10:
			if best == "" {
				best = n
			}
		}
	}
	return best
}

// normalizeISBN mirrors internal/epub's own unexported normalizeISBN:
// strips hyphens and spaces and upper-cases a trailing check-digit X. It is
// duplicated rather than imported — internal/storage's own
// normalizeIfISBNShaped makes the same choice — because the rule is small,
// stable, and each caller applies it to a different shape of input.
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
