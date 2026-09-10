package enrich

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// MaxCoverBytes bounds a fetched cover image's response body, checked
// before the bytes ever reach internal/cover.Store's image.Decode — the one
// place handing a decoder an arbitrary remote image could make this feature
// allocate without bound. Comfortably above a typical cover JPEG/PNG.
//
// It is deliberately smaller than cover.MaxCoverBytes, the cap the EPUB and
// FB2 readers apply to an embedded cover, and stays a constant of its own
// rather than an alias: this one is a network bound, and internal/googlebooks
// chose which cover size to ask for by measuring against exactly this figure
// (medium over extraLarge, since the latter runs past 512 KiB and would be
// refused). Raising it here would silently make that choice wrong. A
// downloaded cover is always under Store's own cap by construction
const MaxCoverBytes = 512 * 1024

// maxCoverRedirects bounds how far a cover URL may bounce before the fetch
// gives up — the same order as net/http's own default, restated because
// setting CheckRedirect replaces that default outright.
const maxCoverRedirects = 5

// coverUserAgent identifies this client to whichever host serves a cover,
// for the same reason internal/openlibrary and internal/googlebooks each
// set one: Open Library's terms ask for a descriptive agent and throttle
// Go's generic default, and covers.openlibrary.org — the host its own
// cover URLs name — is theirs. A block there arrives as an ordinary fetch
// failure, so every Open Library cover would silently go unstored with
// nothing pointing at the cause.
const coverUserAgent = "library/1.0 (+https://github.com/meandnano/AppLibris)"

// coverSchemeAllowed reports whether a URL is one a cover fetch may follow.
func coverSchemeAllowed(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// refusedPrefixes are the ranges net.IP's own predicates do not cover.
// Every one of them is reachable from the deployment and not from the
// internet, which is the whole test a cover host has to pass.
//
// 100.64.0.0/10 is the one that matters most here rather than least: it is
// carrier-grade NAT, and it is also the range Tailscale assigns, so on the
// deployment this project's README recommends every tailnet peer of the
// host sits in it. The rest are ranges no cover host can legitimately
// answer from — "this network" (RFC 1122), IETF protocol assignments,
// benchmarking, the reserved former class E, the broadcast address, and
// IPv6 site-local, which is deprecated but still routed by some stacks.
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("fec0::/10"),
}

// translatedPrefixes embed an IPv4 address, at a different offset each, so
// an address inside one is checked again as the IPv4 address it names. On
// a host with either route configured, 64:ff9b::7f00:1 and 2002:7f00:1::
// both reach 127.0.0.1.
//
// The offsets are not interchangeable: NAT64's well-known prefix is /96 and
// carries the address in the last four bytes, while 6to4 is /16 and carries
// it in the four bytes straight after the prefix.
var translatedPrefixes = []struct {
	prefix netip.Prefix
	offset int
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},
	{netip.MustParsePrefix("2002::/16"), 2},
}

// RefusePrivateAddress reports an error for any address a cover fetch must
// not connect to: loopback, an RFC 1918 or IPv6 unique-local address,
// link-local unicast (which is where a cloud metadata endpoint lives),
// multicast, the unspecified address, and the ranges in refusedPrefixes.
// Everything else is allowed.
//
// A cover URL is chosen by whichever host answered a provider lookup, and
// each redirect hop by whichever host answered the one before it. Without
// this the fetch is a blind GET at any address the deployment can reach —
// another container on the same Docker network, a tailnet peer, a router's
// admin page, the link-local metadata service. Only image bytes are ever
// kept, so the exposure is small, but "small" is not the same as bounded.
//
// It is a deny list of the ranges that are unreachable from the internet
// rather than an allow list of public ones: an allow list has to be revised
// every time IANA assigns a block, and a cover host is an ordinary public
// server.
//
// An IPv4-mapped IPv6 address needs no special case: net.IP's predicates
// go through To4, so ::ffff:127.0.0.1 is loopback to all of them, and the
// netip conversion below unmaps for the prefix checks.
func RefusePrivateAddress(ip net.IP) error {
	switch {
	case ip == nil:
		return fmt.Errorf("cover address is not an IP")
	case ip.IsLoopback():
		return fmt.Errorf("cover address %s is loopback", ip)
	case ip.IsPrivate():
		return fmt.Errorf("cover address %s is private", ip)
	case ip.IsLinkLocalUnicast():
		return fmt.Errorf("cover address %s is link-local", ip)
	case ip.IsMulticast():
		return fmt.Errorf("cover address %s is multicast", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("cover address %s is unspecified", ip)
	}

	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("cover address %s is not 4 or 16 bytes", ip)
	}
	addr = addr.Unmap()
	for _, p := range refusedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("cover address %s is in the reserved range %s", addr, p)
		}
	}
	for _, t := range translatedPrefixes {
		if !t.prefix.Contains(addr) {
			continue
		}
		b := addr.As16()
		if err := RefusePrivateAddress(net.IP(b[t.offset : t.offset+4])); err != nil {
			return fmt.Errorf("cover address %s translates to a refused address: %w", addr, err)
		}
	}
	return nil
}

// coverDialContext builds the DialContext a cover client uses, applying
// guard to the address every connection attempt actually resolves to.
//
// The check belongs here rather than on the URL's host for two reasons a
// URL check cannot cover. A hostname that resolves to a public address when
// the URL is inspected and a private one when the connection is made — DNS
// rebinding, or simply a short TTL — is caught, because this runs at the
// moment of connection with the address the dialer settled on. And every
// redirect hop goes through the same dialer, so a Location naming a bare
// private IP is refused without CheckCoverRedirect having to parse it.
//
// net.Dialer.Control is the hook rather than a resolve-then-dial of our
// own: Go calls it once per candidate address after resolution and before
// the connect, so there is no window between the address being checked and
// the address being used. Resolving by hand and then dialing the hostname
// would reopen exactly that window.
func coverDialContext(guard func(net.IP) error) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   coverFetchTimeout,
		KeepAlive: 30 * time.Second,
		// Not logged here: the refusal travels back through FetchCover as
		// an ordinary fetch failure and storeCover already warns with it,
		// and the message carries the address, so a line here would say
		// the same thing twice with less context.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("cover dial address %q: %w", address, err)
			}
			return guard(net.ParseIP(host))
		},
	}
	return dialer.DialContext
}

// CheckCoverRedirect is the CheckRedirect a client passed to FetchCover
// should carry. Without it the scheme check below guards only the first
// URL, and every hop after it — chosen by whatever host answered, not by
// the provider whose response named the cover — is followed unexamined.
func CheckCoverRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxCoverRedirects {
		return fmt.Errorf("stopped after %d redirects", maxCoverRedirects)
	}
	if !coverSchemeAllowed(req.URL.Scheme) {
		return fmt.Errorf("cover redirect scheme %q is not http or https", req.URL.Scheme)
	}
	return nil
}

// FetchCover downloads the image at rawURL through client, capped at
// MaxCoverBytes read before any decoding is attempted anywhere. rawURL ==
// "" (no cover known) returns no bytes and no error. A non-200 response, a
// transport failure, or a body over the cap are all errors: the caller's
// own answer (the rest of a provider's Metadata) must never be blocked on
// this, so a caller logs and otherwise ignores this error rather than
// treating it as a ByISBN/Search failure.
func FetchCover(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	if rawURL == "" {
		return nil, nil
	}

	// The URL comes out of a third party's response body, so the scheme is
	// checked rather than assumed: http and https are the only ones a cover
	// can legitimately use, and refusing the rest keeps a hostile or
	// mangled response from turning this into a fetch of file:// or any
	// other scheme a future http.Client transport might be taught.
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse cover url: %w", err)
	}
	if !coverSchemeAllowed(parsed.Scheme) {
		return nil, fmt.Errorf("cover url scheme %q is not http or https", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build cover request: %w", err)
	}
	req.Header.Set("User-Agent", coverUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cover request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cover request: unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxCoverBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read cover body: %w", err)
	}
	if len(data) > MaxCoverBytes {
		return nil, fmt.Errorf("cover response exceeds %d bytes", MaxCoverBytes)
	}
	return data, nil
}
