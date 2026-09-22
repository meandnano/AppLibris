package enrich

import (
	"net"
	"net/http"
)

// allowAnyCoverAddress lets a Worker's cover fetch reach any address,
// including the loopback one the worker tests' cover servers listen on.
// RefusePrivateAddress refuses that by design, so a cover test either opts
// out here or is testing the guard rather than the thing it named.
//
// This lives in a _test.go file on purpose: production code has no way to
// switch the guard off, and the opt-out is one call a reader can grep for.
// A test asserting the guard's own behaviour must not call it.
func allowAnyCoverAddress(w *Worker) {
	w.dialGuard = func(net.IP) error { return nil }
}

// useCoverTransport sends a Worker's cover fetches through rt, an in-memory
// server's transport, so a test about what the worker does with a fetch can
// run on synctest's clock. That transport never dials, so the address guard
// never runs: a test about the guard must not call this either
func useCoverTransport(w *Worker, rt http.RoundTripper) {
	w.coverClient.Transport = rt
}
