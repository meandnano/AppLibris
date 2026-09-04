// Package providers is the compile-time registry mapping a configured
// provider name to its constructor, and the METADATA_PROVIDERS parsing
// that turns cmd/server's env var into the ordered enrich.Provider chain
// enrich.New expects. It lives outside internal/enrich rather than inside
// it (as the plan's own sketch first put it) because internal/openlibrary
// and internal/googlebooks both import internal/enrich for Provider and
// Metadata — a registry in that package importing them back would be an
// import cycle.
package providers

import (
	"fmt"
	"sort"
	"strings"

	"library/internal/enrich"
	"library/internal/googlebooks"
	"library/internal/openlibrary"
)

// constructors builds the name -> constructor map fresh each call rather
// than as a package-level var, since googlebooks.New takes an API key that
// is only known once cmd/server has read the environment.
func constructors(googleBooksAPIKey string) map[string]func() enrich.Provider {
	return map[string]func() enrich.Provider{
		"openlibrary": func() enrich.Provider { return compose(openlibrary.New()) },
		"googlebooks": func() enrich.Provider { return compose(googlebooks.New(googleBooksAPIKey)) },
	}
}

// compose wraps p in the three decorators in the order fixed at
// construction: cache innermost so a cached hit costs neither a
// rate-limit token nor a retry attempt, retry next so a retried attempt is
// still paced, rate limit outermost so retries are paced too.
func compose(p enrich.Provider) enrich.Provider {
	return enrich.WithRateLimit(
		enrich.WithRetry(
			enrich.WithCache(p, enrich.DefaultCacheSize),
			enrich.DefaultRetryAttempts,
		),
		enrich.DefaultRateLimitInterval,
	)
}

// Resolve turns an ordered list of configured provider names into the
// decorated Provider chain enrich.New expects. An unknown name fails
// outright, naming it and listing the valid ones, rather than silently
// running with fewer providers than configured — the difference DESIGN.md
// draws between "I asked for something specific and did not get it" and an
// unset RESEND_API_KEY, which only warns. An empty names list resolves to
// an empty, non-nil slice: enrichment disabled, not an error.
func Resolve(names []string, googleBooksAPIKey string) ([]enrich.Provider, error) {
	ctor := constructors(googleBooksAPIKey)

	result := make([]enrich.Provider, 0, len(names))
	for _, name := range names {
		c, ok := ctor[name]
		if !ok {
			return nil, fmt.Errorf("unknown metadata provider %q (valid: %s)", name, strings.Join(validNames(ctor), ", "))
		}
		result = append(result, c())
	}
	return result, nil
}

func validNames(ctor map[string]func() enrich.Provider) []string {
	names := make([]string, 0, len(ctor))
	for name := range ctor {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ParseNames splits a METADATA_PROVIDERS-shaped value ("openlibrary,
// googlebooks") into an ordered list of names, trimming whitespace around
// each and dropping empty entries — including when raw is empty or
// entirely blank, which parses to no names at all rather than one blank
// one, so an explicitly empty METADATA_PROVIDERS resolves to no providers
// rather than a resolution error.
func ParseNames(raw string) []string {
	var names []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}
