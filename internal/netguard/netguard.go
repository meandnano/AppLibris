// Package netguard decides which addresses an outbound fetch whose URL came
// from outside the deployment may connect to. It exists once so that every
// such fetch refuses the same addresses for the same reasons
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// refusedPrefixes are the ranges net.IP's own predicates do not cover.
// Every one of them is reachable from the deployment and not from the
// internet, which is the whole test a fetched host has to pass.
//
// 100.64.0.0/10 is the one that matters most here rather than least: it is
// carrier-grade NAT, and it is also the range Tailscale assigns, so on the
// deployment this project's README recommends every tailnet peer of the
// host sits in it. The rest are ranges no public host can legitimately
// answer from — "this network" (RFC 1122), IETF protocol assignments,
// benchmarking, the reserved former class E, the broadcast address, and
// IPv6 site-local, which is deprecated but still routed by some stacks.
//
// Two IPv6 ranges that embed an IPv4 address are refused whole rather than
// unwrapped like translatedPrefixes: the NAT64 local-use prefix (RFC 8215),
// whose network-specific prefixes vary in length and so carry the address
// at no one offset, and the deprecated IPv4-compatible ::/96, which
// net.IP.To4 does not treat as IPv4 and which stacks with sit0 still route.
// Neither is ever a public host's address
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("::/96"),
}

// translatedPrefixes embed an IPv4 address, at a different offset each, so
// an address inside one is checked again as the IPv4 address it names. On
// a host with either route configured, 64:ff9b::7f00:1 and 2002:7f00:1::
// both reach 127.0.0.1.
//
// The offsets are not interchangeable: NAT64's well-known prefix is /96 and
// carries the address in the last four bytes, while 6to4 is /16 and carries
// it in the four bytes straight after the prefix
var translatedPrefixes = []struct {
	prefix netip.Prefix
	offset int
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},
	{netip.MustParsePrefix("2002::/16"), 2},
}

// RefusePrivateAddress reports an error for any address an outbound fetch
// must not connect to: loopback, an RFC 1918 or IPv6 unique-local address,
// link-local unicast (which is where a cloud metadata endpoint lives),
// multicast, the unspecified address, and the ranges in refusedPrefixes.
// Everything else is allowed.
//
// The URLs these fetches follow are chosen by a third party, and each
// redirect hop by whichever host answered the one before it. Without this
// the fetch is a blind GET at any address the deployment can reach —
// another container on the same Docker network, a tailnet peer, a router's
// admin page, the link-local metadata service.
//
// It is a deny list of the ranges that are unreachable from the internet
// rather than an allow list of public ones: an allow list has to be revised
// every time IANA assigns a block, and every host these fetches want is an
// ordinary public server.
//
// An IPv4-mapped IPv6 address needs no special case: net.IP's predicates
// go through To4, so ::ffff:127.0.0.1 is loopback to all of them, and the
// netip conversion below unmaps for the prefix checks
func RefusePrivateAddress(ip net.IP) error {
	switch {
	case ip == nil:
		return fmt.Errorf("address is not an IP")
	case ip.IsLoopback():
		return fmt.Errorf("address %s is loopback", ip)
	case ip.IsPrivate():
		return fmt.Errorf("address %s is private", ip)
	case ip.IsLinkLocalUnicast():
		return fmt.Errorf("address %s is link-local", ip)
	case ip.IsMulticast():
		return fmt.Errorf("address %s is multicast", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("address %s is unspecified", ip)
	}

	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("address %s is not 4 or 16 bytes", ip)
	}
	addr = addr.Unmap()
	for _, p := range refusedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("address %s is in the reserved range %s", addr, p)
		}
	}
	for _, t := range translatedPrefixes {
		if !t.prefix.Contains(addr) {
			continue
		}
		b := addr.As16()
		if err := RefusePrivateAddress(net.IP(b[t.offset : t.offset+4])); err != nil {
			return fmt.Errorf("address %s translates to a refused address: %w", addr, err)
		}
	}
	return nil
}

// DialContext builds the DialContext a guarded client's transport uses,
// applying guard to the address every connection attempt actually resolves
// to, with timeout bounding each connect.
//
// The check belongs here rather than on the URL's host for two reasons a
// URL check cannot cover. A hostname that resolves to a public address when
// the URL is inspected and a private one when the connection is made — DNS
// rebinding, or simply a short TTL — is caught, because this runs at the
// moment of connection with the address the dialer settled on. And every
// redirect hop goes through the same dialer, so a Location naming a bare
// private IP is refused without the redirect policy having to parse it.
//
// net.Dialer.Control is the hook rather than a resolve-then-dial of our
// own: Go calls it once per candidate address after resolution and before
// the connect, so there is no window between the address being checked and
// the address being used. Resolving by hand and then dialing the hostname
// would reopen exactly that window.
//
// The transport must also carry Proxy: nil, since through a proxy Control
// sees the proxy's address rather than the host the URL names, and must
// leave DialTLSContext unset, so https connects through this dial too
func DialContext(guard func(net.IP) error, timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		// Not logged here: the refusal travels back to the caller as an
		// ordinary fetch failure carrying the address, and the caller logs
		// it with the context this dial does not have
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("dial address %q: %w", address, err)
			}
			return guard(net.ParseIP(host))
		},
	}
	return dialer.DialContext
}
