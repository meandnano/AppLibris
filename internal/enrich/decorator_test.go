package enrich

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithRateLimitPacesCalls(t *testing.T) {
	const every = 50 * time.Millisecond
	fake := &fakeProvider{name: "fake"}
	p := WithRateLimit(fake, every)

	start := time.Now()
	if _, err := p.ByISBN(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ByISBN(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < every {
		t.Errorf("second call returned after %v, want at least %v", elapsed, every)
	}
	if fake.calls != 2 {
		t.Fatalf("calls = %d, want 2", fake.calls)
	}
}

func TestWithRateLimitReturnsPromptlyOnCancellation(t *testing.T) {
	const every = time.Hour // long enough the token never refills during this test
	fake := &fakeProvider{name: "fake"}
	p := WithRateLimit(fake, every)

	// Consume the pre-loaded token so the next call has to wait.
	if _, err := p.ByISBN(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := p.ByISBN(ctx, "2")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("returned after %v, want promptly after the context deadline", elapsed)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (the second call must never reach the provider)", fake.calls)
	}
}

func TestWithCacheServesRepeatWithoutSecondCall(t *testing.T) {
	fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Title: "Cached Book"}, nil
	}}
	p := WithCache(fake, 10)

	for i := range 2 {
		m, err := p.ByISBN(context.Background(), "9780000000001")
		if err != nil {
			t.Fatal(err)
		}
		if m.Title != "Cached Book" {
			t.Errorf("call %d: title = %q, want %q", i, m.Title, "Cached Book")
		}
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (the second lookup should be served from cache)", fake.calls)
	}
}

func TestWithCacheServesNoMatch(t *testing.T) {
	fake := &fakeProvider{name: "fake"} // byISBN is nil -> Metadata{}, nil
	p := WithCache(fake, 10)

	for range 2 {
		if _, err := p.ByISBN(context.Background(), "no-match"); err != nil {
			t.Fatal(err)
		}
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (a cached no-match must not be re-asked)", fake.calls)
	}
}

func TestWithCacheNeverCachesAnError(t *testing.T) {
	fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("boom")
	}}
	p := WithCache(fake, 10)

	for range 2 {
		if _, err := p.ByISBN(context.Background(), "1"); err == nil {
			t.Fatal("want error")
		}
	}
	if fake.calls != 2 {
		t.Errorf("calls = %d, want 2 (an error must never be served from cache)", fake.calls)
	}
}

func TestWithCacheEvictsOldestWhenFull(t *testing.T) {
	fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{Title: isbn}, nil
	}}
	p := WithCache(fake, 2)

	ctx := context.Background()
	mustByISBN(t, p, ctx, "a")
	mustByISBN(t, p, ctx, "b")
	mustByISBN(t, p, ctx, "c") // over the size-2 cap: evicts "a", the oldest

	mustByISBN(t, p, ctx, "a")
	if fake.calls != 4 {
		t.Errorf("calls = %d, want 4 (the evicted entry must be re-fetched)", fake.calls)
	}
}

func TestWithCacheSearchKeyIncludesAuthors(t *testing.T) {
	fake := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{Title: title}, nil
	}}
	p := WithCache(fake, 10)
	ctx := context.Background()

	if _, err := p.Search(ctx, "Dune", []string{"Herbert"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Search(ctx, "Dune", []string{"Someone Else"}); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 {
		t.Errorf("calls = %d, want 2 (different authors must be a different cache key)", fake.calls)
	}

	if _, err := p.Search(ctx, "Dune", []string{"Herbert"}); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 {
		t.Errorf("calls = %d, want 2 (repeating the first lookup should hit the cache)", fake.calls)
	}
}

func mustByISBN(t *testing.T, p Provider, ctx context.Context, isbn string) {
	t.Helper()
	if _, err := p.ByISBN(ctx, isbn); err != nil {
		t.Fatal(err)
	}
}

func TestWithRetryRetriesServerError(t *testing.T) {
	fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("503")
	}}
	p := WithRetry(fake, 3)

	if _, err := p.ByISBN(context.Background(), "1"); err == nil {
		t.Fatal("want error")
	}
	if fake.calls != 3 {
		t.Errorf("calls = %d, want 3", fake.calls)
	}
}

func TestWithRetryDoesNotRetryNoMatch(t *testing.T) {
	fake := &fakeProvider{name: "fake"} // Metadata{}, nil
	p := WithRetry(fake, 3)

	if _, err := p.ByISBN(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (a no-match answer must never be retried)", fake.calls)
	}
}

func TestWithRetrySucceedsAfterTransientError(t *testing.T) {
	fake := &fakeProvider{name: "fake"}
	fake.byISBN = func(ctx context.Context, isbn string) (Metadata, error) {
		if fake.calls < 2 {
			return Metadata{}, errors.New("503")
		}
		return Metadata{Title: "Recovered"}, nil
	}
	p := WithRetry(fake, 3)

	m, err := p.ByISBN(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Recovered" {
		t.Errorf("title = %q, want %q", m.Title, "Recovered")
	}
	if fake.calls != 2 {
		t.Errorf("calls = %d, want 2", fake.calls)
	}
}

func TestWithRetryStopsOnCancellation(t *testing.T) {
	fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("503")
	}}
	p := WithRetry(fake, 5)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := p.ByISBN(ctx, "1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fake.calls >= 5 {
		t.Errorf("calls = %d, want fewer than the full attempt budget (cancellation should cut it short)", fake.calls)
	}
}
