package enrich

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MaxLookupRedirects bounds how far a provider lookup may bounce. Setting
// CheckRedirect at all replaces net/http's own default limit, so a policy
// that only checked the scheme would follow a chain forever.
const MaxLookupRedirects = 5

// ErrRedirectRefused marks every refusal CheckLookupRedirect issues, so a
// lookup can classify one as non-retryable. The policy is a pure function
// of URLs that do not change between attempts, so a second attempt reaches
// the identical refusal — the same reason a 400 or a 403 is not retried.
// Without the sentinel a refusal burns DefaultRetryAttempts, three lookups
// spent to be told the same thing three times.
//
// http.Client.Do returns a CheckRedirect error inside a *url.Error, which
// unwraps, so errors.Is reaches this through the wrapping the client adds.
var ErrRedirectRefused = errors.New("redirect refused")

// CheckLookupRedirect is the CheckRedirect both provider clients carry. It
// bounds the hops, checks every hop's scheme rather than only the first
// URL's — each hop after the first is chosen by whatever host answered,
// not by the client — and refuses a hop that leaves the host the lookup
// started against or drops off TLS.
//
// Following a redirect at all is not optional: an Open Library ISBN is
// frequently an alias for the canonical edition key, so the Read API
// answers a hop rather than a record.
//
// The host check earns its place twice over, once per client. For
// internal/googlebooks it guards a credential: the API key travels in the
// query string, and net/http sets Referer on every hop from the previous
// request's full URL — suppressing it only on https→http — so an ordinary
// https→https redirect would hand "?key=…" to whichever host answered, in
// a header. Moving the key out of the query string would not substitute,
// since Go forwards non-sensitive headers across hosts. For
// internal/openlibrary there is no credential, and the reason is the other
// one the check closes for both: a cross-host hop makes the client adopt
// the answering host's whole response, which plausibleMatch gates by title
// and author on a search and which nothing gates on an ISBN lookup or on
// Google's detail request.
//
// Refusing costs nothing measurable. Neither API was observed to redirect
// cross-host: Google's does not redirect at all, and Open Library's hops
// stay on openlibrary.org.
//
// One function rather than one per client, because the two are the same
// rule and a second copy of a check that decides whether a credential
// leaves the host is not a thing to let drift.
func CheckLookupRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= MaxLookupRedirects {
		return fmt.Errorf("stopped after %d redirects: %w", MaxLookupRedirects, ErrRedirectRefused)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect scheme %q is not http or https: %w", req.URL.Scheme, ErrRedirectRefused)
	}
	// Refuses an empty via rather than passing it. net/http always
	// supplies the requests already made, so this cannot happen — but it
	// is the clause guarding a credential, and one that fails open is one
	// a future reuse can walk through.
	if len(via) == 0 {
		return fmt.Errorf("redirect with no originating request to compare against: %w", ErrRedirectRefused)
	}
	// Compared against via[0], the request the lookup made, rather than
	// the previous hop. The two are equivalent — every hop is checked, so
	// the host can never change — but via[0] states the invariant the
	// policy actually has: a lookup never leaves the host it started
	// against.
	if !SameHost(req.URL, via[0].URL) {
		return fmt.Errorf("redirect to %q leaves the host the lookup started against: %w", req.URL.Host, ErrRedirectRefused)
	}
	// A same-host downgrade off TLS is refused too. It reads as redundant
	// and is not: SameHost's default-port normalisation already refuses an
	// ordinary https→http hop as a port change, 443 against 80, but a
	// Location that writes the port out on both sides compares equal there
	// and reaches this.
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect downgrades from https to %q: %w", req.URL.Scheme, ErrRedirectRefused)
	}
	return nil
}

// SameHost compares two URLs' authorities the way a host is actually the
// same rather than the way two strings are: case-insensitively, with the
// scheme's default port and an explicit one treated alike, and a fully
// qualified trailing dot ignored.
//
// A byte compare of URL.Host would refuse every one of those, which is safe
// in the sense that nothing is admitted wrongly and unsafe in the sense
// that matters: a redirect refusal is a lookup failure, so a Location that
// merely spells the same host differently would leave enrichment quietly
// answering nothing for that book.
//
// One caveat, harmless as things stand: an http→https upgrade on the same
// name reads as a different host, 80 against 443. Both clients start from
// an https base URL, so no lookup can meet it.
func SameHost(a, b *url.URL) bool {
	return strings.EqualFold(canonicalHostname(a), canonicalHostname(b)) &&
		effectivePort(a) == effectivePort(b)
}

// canonicalHostname drops the trailing dot of a fully qualified name, which
// names the same host as the form without it.
func canonicalHostname(u *url.URL) string {
	return strings.TrimSuffix(u.Hostname(), ".")
}

// effectivePort fills in the scheme's default, so an omitted port and an
// explicitly written default one compare equal.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
