package enrich

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// testCoverURL sits under .test, which never resolves, so a client that
// somehow missed the in-memory transport fails rather than fetching
// anything real
const testCoverURL = "http://covers.test/cover.jpg"

// testCoverClient is an in-memory server's client carrying the production
// redirect policy, which httptest's own client does not: without it the
// redirect tests would assert net/http's default hop limit and no scheme
// check at all
func testCoverClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	client := httptest.NewTestServer(t, handler).Client()
	client.CheckRedirect = CheckCoverRedirect
	return client
}

func TestFetchCoverEmptyURLReturnsNothing(t *testing.T) {
	hits := 0
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
	})

	data, err := FetchCover(context.Background(), client, "")
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
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(want)
	})

	got, err := FetchCover(context.Background(), client, testCoverURL)
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
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(oversized)
	})

	data, err := FetchCover(context.Background(), client, testCoverURL)
	if err == nil {
		t.Fatal("FetchCover: want error for a body over MaxCoverBytes, got nil")
	}
	if data != nil {
		t.Errorf("data = %q, want nil", data)
	}
}

func TestFetchCoverAcceptsBodyExactlyAtCap(t *testing.T) {
	want := bytes.Repeat([]byte{0xAB}, MaxCoverBytes)
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(want)
	})

	got, err := FetchCover(context.Background(), client, testCoverURL)
	if err != nil {
		t.Fatalf("FetchCover: want nil error for a body exactly at MaxCoverBytes, got %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("data length = %d, want %d", len(got), len(want))
	}
}

func TestFetchCoverNon200IsAnError(t *testing.T) {
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	data, err := FetchCover(context.Background(), client, testCoverURL)
	if err == nil {
		t.Fatal("FetchCover: want error on 404, got nil")
	}
	if data != nil {
		t.Errorf("data = %q, want nil", data)
	}
}

func TestFetchCoverTransportErrorIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The handler never answers, so only the caller's deadline ends the
		// request
		client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()

		_, err := FetchCover(ctx, client, testCoverURL)
		if err == nil {
			t.Fatal("FetchCover: want error on a timed-out request, got nil")
		}
	})
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
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://elsewhere.invalid/cover.jpg", http.StatusFound)
	})

	if _, err := FetchCover(context.Background(), client, testCoverURL); err == nil {
		t.Fatal("FetchCover succeeded, want an error for a redirect off http/https")
	}
}

// A redirect loop must end in an error rather than spinning until the
// client's own timeout, which is the whole enrichment job's budget.
func TestFetchCoverStopsAfterTooManyRedirects(t *testing.T) {
	hits := 0
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/cover.jpg", http.StatusFound)
	})

	if _, err := FetchCover(context.Background(), client, testCoverURL); err == nil {
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
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved.jpg" {
			w.Write(want)
			return
		}
		http.Redirect(w, r, "/moved.jpg", http.StatusFound)
	})

	got, err := FetchCover(context.Background(), client, testCoverURL)
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
	client := testCoverClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write([]byte("fake-jpeg-bytes"))
	})

	if _, err := FetchCover(context.Background(), client, testCoverURL); err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if got != coverUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, coverUserAgent)
	}
}
