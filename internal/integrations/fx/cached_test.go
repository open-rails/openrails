package fx

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestCachedProvidersReturnIndependentQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(Provider) Provider
	}{
		{"memory", func(p Provider) Provider { return NewCachedProvider(p, time.Hour) }},
		{"redis fallback", func(p Provider) Provider { return NewRedisCachedProvider(nil, p, time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				upstream := &scriptedProvider{gate: make(chan struct{})}
				p := tc.new(upstream)
				type result struct {
					quote *Quote
					err   error
				}
				results := make(chan result, 2)
				for range 2 {
					go func() {
						q, err := p.Quote(context.Background(), "EUR", "USD")
						results <- result{q, err}
					}()
				}
				synctest.Wait()
				close(upstream.gate)
				var quotes [2]*Quote
				for i := range quotes {
					r := <-results
					if r.err != nil || r.quote == nil {
						t.Fatalf("quote: %v", r.err)
					}
					quotes[i] = r.quote
				}
				want := *quotes[0]
				*quotes[0] = Quote{}
				if *quotes[1] != want {
					t.Fatal("mutating one miss changed another caller's quote")
				}
				amount, hit, err := ConvertAmount(t.Context(), p, "EUR", "USD", 4)
				if err != nil || amount != 5 || hit == nil || *hit != want {
					t.Fatalf("conversion after mutation: %d, %+v, %v", amount, hit, err)
				}
				*hit = Quote{}
				q, err := p.QuoteToUSD(t.Context(), "EUR")
				if err != nil || q == nil || *q != want || upstream.calls.Load() != 1 {
					t.Fatalf("quote after mutating hit: %+v, %v; calls %d", q, err, upstream.calls.Load())
				}
			})
		})
	}
}
