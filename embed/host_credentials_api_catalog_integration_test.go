//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
)

func TestHostCredentialsWithAPICatalog(t *testing.T) {
	ctx := t.Context()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := manifestModeConfig(dsn)
	cfg.Env = "production"
	cfg.TestMode = config.CredentialPostureSandbox
	cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly
	cfg.CatalogSource = config.CatalogSourceAPI
	require.Nil(t, cfg.Encryption, "host credentials must not need an encryption key")
	slug := "host-catalog-" + uuid.NewString()
	accountID := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const secret = "sk_test_host_owned_fixture"
	boot := func(key string) *embed.Runtime {
		rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), StripeTransport: catalogAuthorityTransport{t: t, key: key}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		_, err = rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{
			DisplayName: "Host credential catalog",
			PSPs: map[string]embed.PSPConfig{"stripe": {"stripe": {
				AccountID: accountID,
				Secrets:   map[string]string{"secret_key": key, "webhook_signing_secret": "whsec_host_fixture"},
			}}},
		})
		require.NoError(t, err)
		return rt
	}
	rt := boot(secret)
	client, err := rt.Client()
	require.NoError(t, err)
	product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: "post", DisplayName: "First title"})
	require.NoError(t, err)
	title := "Updated title"
	_, err = client.UpdateProduct(ctx, product.ID, openrails.UpdateProductRequest{DisplayName: &title})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: "post-usd", UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	require.Equal(t, product.ID, price.ProductID)
	require.ErrorContains(t, rt.PushCatalog(ctx, embed.PushCatalogOptions{Manifest: manifestModeCatalogYAML(slug, 9_000_000), Insert: true}), "catalog_source=api")
	require.NoError(t, rt.PushCatalog(ctx, embed.PushCatalogOptions{Manifest: manifestModeCatalogYAML(slug, 9_000_000), Out: io.Discard}), "API catalogs still permit a read-only manifest comparison")
	runtime := app.HostGraph(rt).Runtime
	mid := runtime.ConfiguredMerchant()
	name, err := merchants.PSPSecretName("stripe", "test", accountID, "secret_key")
	require.NoError(t, err)
	value, err := runtime.ManifestSecrets.Get(ctx, mid, name)
	require.NoError(t, err)
	require.Equal(t, secret, value.Value)
	require.Zero(t, merchantSecretRowCount(t, runtime.DB.Pool(), ctx, mid))
	handler, err := rt.Handler(embed.MountOptions{RouteSets: []embed.RouteSet{embed.RouteSetPaymentProviders}, Gate: allowAllGate{id: mid}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v1/merchant/payment-providers/stripe", strings.NewReader(`{"credentials":{"secret_key":"replacement"}}`)))
	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "manifest_driven")
	require.Zero(t, merchantSecretRowCount(t, runtime.DB.Pool(), ctx, mid))
	value, err = runtime.ManifestSecrets.Get(ctx, mid, name)
	require.NoError(t, err)
	require.Equal(t, secret, value.Value, "rejected mutation must not change the in-memory credential")
	// Rotation follows the host configuration/restart contract, while dynamic
	// catalog state survives that restart and is not overwritten by a manifest.
	require.NoError(t, rt.Close(ctx))
	rt2 := boot("sk_test_rotated_fixture")
	value, err = app.HostGraph(rt2).Runtime.ManifestSecrets.Get(ctx, mid, name)
	require.NoError(t, err)
	require.Equal(t, "sk_test_rotated_fixture", value.Value)
	client2, err := rt2.Client()
	require.NoError(t, err)
	read, err := client2.GetProduct(ctx, product.ID)
	require.NoError(t, err)
	require.Equal(t, title, read.DisplayName)
	require.Zero(t, merchantSecretRowCount(t, app.HostGraph(rt2).Runtime.DB.Pool(), ctx, mid))
	require.NoError(t, rt2.PushCatalog(ctx, embed.PushCatalogOptions{Manifest: manifestModeCatalogYAML(slug, 9_000_000), Out: io.Discard}), "provider comparison must use the rotated host credential")
}

// The inverse combination keeps provider API custody while catalog mutations
// remain host-declared. A webhook signing credential needs no provider traffic.
func TestManagedCredentialsWithManifestCatalog(t *testing.T) {
	ctx := t.Context()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := manifestModeConfig(dsn)
	cfg.Env = "production"
	cfg.TestMode = config.CredentialPostureSandbox
	cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly
	cfg.MerchantConfigSource, cfg.CatalogSource = config.MerchantConfigSourceAPI, config.CatalogSourceManifest
	cfg.SecretBackend = config.SecretBackendDB
	cfg.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	slug := "managed-manifest-" + uuid.NewString()
	account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	boot := func() *embed.Runtime {
		rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), StripeTransport: catalogAuthorityTransport{t: t}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		_, err = rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
		require.NoError(t, err)
		return rt
	}
	rt := boot()
	runtime := app.HostGraph(rt).Runtime
	mid := runtime.ConfiguredMerchant()
	require.Nil(t, runtime.ManifestSecrets, "managed credentials must not acquire a host fallback")
	handler, err := rt.Handler(embed.MountOptions{RouteSets: []embed.RouteSet{embed.RouteSetPaymentProviders}, Gate: allowAllGate{id: mid}})
	require.NoError(t, err)
	for _, secret := range []string{"whsec_managed_catalog_v1", "whsec_managed_catalog_v2"} {
		payload, err := json.Marshal(map[string]any{"account_id": account, "credentials": map[string]string{"webhook_signing_secret": secret}})
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/v1/merchant/payment-providers/stripe", strings.NewReader(string(payload)))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		loaded, err := runtime.Merchants.LoadStripeCredentials(ctx, mid)
		require.NoError(t, err)
		require.Equal(t, secret, loaded.WebhookSigningSecret)
		require.NotZero(t, merchantSecretRowCount(t, runtime.DB.Pool(), ctx, mid))
		inMerchantScope(t, runtime.DB.Pool(), ctx, mid, func(tx pgx.Tx) {
			var persisted string
			name, err := merchants.PSPSecretName("stripe", "test", account, "webhook_signing_secret")
			require.NoError(t, err)
			require.NoError(t, tx.QueryRow(ctx, `SELECT value FROM billing.merchant_secrets WHERE merchant_id=$1 AND name=$2`, mid.UUID(), name).Scan(&persisted))
			require.NotEqual(t, secret, persisted, "managed provider credentials must be encrypted")
		})
	}
	require.NoError(t, rt.PushCatalog(ctx, embed.PushCatalogOptions{Manifest: manifestModeCatalogYAML(slug, 3_000_000), Insert: true, Out: io.Discard}))
	client, err := rt.Client()
	require.NoError(t, err)
	product, err := client.GetProductByKey(ctx, "pro")
	require.NoError(t, err, "manifest catalog reads remain available")
	title := "API overwrite"
	_, err = client.UpdateProduct(ctx, product.ID, openrails.UpdateProductRequest{DisplayName: &title})
	var refusal *openrails.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusMethodNotAllowed, refusal.Status)
	require.Contains(t, err.Error(), "catalog_source=manifest")
	require.NoError(t, rt.Close(ctx))
	restarted := boot()
	loaded, err := app.HostGraph(restarted).Runtime.Merchants.LoadStripeCredentials(ctx, mid)
	require.NoError(t, err)
	require.Equal(t, "whsec_managed_catalog_v2", loaded.WebhookSigningSecret, "managed rotation must survive restart")
}

// Plan-only catalog comparisons may read provider products. Keep those reads
// on a fake transport and verify which host credential is used after restart.
// Neither this fixture nor the managed webhook-only fixture permits writes.
type catalogAuthorityTransport struct {
	t   *testing.T
	key string
}

func (n catalogAuthorityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if n.key != "" && req.Method == http.MethodGet && req.URL.Host == "api.stripe.com" && (req.URL.Path == "/v1/products" || req.URL.Path == "/v1/prices") {
		require.Equal(n.t, "Bearer "+n.key, req.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[],"has_more":false}`)), Request: req}, nil
	}
	n.t.Errorf("unexpected provider request: %s %s", req.Method, req.URL.Path)
	return nil, fmt.Errorf("provider network is disabled in catalog authority tests")
}
