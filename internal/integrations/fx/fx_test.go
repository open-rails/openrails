package fx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	redis "github.com/redis/go-redis/v9"
)

// MONEY-3: exact rational conversion with one final ceiling, across ISO scales.
func TestConvertAmount(t *testing.T) {
	p := NewMockProvider(map[string]float64{"eur": 1.08, "jpy": 0.01})
	for _, tc := range []struct {
		from, to  string
		amt, want int64
	}{
		{"EUR", "USD", 100, 108},           // float 100*1.08 = 108.00000000000001 would ceil to 109
		{"USD", "EUR", 1_000_000, 925_926}, // 925925.92… rounds up, never under-counts
		{"USD", "EUR", 1, 1},
		{"USD", "JPY", 123_456, 123_456},
		{"JPY", "USD", 123, 123},
		{"JPY", "USD", 1, 1},
		{"usd", "USD", 42, 42},
		{"EUR", "USD", 0, 0},
	} {
		got, q, err := ConvertAmount(context.Background(), p, tc.from, tc.to, tc.amt)
		if err != nil || got != tc.want || q == nil {
			t.Errorf("%s->%s %d = %d, %v; want %d", tc.from, tc.to, tc.amt, got, err, tc.want)
		}
	}

	for name, call := range map[string]func() error{
		"negative":         func() error { _, _, err := ConvertAmount(context.Background(), p, "USD", "EUR", -1); return err },
		"unknown currency": func() error { _, _, err := ConvertAmount(context.Background(), p, "USD", "XXQ", 1); return err },
		"no provider":      func() error { _, _, err := ConvertAmount(context.Background(), nil, "USD", "EUR", 1); return err },
		"provider error": func() error {
			_, _, err := ConvertAmount(context.Background(), &MockProvider{Error: errors.New("down")}, "USD", "EUR", 1)
			return err
		},
		"zero rate": func() error {
			_, _, err := ConvertAmount(context.Background(), NewMockProvider(map[string]float64{"eur": 0}), "EUR", "USD", 1)
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if got, _, err := ConvertAmount(context.Background(), nil, "USD", "USD", 7); err != nil || got != 7 {
		t.Fatalf("same currency needs no provider: %d %v", got, err)
	}
}

func TestCachedProvider(t *testing.T) {
	mock := NewMockProvider(map[string]float64{"eur": 1.08})
	cached := NewCachedProvider(mock, time.Minute)
	ctx := context.Background()
	for range 2 {
		q, err := cached.QuoteToUSD(ctx, "eur")
		if err != nil || q.Rate != 1.08 || q.FromCurrency != "EUR" || mock.CallCount != 1 {
			t.Fatalf("quote %+v err %v calls %d", q, err, mock.CallCount)
		}
	}
	cached.InvalidateAll()
	if _, _ = cached.QuoteToUSD(ctx, "EUR"); mock.CallCount != 2 {
		t.Fatalf("invalidate must refetch, calls %d", mock.CallCount)
	}
	if _, err := cached.QuoteToUSD(ctx, "gbp"); err == nil {
		t.Fatal("provider error must not be cached as a quote")
	}
	expiring := NewCachedProvider(mock, 0)
	_, _ = expiring.QuoteToUSD(ctx, "eur")
	_, _ = expiring.QuoteToUSD(ctx, "eur")
	if mock.CallCount != 5 {
		t.Fatalf("zero ttl must never serve cache, calls %d", mock.CallCount)
	}
}

// CUR-6 wire exception: the endpoint is lower case in path and keys; quotes stay upper.
func TestExchangeAPIWire(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"date":"2026-09-01","eur":{"usd":1.08,"gbp":0.85}}`))
	}))
	defer srv.Close()
	p := &ExchangeAPIProvider{client: srv.Client(), baseURL: srv.URL}
	q, err := p.QuoteToUSD(context.Background(), "EUR")
	if err != nil || path != "/eur.json" || q.Rate != 1.08 || q.FromCurrency != "EUR" || q.ToCurrency != "USD" ||
		!q.AsOf.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("quote %+v path %q err %v", q, path, err)
	}
	if q, err := p.Quote(context.Background(), "usd", "USD"); err != nil || q.Rate != 1 {
		t.Fatalf("same currency: %+v %v", q, err)
	}
}

// RedisCachedProvider.Start suppresses shutdown noise via errors.Is(err, context.Canceled).
func TestExchangeAPICanceledContextIsDetectable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewExchangeAPIProvider().Quote(ctx, "eur", "usd"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled through the wrap chain, got %v", err)
	}
}

// The Redis cache fails closed when no fresh rate exists anywhere rather than
// inventing one.
func TestRedisCachedProviderFailsClosedWithoutARate(t *testing.T) {
	p := NewRedisCachedProvider(nil, NewMockProvider(nil), 0)
	if _, err := p.Quote(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("an unquotable pair must refuse")
	}
	if q, err := p.Quote(context.Background(), "usd", "USD"); err != nil || q.Rate != 1 {
		t.Fatalf("same currency: %+v %v", q, err)
	}
}

type flakyProvider struct {
	calls atomic.Int64
	down  atomic.Bool
	fail  atomic.Int64 // calls to fail before answering
}

func (f *flakyProvider) Quote(_ context.Context, from, to string) (*Quote, error) {
	n := f.calls.Add(1)
	if f.down.Load() || n <= f.fail.Load() {
		return nil, errors.New("upstream down")
	}
	return &Quote{FromCurrency: from, ToCurrency: to, Rate: 1.25, AsOf: time.Now()}, nil
}

func (f *flakyProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return f.Quote(ctx, currency, "USD")
}

// Redis is an accelerator: while it is unreachable, quotes come from the
// in-memory rates and the refresher keeps retrying the publish.
func TestRedisCachedProviderServesMemoryWhileRedisIsDown(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	upstream := &flakyProvider{}
	p := NewRedisCachedProvider(rdb, upstream, time.Hour)
	ctx := context.Background()

	if err := p.Refresh(ctx, []string{"EUR", "USD"}); err == nil {
		t.Fatal("the redis publish must report the outage")
	}
	fetched := upstream.calls.Load()
	upstream.down.Store(true)
	q, err := p.Quote(ctx, "EUR", "USD")
	if err != nil || q.Rate != 1.25 {
		t.Fatalf("quote from memory: %+v %v", q, err)
	}
	if upstream.calls.Load() != fetched {
		t.Fatal("a fresh in-memory rate needs no upstream call")
	}
	if p.LastRefresh().IsZero() {
		t.Fatal("a complete fetch is a successful refresh even when redis is down")
	}
}

// A failed refresh is retried with backoff, not after the next 2h interval.
func TestRedisCachedProviderRetriesFailedRefresh(t *testing.T) {
	upstream := &flakyProvider{}
	upstream.fail.Store(1)
	p := NewRedisCachedProvider(nil, upstream, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx, []string{"EUR", "USD"}, 2*time.Hour)
	t.Cleanup(p.Stop)
	deadline := time.Now().Add(10 * time.Second)
	for p.LastRefresh().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("the failed refresh was not retried")
		}
		time.Sleep(20 * time.Millisecond)
	}
	upstream.down.Store(true)
	if q, err := p.Quote(ctx, "USD", "EUR"); err != nil || q.Rate != 1.25 {
		t.Fatalf("retried pair: %+v %v", q, err)
	}
}

type scriptedProvider struct {
	calls atomic.Int64
	gate  chan struct{}
	asOf  time.Time
	err   error
}

func (s *scriptedProvider) Quote(_ context.Context, from, to string) (*Quote, error) {
	s.calls.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	if s.err != nil {
		return nil, s.err
	}
	asOf := s.asOf
	if asOf.IsZero() {
		asOf = time.Now()
	}
	return &Quote{FromCurrency: from, ToCurrency: to, Rate: 1.25, AsOf: asOf}, nil
}

func (s *scriptedProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return s.Quote(ctx, currency, "USD")
}

// A lagging upstream file is not a fresh rate, however recently it was fetched.
func TestRedisCachedProviderRejectsStaleUpstreamRates(t *testing.T) {
	p := NewRedisCachedProvider(nil, &scriptedProvider{asOf: time.Now().Add(-5 * 24 * time.Hour)}, time.Hour)
	if _, err := p.Quote(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("a rate published days ago must not quote")
	}
	if err := p.Refresh(context.Background(), []string{"EUR", "USD"}); err == nil {
		t.Fatal("refresh must report a stale upstream")
	}
}

// Concurrent misses for one pair make one upstream call.
func TestRedisCachedProviderSingleFlightsInlineFetch(t *testing.T) {
	upstream := &scriptedProvider{gate: make(chan struct{})}
	p := NewRedisCachedProvider(nil, upstream, time.Hour)
	errs := make(chan error, 8)
	for range 8 {
		go func() { _, err := p.Quote(context.Background(), "EUR", "USD"); errs <- err }()
	}
	time.Sleep(100 * time.Millisecond)
	close(upstream.gate)
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := upstream.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

// An upstream failure is remembered briefly, so a burst of quotes does not
// hammer a provider that just failed.
func TestRedisCachedProviderNegativeCachesUpstreamFailure(t *testing.T) {
	upstream := &scriptedProvider{err: errors.New("upstream down")}
	p := NewRedisCachedProvider(nil, upstream, time.Hour)
	p.flights.negativeTTL = 200 * time.Millisecond
	for range 3 {
		if _, err := p.Quote(context.Background(), "EUR", "USD"); err == nil {
			t.Fatal("want failure")
		}
	}
	if n := upstream.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 inside the negative window", n)
	}
	time.Sleep(250 * time.Millisecond)
	upstream.err = nil
	if _, err := p.Quote(context.Background(), "EUR", "USD"); err != nil {
		t.Fatal(err)
	}
	if n := upstream.calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 after the window", n)
	}
}

// The in-memory cache (no Redis) holds the same line as the Redis cache: a
// lagging upstream file is not fresh, concurrent misses make one call, and a
// failure is remembered briefly.
func TestCachedProviderRejectsStaleUpstreamRates(t *testing.T) {
	p := NewCachedProvider(&scriptedProvider{asOf: time.Now().Add(-5 * 24 * time.Hour)}, time.Hour)
	if _, err := p.Quote(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("a rate published days ago must not quote")
	}
}

func TestCachedProviderSingleFlightsFetch(t *testing.T) {
	upstream := &scriptedProvider{gate: make(chan struct{})}
	p := NewCachedProvider(upstream, time.Hour)
	errs := make(chan error, 8)
	for range 8 {
		go func() { _, err := p.Quote(context.Background(), "EUR", "USD"); errs <- err }()
	}
	time.Sleep(100 * time.Millisecond)
	close(upstream.gate)
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := upstream.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

func TestCachedProviderNegativeCachesFailure(t *testing.T) {
	upstream := &scriptedProvider{err: errors.New("upstream down")}
	p := NewCachedProvider(upstream, time.Hour)
	p.flights.negativeTTL = 200 * time.Millisecond
	for range 3 {
		if _, err := p.Quote(context.Background(), "EUR", "USD"); err == nil {
			t.Fatal("want failure")
		}
	}
	if n := upstream.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 inside the negative window", n)
	}
}

// A cached rate ages out by its publication date at use time, not only when it
// was fetched: both caches refetch rather than serve a rate now too old.
func TestCachedRatesAgeAtUseTime(t *testing.T) {
	published := time.Now()
	for name, build := range map[string]func(Provider) (Provider, *func() time.Time){
		"memory": func(up Provider) (Provider, *func() time.Time) {
			p := NewCachedProvider(up, 1000*time.Hour)
			return p, &p.now
		},
		"redis": func(up Provider) (Provider, *func() time.Time) {
			p := NewRedisCachedProvider(nil, up, 1000*time.Hour)
			return p, &p.now
		},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &scriptedProvider{asOf: published}
			p, now := build(upstream)
			if _, err := p.Quote(context.Background(), "EUR", "USD"); err != nil {
				t.Fatal(err)
			}
			*now = func() time.Time { return published.Add(49 * time.Hour) }
			if _, err := p.Quote(context.Background(), "EUR", "USD"); err == nil {
				t.Fatal("a rate published 49h ago must not be served from cache")
			}
			if n := upstream.calls.Load(); n != 2 {
				t.Fatalf("upstream calls = %d, want a refetch", n)
			}
		})
	}
}

// An FX file without a publication date is not fresh: it carries no AsOf
// (never "now"), so every cache refuses it.
func TestExchangeAPIUndatedRateIsStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eur":{"usd":1.08}}`))
	}))
	defer srv.Close()
	p := &ExchangeAPIProvider{client: srv.Client(), baseURL: srv.URL}
	q, err := p.QuoteToUSD(context.Background(), "EUR")
	if err != nil || !q.AsOf.IsZero() {
		t.Fatalf("undated quote must carry no AsOf: %+v %v", q, err)
	}
	if _, err := NewCachedProvider(p, time.Hour).Quote(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("an undated rate must not quote")
	}
}
