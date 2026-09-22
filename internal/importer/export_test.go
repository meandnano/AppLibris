package importer

import "net/http"

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
