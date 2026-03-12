package fx

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CachedProvider wraps another Provider with in-memory caching.
type CachedProvider struct {
	provider Provider
	ttl      time.Duration

	mu    sync.RWMutex
	cache map[string]*cachedQuote
}

type cachedQuote struct {
	quote     *Quote
	expiresAt time.Time
}

// NewCachedProvider creates a CachedProvider with the given TTL.
// A TTL of 5 minutes is recommended to balance freshness with API rate limits.
func NewCachedProvider(provider Provider, ttl time.Duration) *CachedProvider {
	if provider == nil {
		panic("fx provider is required")
	}
	return &CachedProvider{
		provider: provider,
		ttl:      ttl,
		cache:    make(map[string]*cachedQuote),
	}
}

// QuoteToUSD returns a cached quote if available and not expired, otherwise fetches a new one.
func (p *CachedProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	currency = normalizeCurrency(currency)
	if currency == "" {
		return nil, fmt.Errorf("currency is required")
	}
	// Check cache first
	p.mu.RLock()
	if cached, ok := p.cache[currency]; ok && time.Now().Before(cached.expiresAt) {
		p.mu.RUnlock()
		// Return a copy to prevent mutation
		return &Quote{
			FromCurrency: cached.quote.FromCurrency,
			ToCurrency:   cached.quote.ToCurrency,
			Rate:         cached.quote.Rate,
			AsOf:         cached.quote.AsOf,
		}, nil
	}
	p.mu.RUnlock()

	// Fetch fresh quote
	quote, err := p.provider.QuoteToUSD(ctx, currency)
	if err != nil {
		return nil, err
	}

	// Cache the result
	p.mu.Lock()
	p.cache[currency] = &cachedQuote{
		quote:     quote,
		expiresAt: time.Now().Add(p.ttl),
	}
	p.mu.Unlock()

	return quote, nil
}

// InvalidateAll clears the entire cache.
func (p *CachedProvider) InvalidateAll() {
	p.mu.Lock()
	p.cache = make(map[string]*cachedQuote)
	p.mu.Unlock()
}
