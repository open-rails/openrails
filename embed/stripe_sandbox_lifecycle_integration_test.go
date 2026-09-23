//go:build integration

package embed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/require"
)

type lifecycleStripeTransport func(*http.Request) (*http.Response, error)

func (f lifecycleStripeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the actual runtime-built checkout Stripe service with two different
// fakes and an ordinary runtime. The ordinary client reads a localhost server;
// neither the default transport nor any global Stripe state is replaced.
func TestStripeSandboxRuntimeTransportLifetime(t *testing.T) {
	_, dsn := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	original := http.DefaultTransport
	var firstRequests, secondRequests, ordinaryRequests atomic.Int64
	handler := func(counter *atomic.Int64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptions" || r.Header.Get(stripeapi.VersionHeader) != stripeapi.APIVersion {
				http.Error(w, "unexpected fake request", 500)
				return
			}
			counter.Add(1)
			_, _ = io.WriteString(w, `{"data":[]}`)
		}
	}
	secondServer := httptest.NewServer(handler(&secondRequests))
	defer secondServer.Close()
	ordinaryServer := httptest.NewServer(handler(&ordinaryRequests))
	defer ordinaryServer.Close()
	boot := func(transport http.RoundTripper, gateway string) *Runtime {
		cfg := &config.Config{TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dsn}}
		if gateway != "" {
			cfg.ProviderSandbox = &config.ProviderSandboxConfig{StripeAPIURL: gateway}
		}
		rt, err := New(t.Context(), Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails(), StripeTransport: transport})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		// Local immutable credentials avoid provider discovery and DB account fixtures.
		rt.app.Runtime.CheckoutService.StripeService.Rails = railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_fake"}}}
		return rt
	}
	first := boot(lifecycleStripeTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "api.stripe.com" || r.URL.Path != "/v1/subscriptions" || r.Header.Get(stripeapi.VersionHeader) != stripeapi.APIVersion {
			return nil, fmt.Errorf("unexpected fake request %s %s", r.Method, r.URL)
		}
		firstRequests.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
	}), "")
	second := boot(nil, secondServer.URL)
	ordinary := boot(nil, "")
	ordinary.app.Runtime.CheckoutService.StripeService.SetBaseURLForTest(ordinaryServer.URL)
	read := func(rt *Runtime) error {
		_, err := rt.app.Runtime.CheckoutService.StripeService.ListActiveSubscriptionsForCustomer(t.Context(), "cus_fake")
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for _, rt := range []*Runtime{first, second, ordinary} {
		wg.Add(1)
		go func(rt *Runtime) {
			defer wg.Done()
			for range 10 {
				errs <- read(rt)
			}
		}(rt)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 20, firstRequests.Load())
	require.EqualValues(t, 20, secondRequests.Load())
	require.EqualValues(t, 20, ordinaryRequests.Load())
	require.NoError(t, first.Close(context.Background()))
	require.NoError(t, read(second))
	require.NoError(t, read(ordinary))
	require.NoError(t, second.Close(context.Background()))
	require.NoError(t, read(ordinary))
	require.EqualValues(t, 20, firstRequests.Load())
	require.EqualValues(t, 22, secondRequests.Load())
	require.EqualValues(t, 24, ordinaryRequests.Load())
	require.Same(t, original, http.DefaultTransport)
}
