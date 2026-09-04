package enrich

import (
	"container/list"
	"context"
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
// slot opens.
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

// rateLimiter hands out one token every `every`, via a ticker feeding a
// capacity-1 channel: a token sitting unused between calls (the ordinary
// case, since enrichment jobs are not a tight loop) is why callers wait on
// the channel rather than the ticker directly — the ticker only ever tops
// the channel back up to one. The first token is pre-loaded so the very
// first call never pays a full interval of latency for no reason. It runs
// for the life of the process, same as the Worker it paces: nothing ever
// stops the ticker, matching every other background ticker in this
// package (e.g. Worker.Run's own pollInterval).
type rateLimiter struct {
	tokens chan struct{}
}

func newRateLimiter(every time.Duration) *rateLimiter {
	rl := &rateLimiter{tokens: make(chan struct{}, 1)}
	rl.tokens <- struct{}{}

	ticker := time.NewTicker(every)
	go func() {
		for range ticker.C {
			select {
			case rl.tokens <- struct{}{}:
			default:
			}
		}
	}()
	return rl
}

func (rl *rateLimiter) wait(ctx context.Context) error {
	select {
	case <-rl.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
// negative answer forever. An error is never cached — DESIGN.md's
// four-case contract treats an error as a transient, retryable condition,
// not an answer worth remembering.
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
	c.cache.put(key, m)
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
	c.cache.put(key, m)
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

// WithRetry retries p's call when it returns an error — the 429/5xx/
// transport case DESIGN.md's four-case contract draws a hard line
// around — up to attempts total tries, with a backoff and a ctx check
// between them. A "no match" (zero Metadata, nil error) is never
// retried: it is not a failure, it is an answer.
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
		lastErr = err
	}
	return Metadata{}, lastErr
}
