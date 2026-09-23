//go:build integration

package controlplane_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestConfiguredStandaloneRoutesReuseOwnedResources(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}, Auth: &config.AuthConfig{Issuer: "https://configured.openrails.test", KeysPath: t.TempDir()}}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(ctx)) })
	_, err = controlplane.Attach(ctx, rt, controlplane.Options{})
	require.NoError(t, err)
	graph := app.HostGraph(rt).Runtime
	merchants, capabilities, solana := graph.Merchants, graph.RouteCapabilities, graph.SolanaRPCResolver
	require.NoError(t, rt.ConfigureHTTP(embed.HTTPConfig{Standalone: true}))
	routes, err := rt.HTTPRoutes()
	require.NoError(t, err)
	require.True(t, rt.HTTPRequiresRoot())
	bundle, err := openrailshttp.Routes(rt)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.ErrorContains(t, bundle.Mount(mux, "/outer"), "root")
	require.NoError(t, bundle.Mount(mux))
	chiRoot := chi.NewRouter()
	require.NoError(t, bundle.MountRoot(chiRoot))
	for _, target := range []http.Handler{mux, chiRoot} {
		w := httptest.NewRecorder()
		target.ServeHTTP(w, httptest.NewRequest("GET", "/auth/capabilities", nil))
		require.Equal(t, 200, w.Code, w.Body.String())
		w = httptest.NewRecorder()
		target.ServeHTTP(w, httptest.NewRequest("GET", "/auth/me", nil))
		require.Equal(t, 401, w.Code, w.Body.String())
		w = httptest.NewRecorder()
		target.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
		require.Equal(t, 404, w.Code)
	}
	require.Same(t, merchants, graph.Merchants)
	require.Same(t, capabilities, graph.RouteCapabilities)
	require.Same(t, solana, graph.SolanaRPCResolver)
	again, err := rt.HTTPRoutes()
	require.NoError(t, err)
	require.Len(t, again, len(routes))
}

func TestRejectedStandaloneExposureDoesNotRearmRuntime(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}, Auth: &config.AuthConfig{Issuer: "https://rejected.openrails.test", KeysPath: t.TempDir()}}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(ctx)) })
	_, err = controlplane.Attach(ctx, rt, controlplane.Options{})
	require.NoError(t, err)
	graph := app.HostGraph(rt).Runtime
	merchants, capabilities, solana := graph.Merchants, graph.RouteCapabilities, graph.SolanaRPCResolver
	reject := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	})
	require.NoError(t, rt.ConfigureHTTP(embed.HTTPConfig{Standalone: true, CustomerExposures: []embed.CustomerHTTPConfig{{Prefix: "/v1/me", DelegatedAuthenticator: reject}}}))
	for range 2 {
		_, err = rt.HTTPRoutes()
		require.ErrorContains(t, err, "conflicting")
		require.Same(t, merchants, graph.Merchants)
		require.Same(t, capabilities, graph.RouteCapabilities)
		require.Same(t, solana, graph.SolanaRPCResolver)
	}
}
