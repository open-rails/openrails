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
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/httptesthost"
	embcp "github.com/open-rails/openrails/internal/operator"
)

// TestHostRoutedWebhookMountHTTP proves the #734 Host-routed webhook mount
// (pkg/embedded's RegisterHostWebhookRoutes, saas #15's engine half): the SAME
// Host->merchant resolver used elsewhere (merchant-scoped route resolution,
// the issuer-consistency check) ALSO pins the merchant for inbound webhooks
// at the canonical no-merchant-segment path
// ("/billing/v1/webhooks/:provider"), mounted only when a control plane is
// attached. An unrecognized Host is a hard 404 (fail closed, never a
// fall-through to unscoped processing); a recognized Host resolves and reaches
// the SAME verify/dispatch primitive the path-slug surface uses (proven by an
// unsupported-provider name reaching the "provider not supported" branch,
// which only happens AFTER merchant resolution succeeds — the exact same
// httphandlers.processResolvedMerchantWebhook TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe
// exercises for the path-slug surface, so per-merchant secret isolation there
// covers this mount too).
func TestHostRoutedWebhookMountHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)

	cfg := &config.Config{
		Env:      "dev",
		TestMode: config.CredentialPostureSandbox,
		DB:       &config.DBConfig{URL: h.DSN},
		Auth:     &config.AuthConfig{Issuer: "https://host-webhook-controlplane.test"},
	}

	e, err := embed.New(context.Background(), embed.Options{Config: cfg, Redis: h.Redis, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	require.NoError(t, embcp.AttachWithOptions(ctx, app.HostGraph(e), cfg, nil, embcp.AttachOptions{}))
	cp := embcp.Get(app.HostGraph(e))
	require.NotNil(t, cp, "control plane attached")

	// A bare merchant directory row + api_host is all Host resolution needs
	// (no AuthKit permission-group linking required for this mechanism).
	hostA := "api.host-webhook-a-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	insertMerchantWithHost(t, h.sharedPool(), "host-webhook-a-"+strings.ReplaceAll(uuid.NewString(), "-", ""), hostA)

	handler, err := httptesthost.Handler(e, httptesthost.Options{HTTP: embed.HTTPConfig{}})
	require.NoError(t, err)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	accountless, raw := postHostWebhook(t, srv.URL+"/v1/webhooks/bogus", hostA)
	require.Equal(t, http.StatusNotFound, accountless, string(raw))

	// Unknown Host: no merchant resolves. Hard 404, never a fall-through.
	status, body := postHostWebhook(t, srv.URL+"/v1/webhooks/bogus/account", "api.unknown-webhook-host.test")
	require.Equal(t, http.StatusNotFound, status, string(body))

	// Known Host, unsupported provider: proves resolution DID succeed (a 400
	// from inside processResolvedMerchantWebhook, reachable only once the
	// merchant is pinned) rather than a 404 from unresolved Host.
	status, body = postHostWebhook(t, srv.URL+"/v1/webhooks/bogus/account", hostA)
	require.Equal(t, http.StatusBadRequest, status, string(body))
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
