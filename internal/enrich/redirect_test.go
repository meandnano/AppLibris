package enrich

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func mustParse(t *testing.T, rawurl string) *url.URL {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatalf("parse %s: %v", rawurl, err)
	}
	return u
}

// Setting CheckRedirect at all replaces net/http's own hop limit, so the
// policy has to supply both halves itself. Both are checked here because
// each is invisible when the other works.
//
// Every refusal also has to carry ErrRedirectRefused: the sentinel is what
// keeps a refusal out of the retryable class, and a return that dropped
// the %w would silently spend three lookups reaching the same refusal.
// Asserting only err != nil would not see that.
func TestCheckLookupRedirect(t *testing.T) {
	hop := func(rawurl string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, rawurl, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		return req
	}
	from := func(rawurl string) []*http.Request { return []*http.Request{hop(rawurl)} }

	const origin = "https://www.googleapis.com/books/v1/volumes?key=secret"

	refuses := func(t *testing.T, err error, why string) {
		t.Helper()
		if err == nil {
			t.Fatalf("allowed, but %s", why)
		}
		if !errors.Is(err, ErrRedirectRefused) {
			t.Errorf("error does not wrap ErrRedirectRefused, so a lookup would retry it: %v", err)
		}
	}

	allowed := []struct{ name, to string }{
		{"same host, same scheme", "https://www.googleapis.com/books/v1/volumes/abc"},
		{"an explicit default port is the same host", "https://www.googleapis.com:443/books/v1/volumes/abc"},
		{"case is not part of a host's identity", "https://WWW.GOOGLEAPIS.COM/books/v1/volumes/abc"},
		{"a fully qualified trailing dot is the same host", "https://www.googleapis.com./books/v1/volumes/abc"},
	}
	for _, c := range allowed {
		t.Run("allowed/"+c.name, func(t *testing.T) {
			// These are refused by a byte compare of URL.Host, and a
			// refusal is a lookup failure — so getting them wrong leaves
			// enrichment quietly answering nothing for that book.
			if err := CheckLookupRedirect(hop(c.to), from(origin)); err != nil {
				t.Errorf("refused: %v", err)
			}
		})
	}

	refused := []struct{ name, to, why string }{
		{"another host", "https://elsewhere.example/volumes", "net/http would send it the key in Referer"},
		{"another host on the same suffix", "https://evil.googleapis.com.attacker.example/volumes", "a suffix is not a host"},
		{"a non-default port", "https://www.googleapis.com:8443/books/v1/volumes/abc", "a different port is a different service"},
		{"a downgrade off TLS", "http://www.googleapis.com/books/v1/volumes/abc", "a credential-guarding policy should not hand the request to cleartext"},
		{"a foreign scheme", "file:///etc/passwd", "not http or https"},
	}
	for _, c := range refused {
		t.Run("refused/"+c.name, func(t *testing.T) {
			refuses(t, CheckLookupRedirect(hop(c.to), from(origin)), c.why)
		})
	}

	t.Run("refused/a downgrade keeping an explicit port", func(t *testing.T) {
		// The default-port normalisation refuses an ordinary https->http
		// hop as a port change (443 against 80), so the scheme clause
		// looks redundant. It is not: with the port written out on both
		// sides, the hosts compare equal and only the scheme differs.
		refuses(t, CheckLookupRedirect(
			hop("http://www.googleapis.com:8443/x"),
			from("https://www.googleapis.com:8443/volumes?key=secret"),
		), "a cleartext hop on the same explicit port is still a downgrade")
	})
	t.Run("allowed/an http origin may stay on http", func(t *testing.T) {
		if err := CheckLookupRedirect(hop("http://books.example/next"), from("http://books.example/first")); err != nil {
			t.Errorf("refused: %v", err)
		}
	})
	t.Run("refused/no originating request", func(t *testing.T) {
		// Unreachable through net/http, which always supplies via. The
		// clause exists so the one guard protecting a credential does not
		// fail open for a future caller.
		refuses(t, CheckLookupRedirect(hop("https://www.googleapis.com/x"), nil), "an empty via compares against nothing")
	})
	t.Run("refused/the hop bound", func(t *testing.T) {
		via := make([]*http.Request, MaxLookupRedirects)
		for i := range via {
			via[i] = hop(origin)
		}
		refuses(t, CheckLookupRedirect(hop("https://www.googleapis.com/books/v1/volumes/abc"), via),
			"without a bound the chain runs forever")
	})
}

// SameHost is the comparison both clients' host checks rest on, so its
// normalisation is pinned directly rather than only through them.
func TestSameHost(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "https://a.example/x", "https://a.example/y", true},
		{"case folds", "https://A.EXAMPLE/x", "https://a.example/y", true},
		{"explicit default port", "https://a.example:443/x", "https://a.example/y", true},
		{"trailing dot", "https://a.example./x", "https://a.example/y", true},
		{"different host", "https://a.example/x", "https://b.example/y", false},
		{"suffix is not a host", "https://a.example.attacker.test/x", "https://a.example/y", false},
		{"different port", "https://a.example:8443/x", "https://a.example/y", false},
		// The one caveat the doc comment names: an http->https upgrade on
		// the same name reads as a port change. Unreachable in production,
		// since both clients start from an https base URL.
		{"scheme change is a port change", "http://a.example/x", "https://a.example/y", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := mustParse(t, c.a)
			b := mustParse(t, c.b)
			if got := SameHost(a, b); got != c.want {
				t.Errorf("SameHost(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
			}
			if got := SameHost(b, a); got != c.want {
				t.Errorf("SameHost is not symmetric for %s and %s", c.a, c.b)
			}
		})
	}
}
