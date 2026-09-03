// Package enrich is the metadata provider-enrichment queue: a single
// worker draining enrichment_jobs, a tiny Provider interface real
// providers implement, and the resolver that decides which fields a book
// still needs and merges what providers answer. Modeled on
// internal/sender's send-to-Kindle worker — same queue shape, same claim/
// process/terminal-write structure — but see worker.go for where the two
// deliberately diverge.
//
// No real Provider exists yet; that's step 05. This package is buildable
// and fully testable without one, per DESIGN.md's "resolver logic is kept
// separate from the providers so ordering and merging are testable without
// any real provider" — see resolver_test.go's fakes.
package enrich

import "context"

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

// Metadata is what a Provider can answer about a book — the same seven
// fields field_sources tracks, minus the cover: field_sources.field is
// CHECK-constrained to text fields only, a cover has no provenance row to
// record, and whether covers are even worth fetching is step 05's decision
// once there's real provider output to judge it against (see the plan's
// Scope). Every field is optional, mirroring internal/epub and
// internal/fb2's own Metadata: a zero Metadata with a nil error means "no
// answer", which is the common case for most books against most
// providers, not an error condition.
type Metadata struct {
	Title         string
	Authors       []string
	Publisher     string
	PublishedDate string
	Language      string
	ISBN          string
	Description   string
}
