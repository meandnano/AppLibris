package enrich

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestWithRateLimitPacesCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every = 50 * time.Millisecond
		fake := &fakeProvider{name: "fake"}
		p := WithRateLimit(fake, every)

		start := time.Now()
		mustByISBN(t, p, context.Background(), "1")
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("first call waited %v, want no wait", elapsed)
		}
		mustByISBN(t, p, context.Background(), "2")
		if elapsed := time.Since(start); elapsed != every {
			t.Errorf("second call returned after %v, want exactly %v", elapsed, every)
		}
		if fake.calls != 2 {
			t.Fatalf("calls = %d, want 2", fake.calls)
		}
	})
}

// A call after a quiet spell goes at once, and the next is spaced from it
// rather than from where a fixed schedule would have put the next slot:
// "at most one every interval" holds between any two calls
func TestWithRateLimitSpacesFromTheLastCallNotAFixedSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every = time.Second
		p := WithRateLimit(&fakeProvider{name: "fake"}, every)
		ctx := context.Background()

		mustByISBN(t, p, ctx, "1")
		time.Sleep(1700 * time.Millisecond)

		start := time.Now()
		mustByISBN(t, p, ctx, "2")
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("a call after a quiet spell waited %v, want no wait", elapsed)
		}
		time.Sleep(100 * time.Millisecond)
		start = time.Now()
		mustByISBN(t, p, ctx, "3")
		if elapsed := time.Since(start); elapsed != 900*time.Millisecond {
			t.Errorf("the next call waited %v, want 900ms so it lands a full interval after the one before", elapsed)
		}
	})
}

func TestWithRateLimitReturnsPromptlyOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every = time.Hour
		fake := &fakeProvider{name: "fake"}
		p := WithRateLimit(fake, every)

		// Take the free slot so the next call has to wait
		mustByISBN(t, p, context.Background(), "1")

		const deadline = 20 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()

		start := time.Now()
		_, err := p.ByISBN(ctx, "2")
		elapsed := time.Since(start)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed != deadline {
			t.Errorf("returned after %v, want exactly the context deadline, %v", elapsed, deadline)
		}
		if fake.calls != 1 {
			t.Errorf("calls = %d, want 1 (the second call must never reach the provider)", fake.calls)
		}
	})
}

// A caller that gives up hands its slot back, so the next caller is paced
// from the last call that actually ran rather than pushed a further
// interval out by one that never did
func TestWithRateLimitGivesACancelledSlotBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every = 50 * time.Millisecond
		p := WithRateLimit(&fakeProvider{name: "fake"}, every)

		start := time.Now()
		mustByISBN(t, p, context.Background(), "1")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if _, err := p.ByISBN(ctx, "2"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}

		mustByISBN(t, p, context.Background(), "3")
		if elapsed := time.Since(start); elapsed != every {
			t.Errorf("the call after a cancelled one ran at %v, want %v", elapsed, every)
		}
	})
}

// A caller already queued behind the one that gives up keeps the slot it
// reserved, and the interval after it stays closed: handing a released
// slot back unconditionally would pull next behind a waiter that is still
// going to run, and let two calls through one interval
func TestWithRateLimitKeepsAQueuedSlotWhenAnEarlierCallerCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every = 50 * time.Millisecond
		p := WithRateLimit(&fakeProvider{name: "fake"}, every)

		start := time.Now()
		mustByISBN(t, p, context.Background(), "1")

		// Reserved one after another, so the second is queued behind the
		// first when it is cancelled
		bCtx, cancelB := context.WithCancel(context.Background())
		bErr := make(chan error, 1)
		go func() {
			_, err := p.ByISBN(bCtx, "2")
			bErr <- err
		}()
		synctest.Wait()

		cRan := make(chan time.Duration, 1)
		go func() {
			_, err := p.ByISBN(context.Background(), "3")
			if err != nil {
				cRan <- -1
				return
			}
			cRan <- time.Since(start)
		}()
		synctest.Wait()

		cancelB()
		if err := <-bErr; !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled call returned %v, want context.Canceled", err)
		}

		if elapsed := <-cRan; elapsed != 2*every {
			t.Errorf("the call queued behind the cancelled one ran at %v, want %v", elapsed, 2*every)
		}

		// The discriminating assertion: an unguarded release would have
		// moved next back to the cancelled call's own slot, leaving this
		// one free to run at once alongside the call before it
		mustByISBN(t, p, context.Background(), "4")
		if elapsed := time.Since(start); elapsed != 3*every {
			t.Errorf("the call after the queued one ran at %v, want %v", elapsed, 3*every)
		}
	})
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
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
			return Metadata{}, retryableError("503")
		}}
		p := WithRetry(fake, 3)

		start := time.Now()
		if _, err := p.ByISBN(context.Background(), "1"); err == nil {
			t.Fatal("want error")
		}
		if fake.calls != 3 {
			t.Errorf("calls = %d, want 3", fake.calls)
		}
		if elapsed, want := time.Since(start), retryBaseDelay+2*retryBaseDelay; elapsed != want {
			t.Errorf("three attempts took %v, want %v of backoff", elapsed, want)
		}
	})
}

// The backoff doubles from retryBaseDelay and stops growing at
// retryMaxDelay, so a provider having a long bad moment is asked at a
// steady pace rather than one that runs away
func TestWithRetryBackoffDoublesUpToItsCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var at []time.Duration
		start := time.Now()
		fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
			at = append(at, time.Since(start))
			return Metadata{}, retryableError("503")
		}}

		if _, err := WithRetry(fake, 6).ByISBN(context.Background(), "1"); err == nil {
			t.Fatal("want error")
		}

		want := []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond,
			3500 * time.Millisecond, 7500 * time.Millisecond, 11500 * time.Millisecond}
		if fmt.Sprint(at) != fmt.Sprint(want) {
			t.Errorf("attempts ran at %v, want %v", at, want)
		}
	})
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
	synctest.Test(t, func(t *testing.T) {
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
	})
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
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeProvider{name: "fake"}
		fake.byISBN = func(ctx context.Context, isbn string) (Metadata, error) {
			if fake.calls < 2 {
				return Metadata{}, retryableError("503")
			}
			return Metadata{Title: "Recovered"}, nil
		}
		p := WithRetry(fake, 3)

		start := time.Now()
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
		if elapsed := time.Since(start); elapsed != retryBaseDelay {
			t.Errorf("recovering on the second attempt took %v, want one backoff, %v", elapsed, retryBaseDelay)
		}
	})
}

func TestWithRetryStopsOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeProvider{name: "fake", byISBN: func(ctx context.Context, isbn string) (Metadata, error) {
			return Metadata{}, retryableError("503")
		}}
		p := WithRetry(fake, 5)

		// Cancellation lands during the first backoff, so exactly one attempt
		// is made. Asserting the exact number rather than "fewer than 5" is
		// the point: a bound that loose passes even when four of five
		// attempts run, i.e. when cancellation is very nearly ignored
		const cancelAfter = 10 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(cancelAfter, cancel)

		start := time.Now()
		_, err := p.ByISBN(ctx, "1")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if fake.calls != 1 {
			t.Errorf("calls = %d, want 1 (cancellation must cut the backoff short)", fake.calls)
		}
		if elapsed := time.Since(start); elapsed != cancelAfter {
			t.Errorf("returned after %v, want the moment it was cancelled, %v", elapsed, cancelAfter)
		}
	})
}

// WithRateLimit's doc claims ByISBN and Search share one budget; nothing
// asserted it, and Search is the path every book without an ISBN takes.
func TestWithRateLimitSharesOneBudgetAcrossBothMethods(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const every = 50 * time.Millisecond
		fake := &fakeProvider{name: "fake"}
		p := WithRateLimit(fake, every)
		ctx := context.Background()

		start := time.Now()
		mustByISBN(t, p, ctx, "1")
		if _, err := p.Search(ctx, "Dune", nil); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != every {
			t.Errorf("ByISBN then Search took %s, want exactly one interval (%s) — the budget is shared", elapsed, every)
		}
	})
}

func TestWithRateLimitReturnsPromptlyOnACancelledContextInSearch(t *testing.T) {
	fake := &fakeProvider{name: "fake"}
	p := WithRateLimit(fake, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	// The first call goes at once; the second would wait an hour, so
	// cancellation is the only thing that can end it
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

// There is no expiry here, so a partial answer stored once is served for
// the life of the process: a single failed detail request would cost that
// book its cover and fuller description until a restart. Both methods, so
// the skip cannot be added to one and forgotten on the other.
func TestCacheDoesNotStoreAPartialAnswer(t *testing.T) {
	ctx := context.Background()

	t.Run("ByISBN", func(t *testing.T) {
		partial := &fakeProvider{name: "partial", byISBN: func(context.Context, string) (Metadata, error) {
			return Metadata{Title: "Half An Answer", Partial: true}, nil
		}}
		p := WithCache(partial, DefaultCacheSize)
		mustByISBN(t, p, ctx, "9780000000001")
		mustByISBN(t, p, ctx, "9780000000001")
		if partial.calls != 2 {
			t.Errorf("calls = %d, want 2 — a partial answer must not be remembered", partial.calls)
		}

		whole := &fakeProvider{name: "whole", byISBN: func(context.Context, string) (Metadata, error) {
			return Metadata{Title: "A Whole Answer"}, nil
		}}
		q := WithCache(whole, DefaultCacheSize)
		mustByISBN(t, q, ctx, "9780000000001")
		mustByISBN(t, q, ctx, "9780000000001")
		if whole.calls != 1 {
			t.Errorf("calls = %d, want 1 — a complete answer is still cached", whole.calls)
		}
	})

	t.Run("Search", func(t *testing.T) {
		partial := &fakeProvider{name: "partial", search: func(context.Context, string, []string) (Metadata, error) {
			return Metadata{Title: "Half An Answer", Partial: true}, nil
		}}
		p := WithCache(partial, DefaultCacheSize)
		for range 2 {
			if _, err := p.Search(ctx, "Piranesi", []string{"Susanna Clarke"}); err != nil {
				t.Fatalf("Search: %v", err)
			}
		}
		if partial.calls != 2 {
			t.Errorf("calls = %d, want 2 — a partial answer must not be remembered", partial.calls)
		}

		whole := &fakeProvider{name: "whole", search: func(context.Context, string, []string) (Metadata, error) {
			return Metadata{Title: "A Whole Answer"}, nil
		}}
		q := WithCache(whole, DefaultCacheSize)
		for range 2 {
			if _, err := q.Search(ctx, "Piranesi", []string{"Susanna Clarke"}); err != nil {
				t.Fatalf("Search: %v", err)
			}
		}
		if whole.calls != 1 {
			t.Errorf("calls = %d, want 1 — a complete answer is still cached", whole.calls)
		}
	})
}
