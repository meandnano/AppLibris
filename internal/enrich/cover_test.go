package enrich

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchCoverEmptyURLReturnsNothing(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)

	data, err := FetchCover(context.Background(), server.Client(), "")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if data != nil {
		t.Errorf("data = %q, want nil", data)
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 — an empty URL has nothing to fetch", hits)
	}
}

func TestFetchCoverDownloadsBody(t *testing.T) {
	want := []byte("fake-jpeg-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(want)
	}))
	t.Cleanup(server.Close)

	got, err := FetchCover(context.Background(), server.Client(), server.URL+"/cover.jpg")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("data = %q, want %q", got, want)
	}
}

// The rejection must happen on size alone, before anything tries to decode
// the body — junk bytes over the cap are rejected exactly like an oversized
// real image would be.
func TestFetchCoverRejectsOversizedBodyBeforeDecoding(t *testing.T) {
	oversized := bytes.Repeat([]byte{0xFF}, MaxCoverBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(oversized)
	}))
	t.Cleanup(server.Close)

	data, err := FetchCover(context.Background(), server.Client(), server.URL+"/cover.jpg")
	if err == nil {
		t.Fatal("FetchCover: want error for a body over MaxCoverBytes, got nil")
	}
	if data != nil {
		t.Errorf("data = %q, want nil", data)
	}
}

func TestFetchCoverAcceptsBodyExactlyAtCap(t *testing.T) {
	want := bytes.Repeat([]byte{0xAB}, MaxCoverBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(want)
	}))
	t.Cleanup(server.Close)

	got, err := FetchCover(context.Background(), server.Client(), server.URL+"/cover.jpg")
	if err != nil {
		t.Fatalf("FetchCover: want nil error for a body exactly at MaxCoverBytes, got %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("data length = %d, want %d", len(got), len(want))
	}
}

func TestFetchCoverNon200IsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	data, err := FetchCover(context.Background(), server.Client(), server.URL+"/cover.jpg")
	if err == nil {
		t.Fatal("FetchCover: want error on 404, got nil")
	}
	if data != nil {
		t.Errorf("data = %q, want nil", data)
	}
}

func TestFetchCoverTransportErrorIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := FetchCover(ctx, server.Client(), server.URL+"/cover.jpg")
	if err == nil {
		t.Fatal("FetchCover: want error on a timed-out request, got nil")
	}
}

// The URL comes out of a third party's response body, so a scheme other
// than http or https is refused before any request is issued.
func TestFetchCoverRefusesANonHTTPScheme(t *testing.T) {
	for _, rawURL := range []string{"file:///etc/passwd", "ftp://example.com/cover.jpg", "gopher://example.com"} {
		_, err := FetchCover(context.Background(), http.DefaultClient, rawURL)
		if err == nil {
			t.Errorf("FetchCover(%q): want an error", rawURL)
			continue
		}
		if !strings.Contains(err.Error(), "not http or https") {
			t.Errorf("FetchCover(%q) error = %v, want it to name the scheme rule", rawURL, err)
		}
	}
}

// The scheme check guards the URL a provider named; a redirect is a URL
// whatever host answered chose, so it gets the same check rather than being
// followed on the strength of the first hop having passed.
func TestFetchCoverRefusesARedirectToANonHTTPScheme(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://elsewhere.invalid/cover.jpg", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	client := &http.Client{CheckRedirect: CheckCoverRedirect}
	if _, err := FetchCover(context.Background(), client, server.URL+"/cover.jpg"); err == nil {
		t.Fatal("FetchCover succeeded, want an error for a redirect off http/https")
	}
}

// A redirect loop must end in an error rather than spinning until the
// client's own timeout, which is the whole enrichment job's budget.
func TestFetchCoverStopsAfterTooManyRedirects(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/cover.jpg", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	client := &http.Client{CheckRedirect: CheckCoverRedirect}
	if _, err := FetchCover(context.Background(), client, server.URL+"/cover.jpg"); err == nil {
		t.Fatal("FetchCover succeeded, want an error for an endless redirect")
	}
	if hits > maxCoverRedirects+1 {
		t.Errorf("server hits = %d, want at most %d", hits, maxCoverRedirects+1)
	}
}

// A redirect within http/https is ordinary — Open Library's covers host
// answers one — so the guard must not cost the fetch itself.
func TestFetchCoverFollowsAnHTTPRedirect(t *testing.T) {
	want := []byte("fake-jpeg-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved.jpg" {
			w.Write(want)
			return
		}
		http.Redirect(w, r, "/moved.jpg", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	client := server.Client()
	client.CheckRedirect = CheckCoverRedirect
	got, err := FetchCover(context.Background(), client, server.URL+"/cover.jpg")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("data = %q, want %q", got, want)
	}
}

// Open Library throttles Go's generic default agent, and the cover host its
// own answers name is theirs — a block there is indistinguishable from any
// other fetch failure, so the header is asserted rather than assumed.
func TestFetchCoverSendsAUserAgent(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write([]byte("fake-jpeg-bytes"))
	}))
	t.Cleanup(server.Close)

	if _, err := FetchCover(context.Background(), server.Client(), server.URL+"/cover.jpg"); err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if got != coverUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, coverUserAgent)
	}
}

// The whole point of the guard is which addresses it names, so the table is
// the test. A cover host is an ordinary public server; everything a cover
// fetch could reach that is not one is refused.
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
		// shows up as a refused real cover host rather than silently.
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
