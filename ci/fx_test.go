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

// Starting reads no rates. A refresh reads each currency's file once (34
// requests, not one per pair), the fallback for a failing primary, and the
// instance then quotes every pair from memory. A base that fails keeps the
// rates it holds.
func TestFXRefreshWarmsTheInstance(t *testing.T) {
	fx, f := fxfake.New(), newFixture(t)
	t.Cleanup(fx.Close)
	rt := engine.Graph(f.fxEngine(t, fx, true)).Runtime
	codes := moneyutil.CurrencyCodes()
	require.Zero(t, fx.Requests(fxfake.Primary, "")+fx.Requests(fxfake.Fallback, ""), "Start reads no rates")

	require.NoError(t, rt.FXRates.Refresh(t.Context()))
	require.Equal(t, len(codes), fx.Requests(fxfake.Primary, ""), "a refresh is one request per currency")
	for _, code := range codes {
		require.Equal(t, 1, fx.Requests(fxfake.Primary, strings.ToLower(code)), code)
	}
	for _, from := range codes {
		for _, to := range codes {
			q, err := rt.FXProvider.Quote(t.Context(), from, to)
			require.NoError(t, err, "%s -> %s", from, to)
			require.Positive(t, q.Rate)
		}
	}
	require.Equal(t, len(codes), fx.Requests(fxfake.Primary, ""), "every quote read memory, none the source")
	require.Zero(t, fx.Requests(fxfake.Fallback, ""))

	fx.Fail(fxfake.Primary, "eur")
	require.NoError(t, rt.FXRates.Refresh(t.Context()))
	require.Equal(t, 2*len(codes), fx.Requests(fxfake.Primary, ""))
	require.Equal(t, 1, fx.Requests(fxfake.Fallback, ""), "only the failing one read from the fallback")
	require.Equal(t, 1, fx.Requests(fxfake.Fallback, "eur"))

	fx.Fail(fxfake.Fallback, "eur")
	err := rt.FXRates.Refresh(t.Context())
	require.ErrorContains(t, err, fmt.Sprintf("1 of %d FX base currencies failed; first FX rates for EUR: ", len(codes)), "one line, however many fail")
	require.NotContains(t, err.Error(), "\n")
	q, err := rt.FXProvider.Quote(t.Context(), "EUR", "USD")
	require.NoError(t, err, "today's held rate stands")
	require.Positive(t, q.Rate)
	require.Equal(t, 3*len(codes), fx.Requests(fxfake.Primary, ""))
}

// A cold instance reads a base's file when a quote first needs it, once
// however many quote at the same time. Each instance holds its own rates.
func TestFXColdInstanceReadsEachBaseOnce(t *testing.T) {
	fx, f := fxfake.New(), newFixture(t)
	t.Cleanup(fx.Close)
	a, b := engine.Graph(f.fxEngine(t, fx, false)).Runtime, engine.Graph(f.fxEngine(t, fx, false)).Runtime

	type pair struct{ from, to string }
	var pairs []pair
	for range 4 {
		for _, to := range []string{"USD", "GBP", "JPY", "CHF"} {
			pairs = append(pairs, pair{"EUR", to})
		}
		for _, to := range []string{"USD", "EUR"} {
			pairs = append(pairs, pair{"JPY", to})
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(pairs))
	for _, p := range pairs {
		wg.Go(func() {
			_, err := a.FXProvider.Quote(t.Context(), p.from, p.to)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, fx.Requests(fxfake.Primary, "eur"), "one read of EUR's file")
	require.Equal(t, 1, fx.Requests(fxfake.Primary, "jpy"), "one read of JPY's file")
	require.Equal(t, 2, fx.Requests(fxfake.Primary, ""))

	_, err := a.FXProvider.Quote(t.Context(), "EUR", "CAD")
	require.NoError(t, err)
	require.Equal(t, 2, fx.Requests(fxfake.Primary, ""), "the held file lists every currency")
	_, err = b.FXProvider.Quote(t.Context(), "EUR", "CAD")
	require.NoError(t, err)
	require.Equal(t, 2, fx.Requests(fxfake.Primary, "eur"), "the other instance reads its own")
}

// A file published days ago, or one neither host serves, is no rate: the
// quote fails and nothing stands in for it. A failure is not asked again
// within the negative window, and a refresh holds nothing it refused.
func TestFXRefusesStaleOrFailedSource(t *testing.T) {
	fx, f := fxfake.New(), newFixture(t)
	t.Cleanup(fx.Close)
	rt := engine.Graph(f.fxEngine(t, fx, false)).Runtime

	fx.Date("gbp", time.Now().Add(-5*24*time.Hour))
	q, err := rt.FXProvider.Quote(t.Context(), "GBP", "USD")
	require.ErrorContains(t, err, "stale")
	require.Nil(t, q)

	fx.Fail(fxfake.Primary, "jpy")
	fx.Fail(fxfake.Fallback, "jpy")
	for range 3 {
		q, err = rt.FXProvider.Quote(t.Context(), "JPY", "USD")
		require.ErrorContains(t, err, "FX rate unavailable for JPY -> USD")
		require.Nil(t, q)
	}
	require.Equal(t, 1, fx.Requests(fxfake.Primary, "jpy"), "a failure is remembered")
	require.Equal(t, 1, fx.Requests(fxfake.Fallback, "jpy"))

	err = rt.FXRates.Refresh(t.Context())
	require.ErrorContains(t, err, "2 of ")
	_, err = rt.FXProvider.Quote(t.Context(), "USD", "GBP")
	require.NoError(t, err, "the other bases refreshed")
	for _, base := range []string{"GBP", "JPY"} {
		q, err = rt.FXProvider.Quote(t.Context(), base, "USD")
		require.Error(t, err, base)
		require.Nil(t, q)
	}
}
