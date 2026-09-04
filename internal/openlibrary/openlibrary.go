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

// coverBaseURL is Open Library's separate covers host — a cover is fetched
// by numeric cover_i id, not by anything search.json itself serves.
const coverBaseURL = "https://covers.openlibrary.org"

const providerName = "openlibrary"

// userAgent identifies this client to Open Library, whose API terms ask
// for a descriptive one with a contact address and throttle or block the
// generic Go default. A block would arrive as a 403 or 429 indistinguishable
// from any other transient failure, so the resolver would go on silently
// skipping this provider for every book.
const userAgent = "library/1.0 (+https://github.com/meandnano/AppLibris)"

// maxErrorBodyBytes bounds how much of an error response body is read into
// the returned error's text — enough for a message, not an unbounded read
// of whatever a misbehaving upstream sends back.
const maxErrorBodyBytes = 512

// maxResponseBytes bounds the success path the same way maxErrorBodyBytes
// bounds the failure one: a search document is a third party's response
// body, and decoding it straight off the wire lets a misbehaving or
// hijacked upstream allocate in this process without limit. Generous
// against a real /search.json answer for limit=1 — even a heavily-reissued
// work's document is well under this — so the cap only ever trips on a
// response nothing here should be parsing anyway.
const maxResponseBytes = 4 * 1024 * 1024

// Client looks books up against Open Library's Search API.
type Client struct {
	baseURL      string
	coverBaseURL string
	httpClient   *http.Client
}

// New returns a Client with Timeout set on its own *http.Client, per
// internal/resend's precedent of never relying on http.DefaultClient, which
// has none at all.
func New() *Client {
	return &Client{
		baseURL:      baseURL,
		coverBaseURL: coverBaseURL,
		httpClient:   &http.Client{Timeout: Timeout},
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
	CoverID          int      `json:"cover_i"`
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

	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport or timeout failure is the retryable case: nothing
		// about the request itself was rejected.
		return enrich.Metadata{}, fmt.Errorf("openlibrary: request failed: %w: %w", enrich.ErrRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return enrich.Metadata{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if isRetryableStatus(resp.StatusCode) {
			return enrich.Metadata{}, fmt.Errorf("openlibrary: unexpected status %d: %s: %w", resp.StatusCode, body, enrich.ErrRetryable)
		}
		return enrich.Metadata{}, fmt.Errorf("openlibrary: unexpected status %d: %s", resp.StatusCode, body)
	}

	var parsed searchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&parsed); err != nil {
		return enrich.Metadata{}, fmt.Errorf("openlibrary: parse response: %w", err)
	}
	if len(parsed.Docs) == 0 {
		return enrich.Metadata{}, nil
	}

	return c.toMetadata(parsed.Docs[0]), nil
}

// toMetadata converts doc, naming its cover's URL on the separate
// covers.openlibrary.org host when doc carries a cover id. Nothing is
// downloaded here: internal/enrich's Worker fetches the image, and only
// for a book whose cover_path is actually empty.
func (c *Client) toMetadata(doc searchDoc) enrich.Metadata {
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
		m.Language = isoLanguage(doc.Language[0])
	}
	if doc.CoverID > 0 {
		m.CoverURL = fmt.Sprintf("%s/b/id/%d-L.jpg", c.coverBaseURL, doc.CoverID)
	}
	return m
}

// marcToISO639 maps the MARC language codes search.json answers with
// ("eng", "rus") onto the two-letter ISO 639-1 codes internal/epub,
// internal/fb2 and internal/googlebooks all produce. Without it the
// language column holds "eng" for one book and "en" for the next, rendered
// raw side by side on the detail page. Only the languages this library
// plausibly contains are listed; anything else passes through unchanged,
// which is still better than a wrong guess.
var marcToISO639 = map[string]string{
	"ara": "ar", "ces": "cs", "chi": "zh", "dan": "da", "dut": "nl",
	"eng": "en", "fin": "fi", "fre": "fr", "ger": "de", "gre": "el",
	"heb": "he", "hun": "hu", "ita": "it", "jpn": "ja", "kor": "ko",
	"lat": "la", "nor": "no", "pol": "pl", "por": "pt", "rus": "ru",
	"spa": "es", "swe": "sv", "tur": "tr", "ukr": "uk",
}

func isoLanguage(code string) string {
	if iso, ok := marcToISO639[strings.ToLower(strings.TrimSpace(code))]; ok {
		return iso
	}
	return code
}

// isRetryableStatus reports whether status is one another attempt could
// plausibly answer differently: 429 (asked to slow down) and any 5xx. A
// 4xx other than that is the server rejecting this request in particular,
// and will reject the next one identically.
func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
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
