package providers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"library/internal/enrich"
)

func TestResolveOrderPreserved(t *testing.T) {
	got, err := Resolve([]string{"googlebooks", "openlibrary"}, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Resolve: got %d providers, want 2", len(got))
	}
	if got[0].Name() != "googlebooks" {
		t.Errorf("Resolve[0].Name() = %q, want %q", got[0].Name(), "googlebooks")
	}
	if got[1].Name() != "openlibrary" {
		t.Errorf("Resolve[1].Name() = %q, want %q", got[1].Name(), "openlibrary")
	}
}

func TestResolveUnknownNameNamesIt(t *testing.T) {
	_, err := Resolve([]string{"openlibrary", "bogus"}, "")
	if err == nil {
		t.Fatal("Resolve with an unknown name: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("Resolve error %q does not name the unknown provider", err.Error())
	}
	if !strings.Contains(err.Error(), "googlebooks") || !strings.Contains(err.Error(), "openlibrary") {
		t.Errorf("Resolve error %q does not list the valid providers", err.Error())
	}
}

func TestResolveEmptyIsNoProviders(t *testing.T) {
	got, err := Resolve(nil, "")
	if err != nil {
		t.Fatalf("Resolve(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Resolve(nil) = %d providers, want 0", len(got))
	}
	// Non-nil is the documented contract, so a caller ranging over the
	// result never has to distinguish "disabled" from "not configured".
	if got == nil {
		t.Error("Resolve(nil) = nil, want an empty non-nil slice")
	}
}

func TestParseNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"default pair", "openlibrary,googlebooks", []string{"openlibrary", "googlebooks"}},
		{"empty disables", "", nil},
		{"blank disables", "   ", nil},
		{"whitespace and empty entries trimmed", " openlibrary ,, googlebooks ", []string{"openlibrary", "googlebooks"}},
		{"a repeated name is kept once, at its first position", "googlebooks,openlibrary,googlebooks", []string{"googlebooks", "openlibrary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseNames(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseNames(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ParseNames(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// countingProvider records how often it is asked and what it answers, so a
// test can observe the decorator chain from outside it.
type countingProvider struct {
	calls  int
	answer func(int) (enrich.Metadata, error)
}

func (c *countingProvider) Name() string { return "counting" }

func (c *countingProvider) ByISBN(ctx context.Context, isbn string) (enrich.Metadata, error) {
	c.calls++
	if c.answer != nil {
		return c.answer(c.calls)
	}
	return enrich.Metadata{Title: "Answered"}, nil
}

func (c *countingProvider) Search(ctx context.Context, title string, authors []string) (enrich.Metadata, error) {
	c.calls++
	return enrich.Metadata{}, nil
}

// Every decorator promotes Name() through its embedded Provider, so the
// other tests here pass whether compose wraps correctly, wraps in the wrong
// order, or does nothing at all. These drive the chain instead.
func TestComposeCachesRepeatLookups(t *testing.T) {
	inner := &countingProvider{}
	p := compose(inner)

	for range 3 {
		if _, err := p.ByISBN(context.Background(), "9780262011532"); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls != 1 {
		t.Errorf("inner calls = %d, want 1 (a repeat lookup must be served from the cache)", inner.calls)
	}
}

// The order is the thing under test: with the cache outermost, an answer
// already in memory costs no rate-limit token, so three repeat lookups
// return promptly rather than taking two full DefaultRateLimitInterval
// waits between them.
func TestComposeCachedHitCostsNoRateLimitToken(t *testing.T) {
	p := compose(&countingProvider{})

	start := time.Now()
	for range 3 {
		if _, err := p.ByISBN(context.Background(), "9780262011532"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed >= enrich.DefaultRateLimitInterval {
		t.Errorf("three repeat lookups took %s, want well under one rate-limit interval (%s)", elapsed, enrich.DefaultRateLimitInterval)
	}
}

// A retryable failure must reach the wrapped provider DefaultRetryAttempts
// times, and must not be cached: the four-case contract treats an error as
// transient, so a later lookup asks again.
func TestComposeRetriesAndNeverCachesAnError(t *testing.T) {
	inner := &countingProvider{answer: func(int) (enrich.Metadata, error) {
		return enrich.Metadata{}, fmt.Errorf("503: %w", enrich.ErrRetryable)
	}}
	p := compose(inner)

	if _, err := p.ByISBN(context.Background(), "9780262011532"); err == nil {
		t.Fatal("want an error")
	}
	if inner.calls != enrich.DefaultRetryAttempts {
		t.Errorf("inner calls = %d, want %d", inner.calls, enrich.DefaultRetryAttempts)
	}
}
