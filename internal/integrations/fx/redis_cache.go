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

// RedisCachedProvider serves only fresh rates from Redis. Refresh fetches from
// the upstream provider; Quote fails closed on missing/stale cross-currency
// rates so admission does not silently default to USD.
type RedisCachedProvider struct {
	rdb      redis.Cmdable
	provider Provider
	ttl      time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	last   time.Time
}

func NewRedisCachedProvider(rdb redis.Cmdable, provider Provider, ttl time.Duration) *RedisCachedProvider {
	if provider == nil {
		panic("fx provider is required")
	}
	if ttl <= 0 {
		ttl = 3 * time.Hour
	}
	return &RedisCachedProvider{rdb: rdb, provider: provider, ttl: ttl}
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
	if p == nil || p.rdb == nil {
		return nil, fmt.Errorf("FX rate cache unavailable")
	}
	raw, err := p.rdb.Get(ctx, redisRateKey(fromCurrency, toCurrency)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("FX rate unavailable for %s -> %s", fromCurrency, toCurrency)
	}
	if err != nil {
		return nil, err
	}
	var rate redisRate
	if err := json.Unmarshal(raw, &rate); err != nil {
		return nil, fmt.Errorf("decode FX rate: %w", err)
	}
	now := time.Now().UTC()
	if rate.Rate <= 0 || !strings.EqualFold(rate.FromCurrency, fromCurrency) || !strings.EqualFold(rate.ToCurrency, toCurrency) || !now.Before(rate.ExpiresAt) {
		return nil, fmt.Errorf("stale FX rate for %s -> %s", fromCurrency, toCurrency)
	}
	return &Quote{FromCurrency: fromCurrency, ToCurrency: toCurrency, Rate: rate.Rate, AsOf: rate.AsOf}, nil
}

func (p *RedisCachedProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return p.Quote(ctx, currency, money.DefaultCurrency)
}

func (p *RedisCachedProvider) Refresh(ctx context.Context, currencies []string) error {
	if p == nil || p.rdb == nil {
		return fmt.Errorf("FX rate cache unavailable")
	}
	currencies = uniqueCurrencies(currencies)
	var errs []error
	now := time.Now().UTC()
	for _, from := range currencies {
		for _, to := range currencies {
			if from == to {
				continue
			}
			q, err := p.provider.Quote(ctx, from, to)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s -> %s: %w", from, to, err))
				continue
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
			payload, err := json.Marshal(rate)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if err := p.rdb.Set(ctx, redisRateKey(from, to), payload, p.ttl).Err(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	p.mu.Lock()
	p.last = now
	p.mu.Unlock()
	return errors.Join(errs...)
}

func (p *RedisCachedProvider) Start(ctx context.Context, currencies []string, interval time.Duration) {
	if p == nil || p.rdb == nil || interval <= 0 {
		return
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()

	go func() {
		if err := p.Refresh(ctx, currencies); err != nil && !errors.Is(err, context.Canceled) {
			log.WithError(err).Warn("fx: refresh failed")
		}
		ticker := time.NewTicker(interval + jitter(interval/20))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.Refresh(ctx, currencies); err != nil && !errors.Is(err, context.Canceled) {
					log.WithError(err).Warn("fx: refresh failed")
				}
			}
		}
	}()
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
