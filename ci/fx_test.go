//go:build e2e && integration

package ci_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const fxPrimary, fxFallback = "latest.currency-api.pages.dev", "cdn.jsdelivr.net"

// fakeFX is the exchange-api: one file per base currency listing its rate to
// every registry currency. It counts each request by host and base.
type fakeFX struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   map[string]int // host/base
	down   map[string]bool
	dates  map[string]string // base -> publication date, default today
}

func newFakeFX(t *testing.T) *fakeFX {
	fx := &fakeFX{seen: map[string]int{}, down: map[string]bool{}, dates: map[string]string{}}
	codes := moneyutil.CurrencyCodes()
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-FX-Host")
		base := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
		fx.mu.Lock()
		fx.seen[host+"/"+base]++
		down, date := fx.down[host+"/"+base], fx.dates[base]
		fx.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if date == "" {
			date = time.Now().UTC().Format(time.DateOnly)
		}
		rates := make([]string, 0, len(codes))
		for i, code := range codes {
			if c := strings.ToLower(code); c != base {
				rates = append(rates, fmt.Sprintf("%q:%d.5", c, i+1))
			}
		}
		_, _ = fmt.Fprintf(w, `{"date":%q,%q:{%s}}`, date, base, strings.Join(rates, ","))
	}))
	t.Cleanup(fx.server.Close)
	return fx
}

// transport sends the exchange-api's hosts to the fake, naming the host asked.
func (fx *fakeFX) transport() http.RoundTripper {
	target, _ := url.Parse(fx.server.URL)
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.Header.Set("X-FX-Host", r.URL.Host)
		out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, ""
		return http.DefaultTransport.RoundTrip(out)
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// requests is how many files host served; base narrows it to one currency.
func (fx *fakeFX) requests(host, base string) int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	n := 0
	for key, count := range fx.seen {
		h, b, _ := strings.Cut(key, "/")
		if h == host && (base == "" || b == base) {
			n += count
		}
	}
	return n
}

func (f *fixture) fxEngine(t *testing.T, fx *fakeFX, start bool) *openrails.Client {
	t.Helper()
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool, FXTransport: fx.transport()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	if start {
		require.NoError(t, client.Start(t.Context()))
	}
	return client
}

// One refresh serves the fleet: two replicas running workers read each
// currency's file once (34 requests, not one per pair), and both quote every
// pair from PostgreSQL. A failing primary is read from the fallback.
func TestFXRefreshOncePerFleet(t *testing.T) {
	fx, f := newFakeFX(t), newFixture(t)
	a, b := f.fxEngine(t, fx, true), f.fxEngine(t, fx, true)
	codes := moneyutil.CurrencyCodes()
	require.Eventually(t, func() bool { return fx.requests(fxPrimary, "") >= len(codes) }, 60*time.Second, 50*time.Millisecond, "the refresh ran")
	require.Never(t, func() bool { return fx.requests(fxPrimary, "") > len(codes) }, 3*time.Second, 100*time.Millisecond, "once for the fleet")
	for _, code := range codes {
		require.Equal(t, 1, fx.requests(fxPrimary, strings.ToLower(code)), code)
	}

	for _, client := range []*openrails.Client{a, b} {
		rates := engine.Graph(client).Runtime.FXProvider
		for _, from := range codes {
			for _, to := range codes {
				q, err := rates.Quote(t.Context(), from, to)
				require.NoError(t, err, "%s -> %s", from, to)
				require.Positive(t, q.Rate)
			}
		}
	}
	require.Equal(t, len(codes), fx.requests(fxPrimary, ""), "every quote read the table, none the source")
	require.Zero(t, fx.requests(fxFallback, ""))

	fx.mu.Lock()
	fx.down[fxPrimary+"/eur"] = true
	fx.mu.Unlock()
	require.NoError(t, engine.Graph(a).Runtime.FXRates.Refresh(t.Context()))
	require.Equal(t, 2*len(codes), fx.requests(fxPrimary, ""), "a refresh is one request per currency")
	require.Equal(t, 1, fx.requests(fxFallback, "eur"), "the failing one read from the fallback")
	require.Equal(t, 1, fx.requests(fxFallback, ""))

	fx.mu.Lock()
	fx.down[fxFallback+"/eur"] = true
	fx.mu.Unlock()
	err := engine.Graph(a).Runtime.FXRates.Refresh(t.Context())
	require.ErrorContains(t, err, fmt.Sprintf("1 of %d FX base currencies failed; first FX rates for EUR: ", len(codes)), "one line, however many fail")
	require.NotContains(t, err.Error(), "\n")
}

// With no refresh running, a quote reads its base currency's file itself,
// once however many quote at the same time, and stores every rate in it for
// the other replicas. A stale file is refused, and a failing one is not asked
// again within the negative window.
func TestFXQuoteReadsItsBaseOnce(t *testing.T) {
	fx, f := newFakeFX(t), newFixture(t)
	a, b := f.fxEngine(t, fx, false), f.fxEngine(t, fx, false)
	ra, rb := engine.Graph(a).Runtime.FXProvider, engine.Graph(b).Runtime.FXProvider

	var wg sync.WaitGroup
	targets := []string{"USD", "GBP", "JPY", "CHF", "USD", "GBP", "JPY", "CHF"}
	errs := make(chan error, len(targets))
	for _, to := range targets {
		wg.Go(func() {
			_, err := ra.Quote(t.Context(), "EUR", to)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, fx.requests(fxPrimary, ""), "one read of EUR's file")
	_, err := rb.Quote(t.Context(), "EUR", "CAD")
	require.NoError(t, err)
	require.Equal(t, 1, fx.requests(fxPrimary, ""), "the other replica quotes the stored rates")

	fx.mu.Lock()
	fx.dates["gbp"] = time.Now().UTC().Add(-5 * 24 * time.Hour).Format(time.DateOnly)
	fx.down[fxPrimary+"/jpy"], fx.down[fxFallback+"/jpy"] = true, true
	fx.mu.Unlock()
	_, err = ra.Quote(t.Context(), "GBP", "USD")
	require.ErrorContains(t, err, "stale", "a file published days ago is no rate")
	for range 3 {
		_, err = ra.Quote(t.Context(), "JPY", "USD")
		require.Error(t, err)
	}
	require.Equal(t, 1, fx.requests(fxPrimary, "jpy"), "a failure is remembered")
	require.Equal(t, 1, fx.requests(fxFallback, "jpy"))
}
