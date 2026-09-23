package importer

import (
	"context"
	"net"
	"net/http"
)

// useFetchTransport sends a Fetcher's requests through rt, an in-memory
// server's transport, so a test about what Fetch does with a response
// needs no socket. That transport never dials, so the address guard never
// runs: a test about the guard must not call this.
//
// This lives in a _test.go file so production code has no way to replace
// the guarded transport
func useFetchTransport(f *Fetcher, rt http.RoundTripper) {
	f.client.Transport = rt
}

// dialHostAt connects a Fetcher's dials to host straight to addr, past the
// guard, and every other dial through the guard as before. It stands a
// public host in for a socket on loopback, so a test can reach one hop and
// have the real guard judge the next
func dialHostAt(f *Fetcher, host, addr string) {
	transport := f.client.Transport.(*http.Transport)
	guarded := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == host {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		return guarded(ctx, network, address)
	}
}
