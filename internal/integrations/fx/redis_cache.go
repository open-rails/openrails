package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/retry"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

const RedisRateProviderName = "exchange-api"

type redisRate struct {
	FromCurrency string    `json:"from_currency"`
	ToCurrency   string    `json:"to_currency"`
	Rate         float64   `json:"rate"`
	Provider     string    `json:"provider"`
	AsOf         time.Time `json:"as_of"`
	FetchedAt    time.Time `json:"fetched_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// RedisCachedProvider shares fresh rates across replicas through Redis and
// keeps its own copy in memory. Quote reads Redis and falls back to memory
// (then the upstream provider) when Redis errors or lacks the pair, so a Redis
// outage never breaks quoting. It fails closed when no fresh rate exists:
// admission never silently defaults to USD.
type RedisCachedProvider struct {
	rdb      redis.Cmdable
	provider Provider
	ttl      time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	last    time.Time
	memory  map[string]redisRate
	now     func() time.Time
	flights *flights
}

func NewRedisCachedProvider(rdb redis.Cmdable, provider Provider, ttl time.Duration) *RedisCachedProvider {
	if provider == nil {
		panic("fx provider is required")
	}
	if ttl <= 0 {
		ttl = 3 * time.Hour
	}
	return &RedisCachedProvider{rdb: rdb, provider: provider, ttl: ttl, memory: map[string]redisRate{}, now: time.Now, flights: newFlights()}
}

func (p *RedisCachedProvider) Quote(ctx context.Context, fromCurrency, toCurrency string) (*Quote, error) {
	fromCurrency = normalizeCurrency(fromCurrency)
	toCurrency = normalizeCurrency(toCurrency)
	if fromCurrency == "" || toCurrency == "" {
		return nil, fmt.Errorf("from_currency and to_currency are required")
	}
	if fromCurrency == toCurrency {
		return &Quote{FromCurrency: fromCurrency, ToCurrency: toCurrency, Rate: 1, AsOf: time.Now()}, nil
	}
	if p.rdb != nil {
		rate, err := p.redisRate(ctx, fromCurrency, toCurrency)
		if err == nil {
			return rate.quote(), nil
		}
		if !errors.Is(err, redis.Nil) {
			log.WithError(err).Debug("fx: redis rate unavailable; using the in-memory rate")
		}
	}
	if rate, ok := p.memoryRate(fromCurrency, toCurrency); ok {
		return rate.quote(), nil
	}
	quote, err := p.flights.do(ctx, redisRateKey(fromCurrency, toCurrency), func(ctx context.Context) (*Quote, error) {
		rate, err := p.fetch(ctx, fromCurrency, toCurrency, p.now().UTC())
		if err != nil {
			return nil, err
		}
		return rate.quote(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("FX rate unavailable for %s -> %s: %w", fromCurrency, toCurrency, err)
	}
	return quote, nil
}

func (p *RedisCachedProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return p.Quote(ctx, currency, money.DefaultCurrency)
}

func (r redisRate) quote() *Quote {
	return &Quote{FromCurrency: r.FromCurrency, ToCurrency: r.ToCurrency, Rate: r.Rate, AsOf: r.AsOf}
}

func (r redisRate) fresh(from, to string, now time.Time) bool {
	return r.Rate > 0 && strings.EqualFold(r.FromCurrency, from) && strings.EqualFold(r.ToCurrency, to) && now.Before(r.ExpiresAt) && !staleRate(r.AsOf, now)
}

// redisRate returns the fresh shared rate; redis.Nil when it is missing or stale.
func (p *RedisCachedProvider) redisRate(ctx context.Context, from, to string) (redisRate, error) {
	raw, err := p.rdb.Get(ctx, redisRateKey(from, to)).Bytes()
	if err != nil {
		return redisRate{}, err
	}
	var rate redisRate
	if err := json.Unmarshal(raw, &rate); err != nil {
		return redisRate{}, fmt.Errorf("decode FX rate: %w", err)
	}
	if !rate.fresh(from, to, p.now().UTC()) {
		return redisRate{}, redis.Nil
	}
	return rate, nil
}

func (p *RedisCachedProvider) memoryRate(from, to string) (redisRate, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rate, ok := p.memory[redisRateKey(from, to)]
	return rate, ok && rate.fresh(from, to, p.now().UTC())
}

// fetch reads one pair from the upstream provider into memory.
func (p *RedisCachedProvider) fetch(ctx context.Context, from, to string, now time.Time) (redisRate, error) {
	q, err := p.provider.Quote(ctx, from, to)
	if err != nil {
		return redisRate{}, err
	}
	if staleRate(q.AsOf, now) {
		return redisRate{}, fmt.Errorf("upstream rate %s -> %s is stale (as of %s)", from, to, q.AsOf.Format(time.RFC3339))
	}
	rate := redisRate{
		FromCurrency: normalizeCurrency(from),
		ToCurrency:   normalizeCurrency(to),
		Rate:         q.Rate,
		Provider:     RedisRateProviderName,
		AsOf:         q.AsOf.UTC(),
		FetchedAt:    now,
		ExpiresAt:    now.Add(p.ttl),
	}
	p.mu.Lock()
	p.memory[redisRateKey(from, to)] = rate
	p.mu.Unlock()
	p.flights.forget(redisRateKey(from, to))
	return rate, nil
}

// Refresh fetches every pair into memory and publishes them to Redis.
func (p *RedisCachedProvider) Refresh(ctx context.Context, currencies []string) error {
	fetchErr := p.refresh(ctx, currencies, time.Time{})
	return errors.Join(fetchErr, p.publish(ctx))
}

// refresh fetches the pairs not already fetched since since.
func (p *RedisCachedProvider) refresh(ctx context.Context, currencies []string, since time.Time) error {
	currencies = uniqueCurrencies(currencies)
	var errs []error
	now := p.now().UTC()
	for _, from := range currencies {
		for _, to := range currencies {
			if from == to {
				continue
			}
			p.mu.Lock()
			current, ok := p.memory[redisRateKey(from, to)]
			p.mu.Unlock()
			if ok && !since.IsZero() && !current.FetchedAt.Before(since) {
				continue
			}
			if _, err := p.fetch(ctx, from, to, now); err != nil {
				errs = append(errs, fmt.Errorf("%s -> %s: %w", from, to, err))
			}
		}
	}
	if len(errs) == 0 {
		p.mu.Lock()
		p.last = now
		p.mu.Unlock()
	}
	return errors.Join(errs...)
}

// publish writes the fresh in-memory rates to Redis for the other replicas.
func (p *RedisCachedProvider) publish(ctx context.Context) error {
	if p.rdb == nil {
		return nil
	}
	p.mu.Lock()
	rates := make([]redisRate, 0, len(p.memory))
	for _, rate := range p.memory {
		rates = append(rates, rate)
	}
	p.mu.Unlock()
	now := p.now().UTC()
	for _, rate := range rates {
		ttl := rate.ExpiresAt.Sub(now)
		if ttl <= 0 {
			continue
		}
		payload, err := json.Marshal(rate)
		if err != nil {
			return err
		}
		if err := p.rdb.Set(ctx, redisRateKey(rate.FromCurrency, rate.ToCurrency), payload, ttl).Err(); err != nil {
			return fmt.Errorf("publish FX rates to redis: %w", err)
		}
	}
	return nil
}

// Start refreshes every interval. A failed cycle is retried with capped
// full-jitter backoff until it succeeds: only the pairs still missing are
// refetched, and a Redis failure only re-publishes.
func (p *RedisCachedProvider) Start(ctx context.Context, currencies []string, interval time.Duration) {
	if p == nil || interval <= 0 {
		return
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()

	go func() {
		for {
			p.cycle(ctx, currencies)
			if !retry.Sleep(ctx, interval+jitter(interval/20)) {
				return
			}
		}
	}()
}

func (p *RedisCachedProvider) cycle(ctx context.Context, currencies []string) {
	since := time.Now().UTC()
	fetchErr := p.refresh(ctx, currencies, time.Time{})
	for attempt := 0; ; attempt++ {
		publishErr := p.publish(ctx)
		err := errors.Join(fetchErr, publishErr)
		if err == nil || ctx.Err() != nil {
			return
		}
		if attempt == 0 {
			log.WithError(err).Warn("fx: refresh failed; retrying with backoff")
		}
		if !retry.Sleep(ctx, retry.Backoff(attempt, time.Second, retry.Max)) {
			return
		}
		if fetchErr != nil {
			fetchErr = p.refresh(ctx, currencies, since)
		}
	}
}

func (p *RedisCachedProvider) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

func (p *RedisCachedProvider) LastRefresh() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func redisRateKey(from, to string) string {
	return "fx:rate:" + normalizeCurrency(from) + ":" + normalizeCurrency(to)
}

func uniqueCurrencies(currencies []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(currencies))
	for _, c := range currencies {
		c = normalizeCurrency(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(time.Now().UnixNano() % int64(max))
}
