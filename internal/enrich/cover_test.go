package enrich

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
