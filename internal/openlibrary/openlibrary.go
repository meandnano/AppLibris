// Package openlibrary is an enrich.Provider backed by Open Library's public
// APIs — no API key, no registration, per docs/notes/enrichment.md's provider choices for
// the metadata chain.
//
// It reads two different endpoints, because Open Library models a *work*
// (the book as written) separately from an *edition* (one publication of
// it), and books.language and books.published_date are edition facts:
//
//   - ByISBN uses the Read API (/api/volumes/brief/isbn/{isbn}.json). An
//     ISBN names one edition, so an edition-scoped answer is available and
//     is the only correct one.
//   - Search uses /search.json, which answers with works. It therefore
//     deliberately reports neither language nor publication date — see
//     toMetadata.
package openlibrary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"library/internal/enrich"
	"library/internal/storage"
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

// coverURL builds the address of a cover by its numeric id.
//
// default=false is the load-bearing part. Without it a cover id with no
// image behind it is answered 200 with a stand-in rather than 404 —
// measured 2026-09-10 against id 999999999, which answers 200 and 43 bytes
// of 1x1 GIF bare, and 404 with the parameter. Nothing downstream can tell
// a stand-in from a cover: enrich.FetchCover sees a 200 and some bytes, and
// whatever survives cover.Store is written under this provider's name and,
// by the missing rule, never reconsidered. A 404 is a failed fetch
// FetchCover already handles, so the book keeps an honest empty cover and
// stays in the missing set for the next provider and the next run. A stale
// cover_i is ordinary in search results, so this is the common case rather
// than a corner.
//
// One helper for both call sites — the search document's cover_i and the
// edition's covers[0] — so the parameter cannot be added to one and
// forgotten on the other.
func (c *Client) coverURL(id int) string {
	return fmt.Sprintf("%s/b/id/%d-L.jpg?default=false", c.coverBaseURL, id)
}

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
// work's document is well under this, and a Read API record for one ISBN
// runs to tens of kilobytes — so the cap only ever trips on a response
// nothing here should be parsing anyway.
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
		httpClient:   &http.Client{Timeout: Timeout, CheckRedirect: enrich.CheckLookupRedirect},
	}
}

// Name identifies this provider in logs and in the provenance
// field_sources records — a stable identifier, not a display string.
func (c *Client) Name() string { return providerName }

// ByISBN looks a book up by isbn, normalised the same way internal/epub
// normalises a stored ISBN (hyphens and spaces stripped, a trailing
// check-digit X upper-cased) — a book found by internal/fb2 stores its ISBN
// with punctuation intact, and normalising here means that lookup key
// reaches Open Library, and comes back out in the same shape internal/epub
// already stores.
//
// It goes to the Read API rather than /search.json because an ISBN names
// one edition, and /search.json answers about the *work*: its language
// field lists every language any edition was ever published in (31 of
// them, beginning "bul", for an English printing of The Hobbit) and its
// date is the work's first publication, 75 years before the edition the
// ISBN identifies.
func (c *Client) ByISBN(ctx context.Context, isbn string) (enrich.Metadata, error) {
	normalized := storage.NormalizeISBN(isbn)
	if normalized == "" {
		return enrich.Metadata{}, nil
	}
	return c.edition(ctx, normalized)
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
	Title      string   `json:"title"`
	AuthorName []string `json:"author_name"`
	Publisher  []string `json:"publisher"`
	ISBN       []string `json:"isbn"`
	CoverID    int      `json:"cover_i"`
}

// search issues one GET against /search.json with query and turns the
// response into Metadata.
func (c *Client) search(ctx context.Context, query url.Values) (enrich.Metadata, error) {
	body, err := c.get(ctx, c.baseURL+"/search.json?"+query.Encode())
	if err != nil || body == nil {
		return enrich.Metadata{}, err
	}

	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return enrich.Metadata{}, fmt.Errorf("openlibrary: parse response: %w", err)
	}
	if len(parsed.Docs) == 0 {
		return enrich.Metadata{}, nil
	}

	return c.toMetadata(parsed.Docs[0]), nil
}

// toMetadata converts a /search.json document, naming its cover's URL on
// the separate covers.openlibrary.org host when doc carries a cover id.
// Nothing is downloaded here: internal/enrich's Worker fetches the image,
// and only for a book whose cover_path is actually empty.
//
// It deliberately reports no language and no publication date. Both exist
// in the response and both describe the *work*: `language` lists every
// language any edition was published in, and `first_publish_year` is when
// the work first appeared, not this book. Leaving them empty is what makes
// them still missing, so enrich.Resolve offers them to the next provider
// and a person can still fill them by hand — where a wrong value reads as
// answered and is never reconsidered. It is the rule internal/epub follows
// in never substituting a creation date for a publication one, and
// internal/fb2 in preferring publish-info/year over title-info/date; a
// provider is not entitled to a looser standard than a file parser.
func (c *Client) toMetadata(doc searchDoc) enrich.Metadata {
	m := enrich.Metadata{
		Title:   strings.TrimSpace(doc.Title),
		Authors: doc.AuthorName,
		ISBN:    bestISBN(doc.ISBN),
	}
	if len(doc.Publisher) > 0 {
		m.Publisher = doc.Publisher[0]
	}
	if doc.CoverID > 0 {
		m.CoverURL = c.coverURL(doc.CoverID)
	}
	return m
}

// readAPIResponse is the subset of the Read API's response this provider
// reads. Both blocks are needed and neither is redundant: `data` is the
// only place author *names* appear (an edition record lists author
// references, and for many editions — The Hobbit among them — no authors at
// all, since authorship belongs to the work), while `details.details` is
// the raw edition record and the only place the language and the
// description appear.
type readAPIResponse struct {
	Records map[string]readAPIRecord `json:"records"`
}

type readAPIRecord struct {
	Data    readAPIData `json:"data"`
	Details struct {
		Details editionRecord `json:"details"`
	} `json:"details"`
}

type readAPIData struct {
	Title   string `json:"title"`
	Authors []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Publishers []struct {
		Name string `json:"name"`
	} `json:"publishers"`
	PublishDate string `json:"publish_date"`
}

type editionRecord struct {
	Title      string   `json:"title"`
	Publishers []string `json:"publishers"`
	// PublishDate is free text, exactly as books.published_date is: "2012",
	// "March 2012" and "2012-03-01" are all real values, so it is passed
	// through unparsed rather than normalised into a lie.
	PublishDate string `json:"publish_date"`
	Languages   []struct {
		// Key is a reference, "/languages/eng", not a bare code.
		Key string `json:"key"`
	} `json:"languages"`
	ISBN13  []string `json:"isbn_13"`
	ISBN10  []string `json:"isbn_10"`
	Covers  []int    `json:"covers"`
	Authors []struct {
		Name string `json:"name"`
	} `json:"authors"`
	// Description is either a {"type", "value"} object or a bare string —
	// both shapes occur in Open Library's data, so it is decoded lazily by
	// textValue rather than typed here.
	Description json.RawMessage `json:"description"`
}

// edition looks isbn up through the Read API, whose answer is scoped to the
// one edition the ISBN names.
func (c *Client) edition(ctx context.Context, isbn string) (enrich.Metadata, error) {
	body, err := c.get(ctx, c.baseURL+"/api/volumes/brief/isbn/"+url.PathEscape(isbn)+".json")
	if err != nil || body == nil {
		return enrich.Metadata{}, err
	}

	// An ISBN the Read API knows nothing about is answered with a bare
	// `[]` — a JSON *array*, where a match is an object — so this is
	// checked before unmarshalling rather than after. Decoding it into
	// readAPIResponse fails with a type error, and reporting a no-match as
	// a parse failure would make the ordinary case for an obscure book look
	// like a broken provider.
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] == '[' {
		return enrich.Metadata{}, nil
	}

	var parsed readAPIResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return enrich.Metadata{}, fmt.Errorf("openlibrary: parse response: %w", err)
	}

	// The records map is keyed by the edition key Open Library resolved the
	// ISBN to ("/books/OL33891995M"), which this package has no use for and
	// cannot predict; an ISBN lookup yields at most one.
	for _, record := range parsed.Records {
		return c.editionMetadata(record), nil
	}
	return enrich.Metadata{}, nil
}

func (c *Client) editionMetadata(record readAPIRecord) enrich.Metadata {
	details := record.Details.Details

	m := enrich.Metadata{
		Title:         strings.TrimSpace(firstNonEmpty(details.Title, record.Data.Title)),
		PublishedDate: strings.TrimSpace(firstNonEmpty(details.PublishDate, record.Data.PublishDate)),
		Description:   textValue(details.Description),
		ISBN:          bestISBN(append(append([]string{}, details.ISBN13...), details.ISBN10...)),
	}

	for _, a := range record.Data.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			m.Authors = append(m.Authors, name)
		}
	}
	if len(m.Authors) == 0 {
		// Some edition records do carry named authors even though the
		// canonical place is the work; both shapes were observed live.
		for _, a := range details.Authors {
			if name := strings.TrimSpace(a.Name); name != "" {
				m.Authors = append(m.Authors, name)
			}
		}
	}

	if len(details.Publishers) > 0 {
		m.Publisher = strings.TrimSpace(details.Publishers[0])
	} else if len(record.Data.Publishers) > 0 {
		m.Publisher = strings.TrimSpace(record.Data.Publishers[0].Name)
	}

	if len(details.Languages) > 0 {
		// "/languages/eng" -> "eng", which is the MARC code marcToISO639
		// already expects.
		m.Language = isoLanguage(path.Base(details.Languages[0].Key))
	}

	if len(details.Covers) > 0 && details.Covers[0] > 0 {
		m.CoverURL = c.coverURL(details.Covers[0])
	}

	return m
}

// get issues one GET and returns its body, implementing the four network
// cases docs/notes/enrichment.md draws a hard line around: a 404 is "no match" — a nil
// body and a nil error, the ordinary answer for a book neither endpoint
// knows; a 429, any 5xx, or a transport/timeout failure are errors the
// retry decorator (internal/enrich) and the resolver's skip-and-continue
// both expect to see as such.
func (c *Client) get(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("openlibrary: build request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A refused redirect is this client's own policy answering, not
		// the network: the policy is a pure function of URLs that do not
		// change between attempts, so a retry reaches the same refusal.
		// Checked before the retryable wrap below, which would otherwise
		// catch it along with every real transport failure.
		if errors.Is(err, enrich.ErrRedirectRefused) {
			return nil, fmt.Errorf("openlibrary: request failed: %w", err)
		}
		// A transport or timeout failure is the retryable case: nothing
		// about the request itself was rejected.
		return nil, fmt.Errorf("openlibrary: request failed: %w: %w", enrich.ErrRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if isRetryableStatus(resp.StatusCode) {
			return nil, fmt.Errorf("openlibrary: unexpected status %d: %s: %w", resp.StatusCode, body, enrich.ErrRetryable)
		}
		return nil, fmt.Errorf("openlibrary: unexpected status %d: %s", resp.StatusCode, body)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("openlibrary: read response: %w: %w", enrich.ErrRetryable, err)
	}
	return body, nil
}

// textValue reads Open Library's text-or-object field shape: a
// {"type": "/type/text", "value": "…"} object, or a bare string in the same
// position. Both occur, so neither is assumed — a decode that expected only
// the object would silently drop every string-shaped description.
func textValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return strings.TrimSpace(obj.Value)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// marcToISO639 maps the MARC language codes search.json answers with
// ("eng", "rus") onto the two-letter ISO 639-1 form. Without it the
// language column holds "eng" for one book and "en" for the next, rendered
// raw side by side on the detail page. Only the languages this library
// plausibly contains are listed; anything else passes through unchanged,
// which is still better than a wrong guess.
//
// What this agrees with is internal/googlebooks, whose baseLanguage cuts
// the Volumes API's BCP-47 tags (pt-BR, zh-CN) back to the same form. It
// does not agree with the file parsers: internal/epub and internal/fb2
// normalise nothing, and EPUB's dc:language is BCP-47 by specification, so
// the column can still receive a subtagged value from a file.
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
		n := storage.NormalizeISBN(raw)
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
