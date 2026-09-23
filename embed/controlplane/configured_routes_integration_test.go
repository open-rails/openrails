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
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestConfiguredStandaloneRoutesReuseOwnedResources(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(ctx)) })
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://configured.openrails.test", KeysPath: t.TempDir()}})
	require.NoError(t, err)
	graph := app.HostGraph(rt).Runtime
	merchants, capabilities, solana := graph.Merchants, graph.RouteCapabilities, graph.SolanaRPCResolver
	routes, err := cp.HTTPRoutes()
	require.NoError(t, err)
	require.True(t, cp.HTTPRequiresRoot())
	bundle, err := openrailshttp.Routes(cp)
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
	again, err := cp.HTTPRoutes()
	require.NoError(t, err)
	require.Len(t, again, len(routes))
}

func TestRejectedStandaloneExposureDoesNotRearmRuntime(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}}
	reject := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	})
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(ctx)) })
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://rejected.openrails.test", KeysPath: t.TempDir()}}, embed.CustomerRoutesConfig{DelegatedAuthenticator: reject})
	require.NoError(t, err)
	graph := app.HostGraph(rt).Runtime
	merchants, capabilities, solana := graph.Merchants, graph.RouteCapabilities, graph.SolanaRPCResolver
	for range 2 {
		_, err = cp.HTTPRoutes()
		require.ErrorContains(t, err, "conflicting")
		require.Same(t, merchants, graph.Merchants)
		require.Same(t, capabilities, graph.RouteCapabilities)
		require.Same(t, solana, graph.SolanaRPCResolver)
	}
}
