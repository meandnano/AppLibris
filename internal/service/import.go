package service

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"library/internal/importer"
)

// ErrImportDisabled is what every import method answers when there is no
// Stager — which is what cmd/server builds when the library directory
// cannot be written. One disabled state and one convention, the same
// Notify follows. The routes stay registered so a stale tab gets this
// explanation rather than a 404.
var ErrImportDisabled = errors.New("service: import is not available")

var (
	// ErrUnsupportedLink is a link StageURL will not try: not http or
	// https, no host, credentials in it, or too long
	ErrUnsupportedLink = errors.New("service: not a supported link")
	// ErrDownloadBusy is a link refused because another is still
	// downloading
	ErrDownloadBusy = errors.New("service: another link is still downloading")
	// ErrWebPage wraps importer.ErrUnsupportedFormat when the link answered
	// with an HTML page, the usual result of pasting the page a download
	// button sits on rather than the button's own link
	ErrWebPage = errors.New("service: the link opens a web page")
	// ErrDownloadFailed is a link that could not be fetched: DNS, connect,
	// TLS, a redirect refused, the body cut off, a request abandoned. One
	// sentinel for all of them, since a refused private address must read
	// the same as a host that is down. Its text carries no URL
	ErrDownloadFailed = errors.New("service: the download failed")
)

// MaxLinkBytes bounds a pasted link. A real download link, signed query
// and all, fits well inside it
const MaxLinkBytes = 2 << 10

// LinkFetcher downloads a link without reading the body.
// *importer.Fetcher is the one production implementation
type LinkFetcher interface {
	Fetch(ctx context.Context, rawURL string) (importer.Download, error)
}

// ImportPreview is one staged import as the import page needs it.
//
// An alias rather than a copy. Every field of importer.Staged is one the
// page renders, and the one thing a separate type would restate — the
// verdict, as a string — the transport casts straight back to compare. A
// per-page copy of sixteen identical fields is a rename of nothing, which
// is the same call BookDetail makes in carrying service.FileLocation as it
// comes. internal/web depends on internal/importer for the error sentinels
// a refusal is matched against anyway, so a copy buys no independence
// either.
type ImportPreview = importer.Staged

// ImportEnabled reports whether this run can import at all, which is what
// decides whether the nav offers the page.
func (s *Service) ImportEnabled() bool {
	return s.importer != nil
}

// MaxImportBytes is the configured size cap, for the sentence a refusal
// shows. Zero when import is unavailable, where no refusal names it.
func (s *Service) MaxImportBytes() int64 {
	if s.importer == nil {
		return 0
	}
	return s.importer.MaxSize()
}

// StageImport writes an uploaded file to staging and previews it. name is
// the filename the client offered, which seeds the library name and nothing
// else — what the file is comes out of its content.
func (s *Service) StageImport(ctx context.Context, name string, r io.Reader) (*ImportPreview, error) {
	if s.importer == nil {
		return nil, ErrImportDisabled
	}
	staged, err := s.importer.Stage(ctx, name, r)
	if err != nil {
		return nil, err
	}
	return &staged, nil
}

// StageURL downloads a pasted link into staging and previews it, the same
// preview StageImport answers for an upload. The download runs inside ctx,
// so the caller's deadline bounds it and a closed tab cancels it
func (s *Service) StageURL(ctx context.Context, rawURL string) (*ImportPreview, error) {
	if s.importer == nil || s.fetcher == nil {
		return nil, ErrImportDisabled
	}
	link, err := validLink(rawURL)
	if err != nil {
		return nil, err
	}

	// Refused rather than queued: a wait here would hold the request open
	// behind a download of unknown length
	select {
	case s.downloads <- struct{}{}:
		defer func() { <-s.downloads }()
	default:
		return nil, ErrDownloadBusy
	}

	staged, err := s.stageLink(ctx, link)
	if !errors.Is(err, ErrWebPage) {
		return staged, err
	}
	// A book's page on a site that serves the file at <page>/epub is the
	// link people actually have at hand. Only a web page is retried, so a
	// failure of any other kind is never hidden behind a second request
	if fallback, ok := epubFallback(link); ok {
		retried, retryErr := s.stageLink(ctx, fallback)
		if retryErr == nil {
			return retried, nil
		}
		// The log names the pasted link only, so the error says which
		// request it came from
		if !fallbackMiss(ctx, retryErr) {
			return nil, fmt.Errorf("retry at /epub: %w", retryErr)
		}
	}
	// The page the person pasted is what they need to hear about, not the
	// guess made on their behalf
	return nil, err
}

// stageLink downloads one link into staging. The caller holds the download
// slot, so a retry reuses it instead of queueing behind other people
func (s *Service) stageLink(ctx context.Context, link string) (*ImportPreview, error) {
	// A context of its own so a page's head can be cut off without ending
	// the caller's window
	fetchCtx, cancelFetch := context.WithCancel(ctx)
	defer cancelFetch()

	d, err := s.fetcher.Fetch(fetchCtx, link)
	if err != nil {
		var status *importer.StatusError
		if errors.As(err, &status) || errors.Is(err, importer.ErrTooLarge) {
			return nil, err
		}
		return nil, downloadFailure(ctx, err)
	}
	defer d.Body.Close()

	var src io.Reader = d.Body
	if d.HTML {
		head, err := readPageHead(d.Body, cancelFetch)
		switch {
		// The caller's window ending is the answer even for a page, the
		// same order the Stage errors below keep, and a retry started on an
		// ended window could only fail
		case ctx.Err() != nil:
			return nil, downloadFailure(ctx, cmp.Or(err, errPageHeadCut))
		case err != nil:
			return nil, downloadFailure(ctx, err)
		case head == nil || !importer.MayBeBook(head):
			return nil, fmt.Errorf("%w: %w", ErrWebPage, importer.ErrUnsupportedFormat)
		}
		src = io.MultiReader(bytes.NewReader(head), d.Body)
	}

	// The deadline is sized for the download alone, and the body's reads
	// already end with ctx through the request that fetched it. What Stage
	// does once the body has ended, the parse and the duplicate lookup, is
	// not the download, and a deadline passing then must not discard a book
	// that arrived whole
	body := &bodyReader{r: src}
	staged, err := s.importer.Stage(context.WithoutCancel(ctx), d.Name, body)
	switch {
	case err == nil:
		return &staged, nil
	// A body cut short by the deadline can read as a clean end, and the
	// truncated bytes then fail as not a book, which would blame the file
	// for what was the wait
	case ctx.Err() != nil, body.err != nil:
		return nil, downloadFailure(ctx, err)
	case errors.Is(err, importer.ErrUnsupportedFormat) && d.HTML:
		return nil, fmt.Errorf("%w: %w", ErrWebPage, err)
	default:
		return nil, err
	}
}

var errPageHeadCut = errors.New("the page's head was cut off")

// readPageHead reads the head of a body labelled a web page and ends its
// fetch once importer.PageSniffTimeout passes. A head cut off that way is
// nil with no error: the label stands, since no byte said otherwise. A head
// whose read succeeded is kept even when the timer fired in the same
// instant, because those bytes did arrive in time and decide what the body
// is; a book's remaining body then fails as cut off, which is what happened
func readPageHead(body io.Reader, cancelFetch context.CancelFunc) ([]byte, error) {
	timer := time.AfterFunc(importer.PageSniffTimeout, cancelFetch)
	head, err := importer.ReadHead(body)
	if fired := !timer.Stop(); fired && err != nil {
		return nil, nil
	}
	return head, err
}

// epubFallback appends /epub to the link's path, leaving the query alone
// since some sites select the book with it. The path is extended in its
// escaped form, because decoding it first turns an encoded slash into a
// separator and names a different address. A link already ending in /epub
// has had that address tried, so it has no fallback
func epubFallback(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil {
		return "", false
	}
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	if strings.EqualFold(escaped[strings.LastIndex(escaped, "/")+1:], "epub") {
		return "", false
	}
	escaped += "/epub"
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", false
	}
	u.Path = decoded
	u.RawPath = escaped
	u.Fragment = ""
	return u.String(), true
}

// fallbackMiss reports whether a failed retry only says the guessed address
// holds no book. Any 4xx but a timeout or a rate limit counts, since S3,
// CloudFront and many firewalls answer a path that does not exist with 403
// or 400. Anything else, a 408, a 429, a 5xx, a body cut off, the caller's
// deadline, a full staging area, a file over the cap, can change on a second
// try or with a smaller file, which the page's message would hide
func fallbackMiss(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var status *importer.StatusError
	if errors.As(err, &status) {
		return status.Code >= 400 && status.Code < 500 &&
			status.Code != http.StatusRequestTimeout && status.Code != http.StatusTooManyRequests
	}
	return errors.Is(err, importer.ErrUnsupportedFormat)
}

// quotedText is a Go-quoted string inside an error's text, which is how
// net/http and net/url name a URL: a redirect's unparseable Location, a
// parse failure, the request's own link
var quotedText = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// downloadFailure wraps a failed download in ErrDownloadFailed, adding the
// context's cause only when ctx itself ended. The fetch error's own chain
// is cut, because a transport's dial, TLS and header timeouts all match
// context.DeadlineExceeded and would otherwise read as the caller's
// deadline. Its text loses the link and every quoted string, because the
// link, or a Location a remote host chose, can carry a signed token into a
// log
func downloadFailure(ctx context.Context, err error) error {
	msg := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		msg = urlErr.Op + ": " + urlErr.Err.Error()
	}
	msg = quotedText.ReplaceAllString(msg, `"…"`)
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w: %s", ErrDownloadFailed, context.Cause(ctx), msg)
	}
	return fmt.Errorf("%w: %s", ErrDownloadFailed, msg)
}

// bodyReader remembers a failed read of the download, so a Stage error it
// caused is told apart from one staging caused on its own
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}

func validLink(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > MaxLinkBytes {
		return "", ErrUnsupportedLink
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrUnsupportedLink
	}
	// url.Parse lowercases the scheme, so HTTPS:// passes as it should.
	// Credentials are refused because a link that needs a session is not a
	// direct link to a file, and a password has no business in a log line
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", ErrUnsupportedLink
	}
	return raw, nil
}

// StagedImport returns one staged import, or nil, nil when it has expired
// or was never there — the same absent-isn't-an-error contract GetBook
// uses.
//
// What a transport makes of that is its own decision, and the web one
// deliberately does not 404: a person looking at the URL of a stage that
// has just expired is looking at a page that was right a moment ago, and
// the file input they need is on it. See importPreviewHandler.
func (s *Service) StagedImport(ctx context.Context, id string) (*ImportPreview, error) {
	if s.importer == nil {
		return nil, ErrImportDisabled
	}
	staged, ok := s.importer.Get(id)
	if !ok {
		return nil, nil
	}
	return &staged, nil
}

// StagedCover returns the cover a staged file had embedded, with its media
// type, or nil when it had none — what the preview's own image is served
// from, since a staged book has no entry in COVERS_DIR and should not
// acquire one before anybody has said to keep it.
//
// The type travels with the bytes because the transport must not decide it
// by sniffing: see importer.Stager.Cover.
func (s *Service) StagedCover(id string) (data []byte, contentType string) {
	if s.importer == nil {
		return nil, ""
	}
	cover, contentType, ok := s.importer.Cover(id)
	if !ok {
		return nil, ""
	}
	return cover, contentType
}

// ConfirmImport copies a staged file into the library and indexes it.
//
// indexed false with a nil error is the one outcome that is neither success
// nor failure: the file is in the library and the index write failed, so
// the bytes are safe and the next sweep picks them up. There is no book to
// redirect to, which is exactly what the caller needs to know.
func (s *Service) ConfirmImport(ctx context.Context, id string) (bookID int64, indexed bool, err error) {
	if s.importer == nil {
		return 0, false, ErrImportDisabled
	}
	bookID, err = s.importer.Confirm(ctx, id)
	switch {
	case errors.Is(err, importer.ErrNotIndexed):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return bookID, true, nil
}

// DiscardImport drops a staged import and its file. An id that is already
// gone is not an error: what was asked for has happened.
func (s *Service) DiscardImport(ctx context.Context, id string) error {
	if s.importer == nil {
		return ErrImportDisabled
	}
	s.importer.Discard(id)
	return nil
}
