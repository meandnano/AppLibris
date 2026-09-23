package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

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

	d, err := s.fetcher.Fetch(ctx, link)
	if err != nil {
		var status *importer.StatusError
		if errors.As(err, &status) || errors.Is(err, importer.ErrTooLarge) {
			return nil, err
		}
		return nil, downloadFailure(ctx, err)
	}
	defer d.Body.Close()

	// The deadline is sized for the download alone, and the body's reads
	// already end with ctx through the request that fetched it. What Stage
	// does once the body has ended, the parse and the duplicate lookup, is
	// not the download, and a deadline passing then must not discard a book
	// that arrived whole
	body := &bodyReader{r: d.Body}
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
