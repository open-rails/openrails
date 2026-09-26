package fx

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
)

// CachedProvider wraps another Provider with in-memory caching.
type CachedProvider struct {
	provider Provider
	ttl      time.Duration
	now      func() time.Time
	flights  *flights

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
		now:      time.Now,
		flights:  newFlights(),
		cache:    make(map[string]*cachedQuote),
	}
}

// Quote returns a cached quote if available and not expired, otherwise fetches a new one.
func (p *CachedProvider) Quote(ctx context.Context, fromCurrency, toCurrency string) (*Quote, error) {
	fromCurrency = normalizeCurrency(fromCurrency)
	toCurrency = normalizeCurrency(toCurrency)
	if fromCurrency == "" || toCurrency == "" {
		return nil, fmt.Errorf("from_currency and to_currency are required")
	}
	key := fromCurrency + ":" + toCurrency
	p.mu.RLock()
	cached, ok := p.cache[key]
	p.mu.RUnlock()
	if ok && p.now().Before(cached.expiresAt) && !staleRate(cached.quote.AsOf, p.now()) {
		return &Quote{
			FromCurrency: cached.quote.FromCurrency,
			ToCurrency:   cached.quote.ToCurrency,
			Rate:         cached.quote.Rate,
			AsOf:         cached.quote.AsOf,
		}, nil
	}
	return p.flights.do(ctx, key, func(ctx context.Context) (*Quote, error) {
		quote, err := p.provider.Quote(ctx, fromCurrency, toCurrency)
		if err != nil {
			return nil, err
		}
		if staleRate(quote.AsOf, p.now()) {
			return nil, fmt.Errorf("upstream rate %s -> %s is stale (as of %s)", fromCurrency, toCurrency, quote.AsOf.Format(time.RFC3339))
		}
		p.mu.Lock()
		p.cache[key] = &cachedQuote{quote: quote, expiresAt: p.now().Add(p.ttl)}
		p.mu.Unlock()
		return quote, nil
	})
}

// QuoteToUSD returns a cached quote to USD.
func (p *CachedProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return p.Quote(ctx, currency, money.DefaultCurrency)
}

// InvalidateAll clears the entire cache.
func (p *CachedProvider) InvalidateAll() {
	p.mu.Lock()
	p.cache = make(map[string]*cachedQuote)
	p.mu.Unlock()
}
