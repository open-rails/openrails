//go:build integration

package openrailsfiber

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gofiber/fiber/v3"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestStandaloneNativeFiberInventoryAndCustomerParameters(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureSandbox, MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}, AdminConsole: &config.AdminConsoleConfig{Enabled: true}}
	assets := fstest.MapFS{"index.html": {Data: []byte("console page")}, "assets/site.js": {Data: []byte("console asset")}}
	calls := 0
	auth := billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		require.Equal(t, "owner", r.PathValue("slug"))
		require.Equal(t, "not-id", r.PathValue("id"))
		require.Equal(t, "/api/v1/merchants/owner/billing/me/subscriptions/not-id/cancel?proof=raw", r.RequestURI)
		return nil, billingauth.GateError{Status: 403, Message: "host denied"}
	})
	runtime, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), ConsoleAssets: assets})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(ctx)) })
	cp, err := controlplane.Attach(ctx, runtime, controlplane.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://fiber.openrails.test", KeysPath: t.TempDir(), AllowMemory: true, AllowEphemeralSigningKey: true, AllowMissingSenders: true, AllowPrivateNetworkJWKS: true, DirectPeerIP: true}}, embed.CustomerRoutesConfig{Prefix: "/api/v1/merchants/{slug}/billing/me", Scope: embed.CustomerSubscriptionManagement, DelegatedAuthenticator: auth})
	require.NoError(t, err)
	bundle, err := Routes(cp)
	require.NoError(t, err)
	target := fiber.New(fiber.Config{CaseSensitive: true, StrictRouting: true})
	require.ErrorContains(t, bundle.Mount(target.Group("/outer")), "root")
	require.Empty(t, target.GetRoutes())
	require.NoError(t, bundle.Mount(target))
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/auth/capabilities", 200}, {"GET", "/auth/me", 401}, {"GET", "/admin/", 200}, {"GET", "/admin/assets/site.js", 200}, {"HEAD", "/admin/assets/site.js", 200},
		{"POST", "/api/v1/merchants/owner/billing/me/subscriptions/not-id/cancel?proof=raw", 403},
		{"POST", "/api/v1/merchants/owner/billing/me/checkout", 404},
	} {
		req := httptest.NewRequest(test.method, test.path, nil)
		req.Header.Set("Authorization", "Bearer untrusted")
		resp, err := target.Test(req)
		require.NoError(t, err)
		require.Equal(t, test.status, resp.StatusCode, test.path)
		resp.Body.Close()
	}
	require.Equal(t, 1, calls)
}
