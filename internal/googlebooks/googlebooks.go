// Package googlebooks is an enrich.Provider backed by the Google Books
// Volumes API (https://www.googleapis.com/books/v1/volumes), per docs/notes/enrichment.md's
// provider choices for the metadata chain. An API key is nominally
// optional, but in practice required: unauthenticated requests are billed
// to one Google-wide project whose daily quota was found already exhausted
// on every attempt, days apart, answering 429 rather than results.
package googlebooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"library/internal/enrich"
	"library/internal/storage"
)

// Timeout is http.Client.Timeout, so it bounds one *request* rather than
// one lookup: a matched lookup issues two (see enrichVolume), and Resolve
// may follow a no-match ByISBN with a Search, each of them retried by
// enrich.WithRetry. Nothing here caps the total, and internal/enrich's
// worker sets no per-job deadline either, so one book against this
// provider can occupy the queue for several multiples of this.
//
// Left as a per-request bound rather than tightened, because the worker is
// single and the queue has nothing waiting behind it that a whole-lookup
// deadline would rescue. Sized short for the same reason
// internal/openlibrary's is: enrichment is a background nicety, not
// something a person is waiting on.
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
// has none at all. apiKey is optional in the sense that an empty string
// still builds a working client, not in the sense that it works: see the
// package comment on the shared anonymous project's exhausted quota.
func New(apiKey string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: Timeout, CheckRedirect: enrich.CheckLookupRedirect},
	}
}

// Name identifies this provider in logs and in the provenance
// field_sources records — a stable identifier, not a display string.
func (c *Client) Name() string { return providerName }

// ByISBN looks a book up by isbn, through the one normalisation every
// reader of an ISBN shares (storage.NormalizeISBN), so the lookup key
// reaches Google Books — and comes back out through toMetadata — in the
// same shape the column already holds
func (c *Client) ByISBN(ctx context.Context, isbn string) (enrich.Metadata, error) {
	normalized := storage.NormalizeISBN(isbn)
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
	// ID names the volume on the single-volume endpoint, which is the only
	// place the cover sizes above thumbnail exist — see enrichVolume.
	ID         string     `json:"id"`
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

// imageLinks is the Volumes API's cover URLs. thumbnail is the only one
// present on every volume that has a cover at all, but it is roughly 128px
// wide — a long edge under internal/cover's 400px target, which never
// upscales — so a larger size is preferred when the volume carries one.
//
// extraLarge is deliberately absent, and it is the one a reader will want
// to add back. Measured against three digitised volumes:
//
//	              long edge   bytes
//	thumbnail       ~195       ~6-18 KB
//	small           ~460      ~12-41 KB
//	medium          ~880      ~30-124 KB
//	large          ~1225      ~48-205 KB
//	extraLarge     ~2670     ~350-800 KB
//
// enrich.MaxCoverBytes is 512 KiB and a cover past it is refused outright,
// so extraLarge is the only size that can turn a good cover into *no*
// cover — and it buys nothing, since cover.Store resizes to 400px on the
// long edge and every size from medium up already clears that. Adding it
// back trades a certain cover for a bigger one nobody sees.
type imageLinks struct {
	Large     string `json:"large"`
	Medium    string `json:"medium"`
	Small     string `json:"small"`
	Thumbnail string `json:"thumbnail"`
}

// best picks the cover URL worth fetching — not the largest the volume
// offers — upgrading it to https: Google answers these as plain http, and cover
// bytes that end up served from /covers/ should not arrive over cleartext
// from a host that serves TLS on the same name.
//
// Every candidate above Thumbnail comes from the single-volume endpoint;
// the list endpoint names only smallThumbnail and thumbnail, whatever the
// volume. That is what enrichVolume's second request is for.
//
// The shortcut to avoid: a thumbnail URL carries a zoom=1 parameter, and
// rewriting it to zoom=2 or higher does return a larger JPEG. But for a
// size a volume does not have, Google answers 200 image/jpeg with an
// "image not available" placeholder — verified by fetching one. Nothing in
// enrich.FetchCover or cover.Store can tell that from a cover, so the
// covers directory would fill with grey placeholders that read as
// successfully enriched. Only a URL Google itself named is safe to fetch.
func (l imageLinks) best() string {
	// medium first, not small, even though small's ~460px average also
	// clears cover.Store's 400px long edge: that is an average over three
	// volumes and an individual small can fall under it, where medium
	// (~880px) never did. medium is the safe floor, so above it the order
	// is smallest-first — large only throws away more pixels for more
	// bytes — and below it, largest-first among the fallbacks.
	//
	// The sample is three volumes. best() returns one URL and
	// enrich.FetchCover refuses a body over MaxCoverBytes outright rather
	// than stepping down, so a medium past 512 KiB loses the cover with
	// small and thumbnail unused in the same response. Not observed; the
	// conclusion is only as good as the sample.
	for _, candidate := range []string{l.Medium, l.Large, l.Small, l.Thumbnail} {
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

// search issues a GET against /volumes with q and turns the response into
// Metadata — plus, for a matched volume, the second request enrichVolume
// makes. It implements the four network cases docs/notes/enrichment.md draws a hard line
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
		// A refused redirect is this client's own policy answering, not
		// the network: the policy is a pure function of URLs that do not
		// change between attempts, so a retry reaches the same refusal.
		// Checked before the retryable wrap below, which would otherwise
		// catch it along with every real transport failure.
		if errors.Is(err, enrich.ErrRedirectRefused) {
			return enrich.Metadata{}, fmt.Errorf("googlebooks: request failed: %w", c.redactKey(err))
		}
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
		// Measured against the live API rather than read off the docs: a
		// rejected key is
		// 400 (API_KEY_INVALID), an exhausted quota is 429 on both the
		// per-day and the per-minute limit, and 403 is the service not
		// being enabled for the project (SERVICE_DISABLED). So only the
		// 429 and the 5xx are worth asking twice; 400 and 403 are
		// configuration, and answer identically however often they are
		// asked.
		//
		// The 429 is retried even when it is the per-day quota, which
		// will not clear for hours. Google names which limit was hit in
		// the body — quota_limit is defaultPerDayPerProject for the
		// daily one — and sends no Retry-After at all, so telling them
		// apart is possible but is a retry-policy decision rather than
		// this classification's.
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

	matched := parsed.Items[0]
	m := c.toMetadata(matched)
	// The list answer stands whatever the detail request did; only the
	// mark changes, and only WithCache reads it. Every failure counts —
	// transport, non-200, a malformed body, a body naming another volume —
	// because each one leaves the same fields unfilled and each one might
	// not happen next time.
	if err := c.enrichVolume(ctx, matched.ID, &m); err != nil {
		m.Partial = true
	}
	return m, nil
}

// enrichVolume fills in what the list endpoint cannot answer, from the
// single-volume endpoint for the same volume.
//
// Two fields differ between the two, and both were measured rather than
// read: the list endpoint names only smallThumbnail and thumbnail — so
// every cover from it is ~128px wide, whose long edge is under
// internal/cover's 400px target, which never upscales — while the
// single-volume endpoint carries small through extraLarge for a volume
// Google has digitised. And the list endpoint's description arrives with
// its markup already flattened, paragraph breaks included, where the
// documented HTML form is the single-volume endpoint's — often different,
// fuller text rather than the same text differently punctuated.
//
// Those paragraph breaks reach the page: .detail__description renders with
// white-space: pre-line, so a stored break is a break in the read view.
//
// The request is made for any matched volume that named an id, not only
// one that also has a cover. Skipping a coverless volume would be free on
// the cover half and would silently drop the description half, which is
// the payoff for a book whose blurb is the thing worth having.
//
// It fails softly, not silently. A larger cover and a paragraph break are
// niceties; the six text fields already in hand are the answer, and losing
// them because a second request timed out would be the wrong trade. So
// every failure leaves m exactly as the list response built it and is
// logged at Debug — this runs once per matched volume, and a Warn per
// enriched book teaches people to ignore Warns.
//
// The difference from silent is the returned error, which the caller turns
// into Metadata.Partial. Without it a transient failure of this request
// would be stored by enrich.WithCache as a complete answer and served for
// the life of the process, so one timeout would cost that book its cover
// and fuller description until a restart.
func (c *Client) enrichVolume(ctx context.Context, id string, m *enrich.Metadata) error {
	if id == "" {
		// Not a failure: a volume with no id has no detail endpoint to
		// ask, so there is nothing a later attempt would do differently.
		return nil
	}

	detail, err := c.volumeByID(ctx, id)
	if err != nil {
		slog.Debug("googlebooks: volume detail lookup failed", "volume_id", id, "error", err)
		return err
	}
	// A body describing some other volume is not an answer about this
	// book, and nothing downstream would catch it: internal/enrich's
	// plausibleMatch gates the Title and Authors of the *list* response,
	// while the two fields taken here come from this one.
	//
	// An absent id is refused along with a wrong one. Treating "" as a
	// pass would let the party being checked opt out of the check by
	// omitting the field. The evidence that the real endpoint always
	// answers with one is thinner than it sounds — testdata holds a single
	// capture of this endpoint, volumes_detail.json, and it carries an id,
	// as does every list capture's item — but a response shape that has
	// never been observed is the right thing to refuse when admitting it
	// costs the check.
	if detail.ID != id {
		slog.Debug("googlebooks: volume detail names a different volume", "requested", id, "answered", detail.ID)
		return fmt.Errorf("googlebooks: volume detail names %q, not %q", detail.ID, id)
	}

	// Each field is replaced only by a present answer, never by an absent
	// one — this endpoint returns "" for a description plenty of volumes
	// have on the list one. Present, not better: neither check compares
	// sizes or quality, so if the list endpoint ever named a size above
	// thumbnail, a detail response naming only thumbnail would replace it
	// with the smaller one. Unreachable today, and stated rather than
	// guarded because guarding it would be code with no way to test it
	// against the real API.
	if cover := detail.VolumeInfo.ImageLinks.best(); cover != "" {
		m.CoverURL = cover
	}
	if description := plainText(detail.VolumeInfo.Description); description != "" {
		m.Description = description
	}
	return nil
}

// volumeByID reads one volume from /volumes/{id}. The endpoint answers a
// bare volume object — the same shape the list response nests — so it
// decodes into the same type.
func (c *Client) volumeByID(ctx context.Context, id string) (volume, error) {
	reqURL := c.baseURL + "/volumes/" + url.PathEscape(id)
	if c.apiKey != "" {
		reqURL += "?" + url.Values{"key": {c.apiKey}}.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return volume{}, fmt.Errorf("build request: %w", c.redactKey(err))
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return volume{}, fmt.Errorf("request failed: %w", c.redactKey(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return volume{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, c.redactKeyBytes(body))
	}

	var parsed volume
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&parsed); err != nil {
		return volume{}, fmt.Errorf("parse response: %w", err)
	}
	return parsed, nil
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
	return redactedError{err: err, text: c.redactString(err.Error())}
}

// redactString removes the key in both the forms it can appear in. A
// transport error embeds the request URL, where url.Values.Encode has
// percent-escaped the key — so a literal substring match alone misses it
// for any key containing a character that needs escaping. Today's Google
// keys are "AIza" plus URL-safe characters, so the escaped form is
// identical to the raw one and the literal match happens to work; that is
// a property of the key format, not of this function, and it is not one to
// rest a credential on.
func (c *Client) redactString(s string) string {
	s = strings.ReplaceAll(s, c.apiKey, "REDACTED")
	if escaped := url.QueryEscape(c.apiKey); escaped != c.apiKey {
		s = strings.ReplaceAll(s, escaped, "REDACTED")
	}
	return s
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
	return c.redactString(string(body))
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
		Language:      baseLanguage(info.Language),
		ISBN:          bestISBN(info.IndustryIdentifiers),
		Description:   plainText(info.Description),
	}
	m.CoverURL = info.ImageLinks.best()
	return m
}

// baseLanguage reduces a language tag to its primary subtag, so
// books.language holds one vocabulary. volumeInfo.language is BCP-47, not the ISO 639-1 code
// internal/openlibrary's marcToISO639 produces: a scan of 188 live volumes
// answered pt-BR 50 times and zh-CN 32, alongside plain en, ru, ja and sv.
// Left alone, the column reads "pt" for a book Open Library answered and
// "pt-BR" for the next one this provider did.
//
// A subtag cut rather than a mapping table, because Google's primary
// subtag is already ISO 639-1 in every value observed — there is nothing
// to translate, only a region to drop, and a table would be a second thing
// to maintain that agreed with the identity function.
//
// It drops more than the region: zh-Hant and zh-Hans both become zh,
// losing Traditional against Simplified, which is a real loss where
// pt-BR against pt-PT is a mild one. Accepted for the same reason — the
// column is one short code that three other writers fill without any
// subtag at all, so keeping one here would make its meaning depend on
// which source filled it.
//
// This does not make the column consistent on its own. internal/epub and
// internal/fb2 pass their file's own value straight through, and EPUB's
// dc:language is BCP-47 by specification, so a subtag can still arrive
// from a file. Fixing that means one derivation shared by every writer,
// which is a different change; see
// docs/plans/completed/2026090608-googlebooks-live-fidelity.md.
func baseLanguage(tag string) string {
	base, _, _ := strings.Cut(strings.TrimSpace(tag), "-")
	return strings.ToLower(base)
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
			return storage.NormalizeISBN(id.Identifier)
		case "ISBN_10":
			if isbn10 == "" {
				isbn10 = storage.NormalizeISBN(id.Identifier)
			}
		}
	}
	return isbn10
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
		// Trimmed even on the fast path, so every return from this
		// function is trimmed. A caller testing the result against "" to
		// decide whether a provider said anything — enrichVolume does —
		// would otherwise read a description of "   " as an answer and
		// overwrite a real one with whitespace that sanitizeValue then
		// trims to nothing, losing the field outright.
		return trimBlank(raw)
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
	return trimBlank(collapseBlankLines(text))
}

// zeroWidth are the characters that carry no ink and that unicode.IsSpace
// does not call space, so strings.TrimSpace leaves them behind. A
// description of nothing but one of them — "&#8203;" unescapes to exactly
// that — would otherwise read as an answer to every "is this empty" test
// between here and the column, and overwrite a real description with a
// value that renders as nothing.
const zeroWidth = "\u200b\u200c\u200d\ufeff"

// trimBlank is strings.TrimSpace widened to the zero-width characters, so
// "blank" here means "renders as nothing" rather than "is Unicode
// whitespace".
//
// Composed with unicode.IsSpace rather than written as a cutset, which is
// the distinction that matters: strings.Trim with a hand-listed cutset is
// not TrimSpace, and spelling out the ASCII spaces plus a couple of
// favourites drops the other seventeen runes IsSpace accepts — U+3000, the
// ordinary CJK ideographic space, among them. Widening a trim by narrowing
// it is an easy trade to make by accident, and this library holds Chinese
// and Japanese books.
func trimBlank(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(zeroWidth, r)
	})
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
