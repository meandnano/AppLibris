package netguard

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// The whole point of the guard is which addresses it names, so the table is
// the test. A host a guarded fetch wants is an ordinary public server, and
// everything else it could reach is refused
func TestRefusePrivateAddress(t *testing.T) {
	refused := []string{
		"127.0.0.1",       // loopback
		"127.1.2.3",       // the rest of 127/8, not just .1
		"::1",             // loopback, v6
		"10.0.0.1",        // RFC 1918
		"172.16.0.1",      // RFC 1918
		"192.168.1.1",     // RFC 1918
		"fc00::1",         // IPv6 unique-local
		"fd12:3456::1",    // IPv6 unique-local
		"169.254.169.254", // the cloud metadata endpoint
		"fe80::1",         // link-local, v6
		"224.0.0.1",       // multicast
		"ff02::1",         // multicast, v6
		"ff01::1",         // interface-local multicast
		"0.0.0.0",         // unspecified
		"::",              // unspecified, v6

		// An IPv4-mapped form must not be a way around any of the above.
		// net.IP's predicates go through To4, so these are covered by the
		// same clauses — pinned because that is a property of the stdlib
		// rather than of this function.
		"::ffff:127.0.0.1",
		"::ffff:10.0.0.1",
		"::ffff:169.254.169.254",

		// The ranges net.IP has no predicate for.
		"100.64.0.1",      // CGNAT, and the range Tailscale assigns
		"100.100.100.100", // the middle of it, not just the edge
		"0.1.2.3",         // "this network"
		"192.0.0.1",       // IETF protocol assignments
		"198.18.0.1",      // benchmarking
		"240.0.0.1",       // reserved
		"255.255.255.255", // broadcast
		"fec0::1",         // deprecated IPv6 site-local
		"64:ff9b::7f00:1", // NAT64 of 127.0.0.1
		"64:ff9b::a00:1",  // NAT64 of 10.0.0.1
		"2002:7f00:1::1",  // 6to4 of 127.0.0.1
		"2002:6440:1::1",  // 6to4 of 100.64.0.1
	}
	for _, addr := range refused {
		t.Run("refused/"+addr, func(t *testing.T) {
			if err := RefusePrivateAddress(net.ParseIP(addr)); err == nil {
				t.Errorf("RefusePrivateAddress(%s) = nil, want a refusal", addr)
			}
		})
	}

	allowed := []string{
		"93.184.216.34",
		"2606:2800:220:1:248:1893:25c8:1946",
		"8.8.8.8",
		// Adjacent to a refused range on either side, so a bounds slip
		// shows up as a refused real host rather than silently.
		"9.255.255.255",
		"11.0.0.1",
		"172.15.255.255",
		"172.32.0.1",
		"100.63.255.255",
		"100.128.0.1",
		"198.17.255.255",
		"198.20.0.1",
		"223.255.255.255", // just below the multicast range
		// A 6to4 address embedding a public IPv4 is an ordinary host.
		"2002:5db8:d822::1",
	}
	for _, addr := range allowed {
		t.Run("allowed/"+addr, func(t *testing.T) {
			if err := RefusePrivateAddress(net.ParseIP(addr)); err != nil {
				t.Errorf("RefusePrivateAddress(%s) = %v, want nil", addr, err)
			}
		})
	}

	// An unparseable address is not evidence of anything, and a dial that
	// cannot say where it is going is one to refuse.
	if err := RefusePrivateAddress(nil); err == nil {
		t.Error("RefusePrivateAddress(nil) = nil, want a refusal")
	}
}

// The guard is handed the address the dial actually connects to, and its
// refusal is the dial's error
func TestDialContextAppliesTheGuardToTheDialedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var seen net.IP
	allow := func(ip net.IP) error {
		seen = ip
		return nil
	}
	conn, err := DialContext(allow, time.Second)(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("allowed dial: %v", err)
	}
	conn.Close()
	if !seen.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("the guard saw %v, want 127.0.0.1", seen)
	}

	refused := errors.New("refused by the test guard")
	deny := func(net.IP) error { return refused }
	if _, err := DialContext(deny, time.Second)(context.Background(), "tcp", ln.Addr().String()); !errors.Is(err, refused) {
		t.Errorf("refused dial = %v, want the guard's refusal", err)
	}
}
