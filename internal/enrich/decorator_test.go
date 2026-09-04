package enrich

import (
	"context"
	"errors"
	"fmt"
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

// retryableError is what a provider returns for the 429/5xx/transport case
// — the only one WithRetry is meant to try again.
func retryableError(text string) error {
	return fmt.Errorf("%s: %w", text, ErrRetryable)
}

func TestWithRetryRetriesServerError(t *testing.T) {
	fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, retryableError("503")
	}}
	p := WithRetry(fake, 3)

	if _, err := p.ByISBN(context.Background(), "1"); err == nil {
		t.Fatal("want error")
	}
	if fake.calls != 3 {
		t.Errorf("calls = %d, want 3", fake.calls)
	}
}

// A 400, a rejected API key or a malformed body fails identically however
// many times it is asked; spending three requests and ~1.5s per book
// discovering that is waste a background worker pays on the whole library.
func TestWithRetryDoesNotRetryANonRetryableError(t *testing.T) {
	fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
		return Metadata{}, errors.New("400 bad request")
	}}
	p := WithRetry(fake, 3)

	if _, err := p.ByISBN(context.Background(), "1"); err == nil {
		t.Fatal("want error")
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (only ErrRetryable is worth another attempt)", fake.calls)
	}
}

// Search must be retried exactly as ByISBN is — it is the path every book
// without an ISBN takes, which is the common FB2 case rather than the rare
// one.
func TestWithRetryRetriesSearchToo(t *testing.T) {
	fake := &fakeProvider{name: "fake", search: func(ctx context.Context, title string, authors []string) (Metadata, error) {
		return Metadata{}, retryableError("503")
	}}
	p := WithRetry(fake, 3)

	if _, err := p.Search(context.Background(), "Dune", nil); err == nil {
		t.Fatal("want error")
	}
	if fake.calls != 3 {
		t.Errorf("calls = %d, want 3", fake.calls)
	}
}

// WithRetry(p, 0) must still make one attempt: zero tries would silently
// disable the provider rather than stop retrying it.
func TestWithRetryAlwaysMakesOneAttempt(t *testing.T) {
	fake := &fakeProvider{name: "fake"}
	if _, err := WithRetry(fake, 0).ByISBN(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1", fake.calls)
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
			return Metadata{}, retryableError("503")
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
		return Metadata{}, retryableError("503")
	}}
	p := WithRetry(fake, 5)

	// Cancellation lands during the first backoff (retryBaseDelay is 500ms,
	// well past this), so exactly one attempt is made. Asserting the exact
	// number rather than "fewer than 5" is the point: a bound that loose
	// passes even when four of five attempts run, i.e. when cancellation is
	// very nearly ignored.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := p.ByISBN(ctx, "1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (cancellation must cut the backoff short)", fake.calls)
	}
}

// WithRateLimit's doc claims ByISBN and Search share one budget; nothing
// asserted it, and Search is the path every book without an ISBN takes.
func TestWithRateLimitSharesOneBudgetAcrossBothMethods(t *testing.T) {
	const every = 50 * time.Millisecond
	fake := &fakeProvider{name: "fake"}
	p := WithRateLimit(fake, every)
	ctx := context.Background()

	start := time.Now()
	mustByISBN(t, p, ctx, "1")
	if _, err := p.Search(ctx, "Dune", nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < every {
		t.Errorf("ByISBN then Search took %s, want at least one interval (%s) — the budget is shared", elapsed, every)
	}
}

func TestWithRateLimitReturnsPromptlyOnACancelledContextInSearch(t *testing.T) {
	fake := &fakeProvider{name: "fake"}
	p := WithRateLimit(fake, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	// The pre-loaded token covers the first call; the second would wait an
	// hour, so cancellation is the only thing that can end it.
	if _, err := p.Search(ctx, "Dune", nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := p.Search(ctx, "Dune II", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 — a cancelled wait must not reach the provider", fake.calls)
	}
}

// The eviction test alone passes for a plain FIFO cache too: nothing there
// re-reads an entry before overflowing. Reading promotes recency, so it is
// the untouched entry that goes.
func TestWithCacheReadPromotesRecency(t *testing.T) {
	fake := &fakeProvider{name: "fake"}
	p := WithCache(fake, 2)
	ctx := context.Background()

	mustByISBN(t, p, ctx, "a")
	mustByISBN(t, p, ctx, "b")
	mustByISBN(t, p, ctx, "a") // a is now the most recently used
	mustByISBN(t, p, ctx, "c") // evicts b, the least recently used
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3 before the eviction check", fake.calls)
	}

	mustByISBN(t, p, ctx, "a")
	if fake.calls != 3 {
		t.Errorf("calls = %d, want 3 — reading a should have kept it from eviction", fake.calls)
	}
	mustByISBN(t, p, ctx, "b")
	if fake.calls != 4 {
		t.Errorf("calls = %d, want 4 — b should have been the one evicted", fake.calls)
	}
}
