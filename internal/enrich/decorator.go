package enrich

import (
	"container/list"
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// DefaultRateLimitInterval paces provider calls at roughly one per second —
// a conservative default for a provider whose only stated limit is a
// courtesy ask rather than an enforced one (Open Library's Search API is
// exactly this: no documented hard limit, a request to keep it reasonable).
const DefaultRateLimitInterval = 1 * time.Second

// WithRateLimit paces p's calls — ByISBN and Search share one budget — to
// at most one every `every`. The wait honours ctx cancellation, so a
// shutdown mid-wait returns promptly instead of blocking until the next
// slot opens. internal/providers wraps it closest to the client, so every
// attempt WithRetry makes takes its own token: a provider answering 429 is
// exactly the one that must not then be sent a burst.
func WithRateLimit(p Provider, every time.Duration) Provider {
	return &rateLimitedProvider{Provider: p, limiter: newRateLimiter(every)}
}

type rateLimitedProvider struct {
	Provider
	limiter *rateLimiter
}

func (r *rateLimitedProvider) ByISBN(ctx context.Context, isbn string) (Metadata, error) {
	if err := r.limiter.wait(ctx); err != nil {
		return Metadata{}, err
	}
	return r.Provider.ByISBN(ctx, isbn)
}

func (r *rateLimitedProvider) Search(ctx context.Context, title string, authors []string) (Metadata, error) {
	if err := r.limiter.wait(ctx); err != nil {
		return Metadata{}, err
	}
	return r.Provider.Search(ctx, title, authors)
}

// rateLimiter spaces calls at least `every` apart by handing each caller the
// next free slot. A caller arriving after a quiet spell gets a slot of now,
// so the first call never pays a full interval of latency for no reason.
// Nothing runs between calls: a limiter owning a goroutine would outlive
// every caller, since nothing that builds one ever closes it, and a
// synctest bubble refuses to end while one is left
type rateLimiter struct {
	every time.Duration

	mu sync.Mutex
	// next is the earliest instant the next caller may proceed
	next time.Time
}

func newRateLimiter(every time.Duration) *rateLimiter {
	// WithRateLimit is exported, so falling back to the default matches how
	// WithRetry and newCache normalise an unusable argument rather than
	// pacing nothing
	if every <= 0 {
		every = DefaultRateLimitInterval
	}
	return &rateLimiter{every: every}
}

func (rl *rateLimiter) wait(ctx context.Context) error {
	rl.mu.Lock()
	now := time.Now()
	slot := rl.next
	if slot.Before(now) {
		slot = now
	}
	rl.next = slot.Add(rl.every)
	rl.mu.Unlock()

	delay := slot.Sub(now)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		rl.release(slot)
		return ctx.Err()
	}
}

// release hands back a slot its caller gave up waiting for, unless a later
// caller has already queued behind it: moving next back then would let two
// calls through one interval
func (rl *rateLimiter) release(slot time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.next.Equal(slot.Add(rl.every)) {
		rl.next = slot
	}
}

// DefaultCacheSize bounds a provider's in-memory answer cache — enough to
// absorb one enrichment sweep's worth of repeat lookups without growing
// without bound. The cache exists to stop a burst of jobs hammering the
// same ISBN, not to be a long-lived store: it holds no state across a
// process restart.
const DefaultCacheSize = 512

// WithCache serves a repeat lookup — same method, same arguments — out of
// a bounded in-memory cache instead of calling p again. A "no match"
// answer is cached too: without that, a shelf of obscure books the
// library re-enriches on every sweep would re-ask a provider for the same
// negative answer forever. An error is never cached — docs/notes/enrichment.md's
// four-case contract treats an error as a transient, retryable condition,
// not an answer worth remembering.
//
// Nor is an answer marked Metadata.Partial, for the same reason one step
// further in: the provider spoke, so it is not an error, but it is missing
// something a later attempt could supply, and there is no expiry here to
// undo the mistake. Storing it would turn one failed request into a
// permanently degraded answer for that key until the process restarts.
// This is deliberately narrower than a TTL, which would change the
// argument the cache rests on; the specific thing not worth remembering is
// the thing that says so.
//
// internal/providers wraps it outermost, so a hit costs neither a
// rate-limit token nor a retry attempt — the whole point of having
// answered once already. Cached values are Metadata, which holds only
// strings: a cover's bytes are the Worker's to fetch and never enter this
// map, so a full cache is kilobytes rather than the hundreds of megabytes
// MaxCoverBytes times DefaultCacheSize would allow.
func WithCache(p Provider, size int) Provider {
	return &cachedProvider{Provider: p, cache: newCache(size)}
}

// cacheKey distinguishes ByISBN from Search so the two methods' answers
// can never collide, and folds a Search's title and authors into one
// string — authors included, since two lookups sharing a title but not an
// author are two different questions.
type cacheKey struct {
	method string
	key    string
}

func searchCacheKey(title string, authors []string) string {
	return title + "\x00" + strings.Join(authors, "\x00")
}

type cachedProvider struct {
	Provider
	cache *cache
}

func (c *cachedProvider) ByISBN(ctx context.Context, isbn string) (Metadata, error) {
	key := cacheKey{method: "ByISBN", key: isbn}
	if m, ok := c.cache.get(key); ok {
		return m, nil
	}
	m, err := c.Provider.ByISBN(ctx, isbn)
	if err != nil {
		return m, err
	}
	if !m.Partial {
		c.cache.put(key, m)
	}
	return m, nil
}

func (c *cachedProvider) Search(ctx context.Context, title string, authors []string) (Metadata, error) {
	key := cacheKey{method: "Search", key: searchCacheKey(title, authors)}
	if m, ok := c.cache.get(key); ok {
		return m, nil
	}
	m, err := c.Provider.Search(ctx, title, authors)
	if err != nil {
		return m, err
	}
	if !m.Partial {
		c.cache.put(key, m)
	}
	return m, nil
}

// cache is a bounded, in-memory, least-recently-used cache from cacheKey to
// Metadata. size <= 0 disables eviction, which no caller here relies on.
type cache struct {
	mu    sync.Mutex
	size  int
	order *list.List
	items map[cacheKey]*list.Element
}

type cacheEntry struct {
	key   cacheKey
	value Metadata
}

func newCache(size int) *cache {
	return &cache{size: size, order: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *cache) get(key cacheKey) (Metadata, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return Metadata{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).value, true
}

func (c *cache) put(key cacheKey, value Metadata) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		el.Value.(*cacheEntry).value = value
		c.order.MoveToFront(el)
		return
	}

	el := c.order.PushFront(&cacheEntry{key: key, value: value})
	c.items[key] = el
	if c.size > 0 && c.order.Len() > c.size {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}

// DefaultRetryAttempts is how many total tries WithRetry makes before
// giving up on a retryable error — the first attempt plus two retries.
const DefaultRetryAttempts = 3

// retryBaseDelay and retryMaxDelay bound the backoff between attempts:
// doubling from half a second, capped at a few seconds, so a provider
// having a bad moment gets some breathing room without the enrichment
// worker stalling on it for long — enrichment is a background nicety, per
// the same reasoning behind each provider's own short Timeout.
const (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 4 * time.Second
)

// WithRetry retries p's call when it fails with an error wrapping
// ErrRetryable — the 429/5xx/transport case docs/notes/enrichment.md's four-case contract
// draws a hard line around — up to attempts total tries, with a backoff
// and a ctx check between them. Any other error is returned on the first
// attempt: a 400, a rejected API key or a malformed body will fail the
// same way however many times it is asked, and spending three requests and
// ~1.5s per book discovering that is waste a background worker pays on
// every book in the library. A "no match" (zero Metadata, nil error) is
// never retried either: it is not a failure, it is an answer.
func WithRetry(p Provider, attempts int) Provider {
	return &retryProvider{Provider: p, attempts: attempts}
}

type retryProvider struct {
	Provider
	attempts int
}

func (r *retryProvider) ByISBN(ctx context.Context, isbn string) (Metadata, error) {
	return r.call(ctx, func() (Metadata, error) { return r.Provider.ByISBN(ctx, isbn) })
}

func (r *retryProvider) Search(ctx context.Context, title string, authors []string) (Metadata, error) {
	return r.call(ctx, func() (Metadata, error) { return r.Provider.Search(ctx, title, authors) })
}

func (r *retryProvider) call(ctx context.Context, fn func() (Metadata, error)) (Metadata, error) {
	attempts := r.attempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	delay := retryBaseDelay
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return Metadata{}, ctx.Err()
			}
			delay *= 2
			if delay > retryMaxDelay {
				delay = retryMaxDelay
			}
		}

		m, err := fn()
		if err == nil {
			return m, nil
		}
		if !errors.Is(err, ErrRetryable) {
			return Metadata{}, err
		}
		lastErr = err
	}
	return Metadata{}, lastErr
}
