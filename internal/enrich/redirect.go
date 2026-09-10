package enrich

import (
	"net/url"
	"strings"
)

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
// It lives here rather than in either client because both apply the same
// rule to their own redirects, and two copies of a host comparison are two
// chances to normalise differently — which, in a check that decides whether
// a credential travels to another host, is not a difference to discover
// later.
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
