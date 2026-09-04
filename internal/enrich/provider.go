// Package enrich is the metadata provider-enrichment queue: a single
// worker draining enrichment_jobs, a tiny Provider interface real
// providers implement, and the resolver that decides which fields a book
// still needs and merges what providers answer. Modeled on
// internal/sender's send-to-Kindle worker — same queue shape, same claim/
// process/terminal-write structure — but see worker.go for where the two
// deliberately diverge.
//
// internal/openlibrary and internal/googlebooks are the two real
// implementations; a nil or empty provider list stays valid, since that is
// what METADATA_PROVIDERS= configures. The package is buildable and fully
// testable without any of them, per DESIGN.md's "resolver logic is kept
// separate from the providers so ordering and merging are testable without
// any real provider" — see resolver_test.go's fakes.
package enrich

import (
	"context"
	"errors"
)

// ErrRetryable marks the subset of provider failures worth another try —
// DESIGN.md's 429/5xx/transport case. A provider wraps it around those and
// only those; WithRetry retries on errors.Is(err, ErrRetryable) and gives
// up immediately on anything else, so a 400, a bad API key, or a malformed
// body costs one request rather than three. A "no match" never reaches
// here at all: that is a zero Metadata with a nil error, an answer rather
// than a failure.
var ErrRetryable = errors.New("retryable provider failure")

// Provider is one metadata source. Implementations are HTTP clients;
// nothing here knows that. Declared on the consumer side (this package),
// the same way internal/sender declares Transport rather than
// internal/resend declaring a Sender interface of its own.
type Provider interface {
	// Name is used for logging and for the provenance written to
	// field_sources, so it is a stable identifier, not a display string.
	Name() string
	// ByISBN looks a book up by its normalised ISBN. A book with no ISBN
	// never reaches it.
	ByISBN(ctx context.Context, isbn string) (Metadata, error)
	// Search is the fallback when there is no ISBN.
	Search(ctx context.Context, title string, authors []string) (Metadata, error)
}

// Metadata is what a Provider can answer about a book: the seven text
// fields field_sources tracks, plus a fetched cover's raw image bytes.
// Every field is optional, mirroring internal/epub and internal/fb2's own
// Metadata: a zero Metadata with a nil error means "no answer", which is
// the common case for most books against most providers, not an error
// condition.
type Metadata struct {
	Title         string
	Authors       []string
	Publisher     string
	PublishedDate string
	Language      string
	ISBN          string
	Description   string
	// CoverURL locates a cover image, or is empty when the provider found
	// none. It is a URL rather than the bytes themselves so that nothing
	// downloads an image until the resolver has established the book
	// actually needs one — most books already carry an embedded cover, and
	// fetching inside the provider would spend a round trip and up to
	// MaxCoverBytes per lookup on an answer Resolve then discards. The
	// download and internal/cover.Store call are the Worker's, which is
	// also what keeps image bytes out of WithCache's bounded map.
	//
	// It never reaches cover_path: the column holds the path Store
	// produced, keyed by the book's content hash, never a remote URL.
	CoverURL string
}
