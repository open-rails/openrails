//go:build integration

package embed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

type lifecycleStripeTransport func(*http.Request) (*http.Response, error)

func (f lifecycleStripeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Closing a configured sandbox must release its process-wide test transport.
// The default transport is intercepted so this test can never contact Stripe.
func TestStripeSandboxRuntimeTransportLifetime(t *testing.T) {
	_, dsn := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var sandboxRequests, otherRequests, defaultRequests atomic.Int64
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxRequests.Add(1)
		_, _ = io.WriteString(w, `{"object":"balance"}`)
	}))
	t.Cleanup(sandbox.Close)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherRequests.Add(1)
		_, _ = io.WriteString(w, `{"object":"balance"}`)
	}))
	t.Cleanup(other.Close)
	original := http.DefaultTransport
	http.DefaultTransport = lifecycleStripeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == strings.TrimPrefix(sandbox.URL, "http://") || r.URL.Host == strings.TrimPrefix(other.URL, "http://") {
			return original.RoundTrip(r)
		}
		if r.URL.Host != "api.stripe.com" {
			return nil, fmt.Errorf("unexpected non-loopback request to %s", r.URL.Host)
		}
		defaultRequests.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"object":"balance"}`)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original; stripeapi.SetBaseTransport(nil) })
	options := func(gateway string) Options {
		return Options{Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dsn}, ProviderSandbox: &config.ProviderSandboxConfig{StripeAPIURL: gateway}}, PGXPool: pool, River: RiverManagedByOpenRails()}
	}
	boot := func(gateway string) *Runtime {
		rt, err := New(t.Context(), options(gateway))
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		return rt
	}
	read := func() {
		resp, err := stripeapi.ReadOnlyClient(0).Get("https://api.stripe.com/v1/balance")
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	configured := boot(sandbox.URL)
	read()
	require.EqualValues(t, 1, sandboxRequests.Load())
	require.Zero(t, defaultRequests.Load())
	require.NoError(t, configured.Close(context.Background()))
	ordinary := boot("")
	read()
	require.EqualValues(t, 1, sandboxRequests.Load(), "the closed runtime must not reroute a later ordinary runtime")
	require.EqualValues(t, 1, defaultRequests.Load())
	require.NoError(t, ordinary.Close(context.Background()))

	// A failed host River bind occurs after the provider graph is constructed.
	failed := options(sandbox.URL)
	failed.River = RiverFromHost()
	failedRuntime, err := New(t.Context(), failed)
	require.NoError(t, err)
	_, err = failedRuntime.BindRiver(t.Context(), func(context.Context, *river.Config) (*river.Client[pgx.Tx], error) {
		return nil, errors.New("deliberate host bind failure")
	})
	require.ErrorContains(t, err, "deliberate host bind failure")
	require.NoError(t, failedRuntime.Close(context.Background()), "failed startup explicitly closes its runtime")
	read()
	require.EqualValues(t, 2, defaultRequests.Load(), "failed startup cleanup releases its sandbox lease")
	require.EqualValues(t, 1, sandboxRequests.Load())

	// Closing owners out of order must neither clear the surviving runtime nor
	// revive an older override when the surviving one eventually closes.
	older, newer := boot(sandbox.URL), boot(other.URL)
	read()
	require.EqualValues(t, 1, otherRequests.Load())
	require.NoError(t, older.Close(context.Background()))
	read()
	require.EqualValues(t, 2, otherRequests.Load(), "the newer owner remains active")
	require.NoError(t, newer.Close(context.Background()))
	read()
	require.EqualValues(t, 3, defaultRequests.Load())
	require.EqualValues(t, 1, sandboxRequests.Load(), "the closed older owner never revives")
}
