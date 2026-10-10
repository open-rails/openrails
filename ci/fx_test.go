//go:build e2e && integration

package ci_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/fxfake"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func (f *fixture) fxEngine(t *testing.T, fx *fxfake.Server, start bool) *openrails.Client {
	t.Helper()
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool, FXTransport: fx.Transport()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	if start {
		require.NoError(t, client.Start(t.Context()))
	}
	return client
}

// One refresh serves the fleet: both replicas schedule the period's refresh,
// which runs once and reads each currency's file once (34 requests, not one
// per pair), and both quote every pair from PostgreSQL. A failing primary is
// read from the fallback.
func TestFXRefreshOncePerFleet(t *testing.T) {
	fx, f := fxfake.New(), newFixture(t)
	t.Cleanup(fx.Close)
	a, b := f.fxEngine(t, fx, true), f.fxEngine(t, fx, true)
	codes := moneyutil.CurrencyCodes()
	for _, replica := range []*openrails.Client{a, b} {
		_, err := engine.Graph(replica).Runtime.RiverClient.Insert(t.Context(), riverjobs.FXRefreshArgs{}, riverjobs.FXRefreshInsertOpts())
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return fx.Requests(fxfake.Primary, "") >= len(codes) }, 60*time.Second, 50*time.Millisecond, "the refresh ran")
	require.Never(t, func() bool { return fx.Requests(fxfake.Primary, "") > len(codes) }, 3*time.Second, 100*time.Millisecond, "once for the fleet")
	for _, code := range codes {
		require.Equal(t, 1, fx.Requests(fxfake.Primary, strings.ToLower(code)), code)
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
	require.Equal(t, len(codes), fx.Requests(fxfake.Primary, ""), "every quote read the table, none the source")
	require.Zero(t, fx.Requests(fxfake.Fallback, ""))

	fx.Fail(fxfake.Primary, "eur")
	require.NoError(t, engine.Graph(a).Runtime.FXRates.Refresh(t.Context()))
	require.Equal(t, 2*len(codes), fx.Requests(fxfake.Primary, ""), "a refresh is one request per currency")
	require.Equal(t, 1, fx.Requests(fxfake.Fallback, "eur"), "the failing one read from the fallback")
	require.Equal(t, 1, fx.Requests(fxfake.Fallback, ""))

	fx.Fail(fxfake.Fallback, "eur")
	err := engine.Graph(a).Runtime.FXRates.Refresh(t.Context())
	require.ErrorContains(t, err, fmt.Sprintf("1 of %d FX base currencies failed; first FX rates for EUR: ", len(codes)), "one line, however many fail")
	require.NotContains(t, err.Error(), "\n")
}

// With no refresh running, a quote reads its base currency's file itself,
// once however many quote at the same time, and stores every rate in it for
// the other replicas. A stale file is refused, and a failing one is not asked
// again within the negative window.
func TestFXQuoteReadsItsBaseOnce(t *testing.T) {
	fx, f := fxfake.New(), newFixture(t)
	t.Cleanup(fx.Close)
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
	require.Equal(t, 1, fx.Requests(fxfake.Primary, ""), "one read of EUR's file")
	_, err := rb.Quote(t.Context(), "EUR", "CAD")
	require.NoError(t, err)
	require.Equal(t, 1, fx.Requests(fxfake.Primary, ""), "the other replica quotes the stored rates")

	fx.Date("gbp", time.Now().Add(-5*24*time.Hour))
	fx.Fail(fxfake.Primary, "jpy")
	fx.Fail(fxfake.Fallback, "jpy")
	_, err = ra.Quote(t.Context(), "GBP", "USD")
	require.ErrorContains(t, err, "stale", "a file published days ago is no rate")
	for range 3 {
		_, err = ra.Quote(t.Context(), "JPY", "USD")
		require.Error(t, err)
	}
	require.Equal(t, 1, fx.Requests(fxfake.Primary, "jpy"), "a failure is remembered")
	require.Equal(t, 1, fx.Requests(fxfake.Fallback, "jpy"))
}
