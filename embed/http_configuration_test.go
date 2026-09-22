package embed

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestHTTPConfigurationIsOneShotAndCopied(t *testing.T) {
	runtime := reviewRuntime(nil, nil)
	require.ErrorContains(t, runtime.ConfigureHTTP(HTTPConfig{Customer: true}), "requires")
	require.Nil(t, runtime.httpConfig, "failed validation cannot enable HTTP")
	policy := HTTPConfig{}
	require.NoError(t, runtime.ConfigureHTTP(policy))
	policy.Catalog = true
	require.ErrorContains(t, runtime.ConfigureHTTP(HTTPConfig{}), "already configured")
	routes, err := runtime.HTTPRoutes()
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	for _, route := range routes {
		require.NotContains(t, route.Path, "catalog")
	}
	original := routes[0].Path
	routes[0].Path = "/caller-mutated"
	again, err := runtime.HTTPRoutes()
	require.NoError(t, err)
	require.Equal(t, original, again[0].Path)
	require.ErrorContains(t, runtime.ConfigureHTTP(HTTPConfig{}), "frozen")
	disabled := reviewRuntime(nil, nil)
	_, err = disabled.HTTPRoutes()
	require.ErrorContains(t, err, "disabled")
	require.ErrorContains(t, disabled.ConfigureHTTP(HTTPConfig{}), "frozen")
}

func TestHTTPConcurrentConfigurationHasOneOwner(t *testing.T) {
	runtime := reviewRuntime(nil, nil)
	var wait sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wait.Go(func() { results <- runtime.ConfigureHTTP(HTTPConfig{}) })
	}
	wait.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			require.ErrorContains(t, err, "already configured")
		}
	}
	require.Equal(t, 1, accepted)
}

func TestLateHTTPAuthenticationAndSharedMountLimits(t *testing.T) {
	runtime := reviewRuntime(nil, nil)
	runtime.app.Config.RateLimits = &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	require.NoError(t, runtime.ConfigureHTTP(HTTPConfig{Customer: true, DelegatedAuthenticator: billingauth.DelegatedAuthenticatorFunc(reviewReject)}))
	// Each host obtains a bundle independently. Runtime policy and counters must
	// not reset simply because the second adapter materializes its mount.
	for i, prefix := range []string{"/first", "/second"} {
		mux := http.NewServeMux()
		routes, err := runtime.HTTPRoutes()
		require.NoError(t, err)
		for _, route := range routes {
			mux.Handle(route.Method+" "+prefix+route.Path, route.Handler)
		}
		req := httptest.NewRequest(http.MethodPost, prefix+"/v1/me/checkout", strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.99:1234"
		req.Header.Set("Authorization", "Bearer bad")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if i == 0 {
			require.Equal(t, http.StatusUnauthorized, w.Code)
		} else {
			require.Equal(t, http.StatusTooManyRequests, w.Code)
		}
	}
}
