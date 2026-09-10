package enrich

import "net"

// allowAnyCoverAddress lets a Worker's cover fetch reach any address,
// including the loopback one every httptest.Server in this package listens
// on. RefusePrivateAddress refuses that by design, so a cover test either
// opts out here or is testing the guard rather than the thing it named.
//
// This lives in a _test.go file on purpose: production code has no way to
// switch the guard off, and the opt-out is one call a reader can grep for.
// A test asserting the guard's own behaviour must not call it.
func allowAnyCoverAddress(w *Worker) {
	w.dialGuard = func(net.IP) error { return nil }
}
