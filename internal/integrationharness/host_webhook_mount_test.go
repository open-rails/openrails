//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/httptesthost"
	embcp "github.com/open-rails/openrails/internal/operator"
)

// A merchant api_host does not select callback authority. Provider accounts,
// runtime restrictions and provider verification select the callback scope.
func TestWebhookMountDoesNotUseHostAsAccountAuthority(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)

	cfg := &config.Config{
		TestMode: config.CredentialPostureSandbox, SecretBackend: config.SecretBackendSnapshot, ProviderWriteMode: config.ProviderWriteModeReadOnly,
		DB: &config.DBConfig{URL: h.DSN},
	}

	e, err := embed.New(context.Background(), embed.Options{Config: cfg, Redis: h.Redis, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	require.NoError(t, embcp.AttachWithOptions(ctx, app.HostGraph(e), cfg, nil, embcp.AttachOptions{Auth: &hostconfig.AuthConfig{Issuer: "https://host-webhook-controlplane.test", AllowMemory: true, AllowEphemeralSigningKey: true, AllowMissingSenders: true, AllowPrivateNetworkJWKS:

	// A bare merchant directory row + api_host is all Host resolution needs
	// (no AuthKit permission-group linking required for this mechanism).
	true, DirectPeerIP: true}}))
	cp := embcp.Get(app.HostGraph(e))
	require.NotNil(t, cp, "control plane attached")

	hostA := "api.host-webhook-a-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	insertMerchantWithHost(t, h.sharedPool(), "host-webhook-a-"+strings.ReplaceAll(uuid.NewString(), "-", ""), hostA)

	handler, err := httptesthost.Handler(e, httptesthost.Options{HTTP: embed.HTTPConfig{}})
	require.NoError(t, err)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	accountless, raw := postHostWebhook(t, srv.URL+"/v1/webhooks/bogus", hostA)
	require.Equal(t, http.StatusNotFound, accountless, string(raw))

	// Unknown and known hosts receive the same unknown-account refusal.
	status, body := postHostWebhook(t, srv.URL+"/v1/webhooks/bogus/account", "api.unknown-webhook-host.test")
	require.Equal(t, http.StatusNotFound, status, string(body))

	// A recognized host cannot turn an invalid provider into a valid callback.
	status, body = postHostWebhook(t, srv.URL+"/v1/webhooks/bogus/account", hostA)
	require.Equal(t, http.StatusNotFound, status, string(body))
	for _, host := range []string{hostA, "api.unknown-webhook-host.test"} {
		status, body = postHostWebhook(t, srv.URL+"/v1/webhooks/stripe/unknown-account", host)
		require.Equal(t, http.StatusNotFound, status, string(body))
	}
}

func postHostWebhook(t *testing.T, url, host string) (int, []byte) {
	t.Helper()
	return requestJSONHost(t, http.MethodPost, url, host, "", map[string]any{"id": "evt_1"})
}

func insertMerchantWithHost(t *testing.T, pool *pgxpool.Pool, slug, apiHost string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO billing.merchants (slug, status, api_host)
		VALUES ($1, 'active', $2)
	`, slug, apiHost)
	require.NoError(t, err)
}
