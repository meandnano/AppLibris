// Package googlebooks is an enrich.Provider backed by the Google Books
// Volumes API (https://www.googleapis.com/books/v1/volumes), per DESIGN.md's
// provider choices for the metadata chain. Anonymous use is allowed at a low
// quota; an optional API key raises it.
package googlebooks

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
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

// userAgent identifies this client rather than leaving Go's generic
// default, matching internal/openlibrary. It carries no API key: the key
// travels in the query string and is redacted out of every error.
const userAgent = "library/1.0 (+https://github.com/meandnano/AppLibris)"

// maxErrorBodyBytes bounds how much of an error response body is read into
// the returned error's text.
const maxErrorBodyBytes = 512

// maxResponseBytes bounds the success path the same way maxErrorBodyBytes
// bounds the failure one: a volumes document is a third party's response
// body, and decoding it straight off the wire lets a misbehaving or
// hijacked upstream allocate in this process without limit. Generous
// against a real maxResults=1 answer, so the cap only ever trips on a
// response nothing here should be parsing anyway.
const maxResponseBytes = 4 * 1024 * 1024

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
// fallback the resolver uses when a book has no ISBN. Each qualifier's
// value is quoted, which is load-bearing rather than cosmetic: the Volumes
// API binds intitle: to the single token following it, so an unquoted
// multi-word title would constrain only its first word and let the rest
// drift into free-text terms — matching a different book entirely, whose
// publisher and ISBN would then be written under this one's provenance.
func (c *Client) Search(ctx context.Context, title string, authors []string) (enrich.Metadata, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return enrich.Metadata{}, nil
	}
	q := `intitle:` + quoteTerm(title)
	if len(authors) > 0 && authors[0] != "" {
		q += ` inauthor:` + quoteTerm(authors[0])
	}
	return c.search(ctx, q)
}

// quoteTerm wraps a qualifier's value in the double quotes that bind it as
// one phrase, dropping any quotes already in the value — an embedded one
// would close the phrase early and turn the remainder into stray terms.
func quoteTerm(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, "") + `"`
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
	ImageLinks          imageLinks           `json:"imageLinks"`
}

type industryIdentifier struct {
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

// imageLinks is the Volumes API's cover URLs, largest first. thumbnail is
// the only one present on every volume that has a cover at all, but it is
// roughly 128px wide — well under internal/cover's 400px target, which
// never upscales — so the larger sizes are preferred when the volume
// carries them and a Google-sourced cover is not needlessly blurry.
type imageLinks struct {
	ExtraLarge string `json:"extraLarge"`
	Large      string `json:"large"`
	Medium     string `json:"medium"`
	Small      string `json:"small"`
	Thumbnail  string `json:"thumbnail"`
}

// best picks the largest cover URL the volume offers, upgrading it to
// https: Google answers these as plain http, and cover bytes that end up
// served from /covers/ should not arrive over cleartext from a host that
// serves TLS on the same name.
func (l imageLinks) best() string {
	for _, candidate := range []string{l.ExtraLarge, l.Large, l.Medium, l.Small, l.Thumbnail} {
		if candidate == "" {
			continue
		}
		// Cut the prefix rather than replacing the first match anywhere:
		// an imageLinks value carrying another URL in a query parameter
		// would otherwise have that one rewritten instead of the scheme.
		if rest, ok := strings.CutPrefix(candidate, "http://"); ok {
			return "https://" + rest
		}
		return candidate
	}
	return ""
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

	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport or timeout failure is the retryable case: nothing
		// about the request itself was rejected.
		return enrich.Metadata{}, fmt.Errorf("googlebooks: request failed: %w: %w", enrich.ErrRetryable, c.redactKey(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return enrich.Metadata{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		// A 403 is Google's over-quota and rejected-key answer as well as
		// its forbidden one, and none of the three is helped by asking
		// twice — only 429 and 5xx are.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return enrich.Metadata{}, fmt.Errorf("googlebooks: unexpected status %d: %s: %w", resp.StatusCode, c.redactKeyBytes(body), enrich.ErrRetryable)
		}
		return enrich.Metadata{}, fmt.Errorf("googlebooks: unexpected status %d: %s", resp.StatusCode, c.redactKeyBytes(body))
	}

	var parsed volumesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&parsed); err != nil {
		return enrich.Metadata{}, fmt.Errorf("googlebooks: parse response: %w", err)
	}
	if len(parsed.Items) == 0 {
		return enrich.Metadata{}, nil
	}

	return c.toMetadata(parsed.Items[0]), nil
}

// redactKey scrubs a configured API key out of an error's text — request
// build and transport errors (*url.Error, most commonly) embed the full
// request URL, and the key must never reach a log line through one.
//
// It wraps rather than replaces, so errors.Is and errors.As still reach
// the original: without Unwrap, whether context.Canceled were detectable
// on a transport failure would depend on whether an API key happened to be
// configured, which is exactly the kind of configuration-shaped difference
// in error handling nobody would think to test for.
func (c *Client) redactKey(err error) error {
	if c.apiKey == "" || err == nil {
		return err
	}
	return redactedError{err: err, text: strings.ReplaceAll(err.Error(), c.apiKey, "REDACTED")}
}

type redactedError struct {
	err  error
	text string
}

func (e redactedError) Error() string { return e.text }
func (e redactedError) Unwrap() error { return e.err }

func (c *Client) redactKeyBytes(body []byte) string {
	if c.apiKey == "" {
		return string(body)
	}
	return strings.ReplaceAll(string(body), c.apiKey, "REDACTED")
}

// toMetadata converts v, naming its cover's URL from the imageLinks
// Google's own response already carries (unlike Open Library, there is no
// separate host to build). Nothing is downloaded here: internal/enrich's
// Worker fetches the image, and only for a book whose cover_path is
// actually empty.
func (c *Client) toMetadata(v volume) enrich.Metadata {
	info := v.VolumeInfo
	m := enrich.Metadata{
		Title:         strings.TrimSpace(info.Title),
		Authors:       info.Authors,
		Publisher:     info.Publisher,
		PublishedDate: info.PublishedDate,
		Language:      info.Language,
		ISBN:          bestISBN(info.IndustryIdentifiers),
		Description:   plainText(info.Description),
	}
	m.CoverURL = info.ImageLinks.best()
	return m
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

// blockTags are the tags whose boundary is a line break in the plain text
// a description column holds. Google's markup is shallow — paragraphs,
// line breaks and the odd list — so the rest carry no structure worth
// preserving and are simply dropped.
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "tr": true, "h1": true,
	"h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// plainText renders one of Google's HTML-formatted description strings as
// the plain text books.description holds. The Volumes API documents
// volumeInfo.description as HTML ("simple formatting elements, such as b,
// i and br tags"), and nothing downstream renders it as markup:
// html/template escapes the detail page's description, so leaving the tags
// in shows a reader a literal "<p>", and the edit textarea then offers
// them the same markup to hand-fix. Open Library's description is plain to
// begin with, which is why this lives here rather than in
// internal/enrich's own sanitizeValue.
//
// Entities are unescaped only after the tags are gone, so text that was
// itself escaped markup ("&lt;b&gt;") survives as the literal characters
// an author wrote rather than being stripped as a tag.
func plainText(raw string) string {
	if !strings.ContainsAny(raw, "<&") {
		return raw
	}

	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '<' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		// A '<' that starts nothing tag-shaped is a character in the
		// description, not markup: "a < b" must survive intact.
		end, name := tagAt(raw, i)
		if end < 0 {
			b.WriteByte(raw[i])
			i++
			continue
		}
		if blockTags[name] {
			b.WriteByte('\n')
		}
		i = end
	}

	text := html.UnescapeString(b.String())
	return strings.TrimSpace(collapseBlankLines(text))
}

// tagAt reports the index just past the tag starting at raw[i] (which the
// caller has already checked is '<') along with its lower-cased name, or
// -1 when what follows is not tag-shaped.
func tagAt(raw string, i int) (int, string) {
	j := i + 1
	if j < len(raw) && raw[j] == '/' {
		j++
	}
	start := j
	for j < len(raw) && isTagNameByte(raw[j]) {
		j++
	}
	if j == start {
		return -1, ""
	}
	name := strings.ToLower(raw[start:j])
	for ; j < len(raw); j++ {
		if raw[j] == '>' {
			return j + 1, name
		}
	}
	// An unterminated '<' runs to the end of the string, which is a
	// truncated description rather than a tag.
	return -1, ""
}

func isTagNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// collapseBlankLines caps a run of newlines at two, since an opening and a
// closing block tag each contribute one and a paragraph break needs only
// the pair.
func collapseBlankLines(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	newlines := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			newlines++
			if newlines > 2 {
				continue
			}
		} else {
			newlines = 0
		}
		b.WriteByte(text[i])
	}
	return b.String()
}
