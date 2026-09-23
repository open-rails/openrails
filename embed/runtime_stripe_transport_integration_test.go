//go:build integration

package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	embcp "github.com/open-rails/openrails/internal/operator"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// #814 gap 1 — the SUPPORTED fake-Stripe seam for embedding hosts.
//
// Options.StripeTransport is a host-supplied, runtime-owned dependency under
// the Stripe choke point. The readonly guard and pinned version still run above
// it, and another runtime cannot replace its destination.
//
// Credential publication drives authenticated provider reads through the fake.
// Native catalog creation retains local terms without creating provider objects.
func TestEmbeddedStripeTransportSeam_DrivesCredentialProbeAndLocalCatalog(t *testing.T) {
	fake := newFakeStripeCatalogAPI(t)

	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sfx := strings.ToLower(uuid.NewString()[:8])
	cfg := &config.Config{
		TestMode:            config.CredentialPostureSandbox,
		AllowCatalogUpdates: true,
		SecretBackend:       config.SecretBackendDB,
		Encryption:          &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
		// Full mode ensures local catalog behavior is not caused by a write guard.
		// Readonly enforcement is qualified by the stripeapi choke-point tests.
		ProviderWriteMode: config.ProviderWriteModeFull,
		DB:                &config.DBConfig{URL: appDSN},
		Auth:              &config.AuthConfig{AllowMemory: true, AllowEphemeralSigningKey: true, KeysPath: t.TempDir(), AllowMissingSenders: true, DirectPeerIP: true, Issuer: "https://stripe-seam-" + sfx + ".openrails.test"},
	}
	e, err := New(context.Background(), Options{
		Config:          cfg,
		PGXPool:         pool,
		River:           RiverManagedByOpenRails(),
		StripeTransport: hostRewriteTransport{target: fake.server.URL},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	ctx := context.Background()
	require.NoError(t, embcp.Attach(ctx, e.app, cfg, pool))
	provisioned, err := embcp.ProvisionMerchant(ctx, e.app, embcp.ProvisionMerchantRequest{Slug: "seam-" + sfx})
	require.NoError(t, err)
	// Bind the engine to its merchant, as an embedding host does at startup.
	e.app.Runtime.SetConfiguredMerchant(provisioned.MerchantID)

	require.NoError(t, e.app.Runtime.EnsureMerchantsService(ctx))
	require.NotNil(t, e.app.Runtime.Merchants, "the merchant credential plane must be armed")
	_, err = e.app.Runtime.Merchants.UpsertPaymentProviderConfig(ctx, provisioned.MerchantID, "stripe",
		merchants.UpsertPaymentProviderConfigRequest{
			OperationID:      uuid.New(),
			ExpectedRevision: new(int64),
			AccountID:        "acct_seam_" + sfx,
			Credentials:      map[string]string{"secret_key": "sk_test_seam_" + sfx},
		})
	require.NoError(t, err)

	svc := e.svc

	var price *billingservice.CatalogPrice
	require.NoError(t, e.app.Runtime.DB.RunInMerchantScope(ctx, provisioned.MerchantID, "catalog rail push", func(mctx context.Context) error {
		product, perr := svc.CreateProduct(mctx, billingservice.CreateProductRequest{
			Key: "seam-prod-" + sfx, DisplayName: "Seam Product",
		})
		if perr != nil {
			return perr
		}
		price, perr = svc.CreatePrice(mctx, billingservice.CreatePriceRequest{
			ProductID:  product.ID,
			UnitAmount: 9_990_000, // micros
			Currency:   "USD",
			PSPs:       []string{"stripe"},
		})
		return perr
	}))
	require.NotNil(t, price)

	// Account and environment verification reached the runtime-owned fake.
	products, prices, versions := fake.snapshot()
	require.Empty(t, products, "native catalog terms do not create Stripe products")
	require.Empty(t, prices, "native catalog terms do not create Stripe prices")
	require.GreaterOrEqual(t, len(versions), 2, "publication verifies account and balance on the fake")
	fake.mu.Lock()
	paths := append([]string(nil), fake.paths...)
	fake.mu.Unlock()
	require.Contains(t, paths, "/v1/account")
	require.Contains(t, paths, "/v1/balance")
	for _, v := range versions {
		require.Equal(t, stripeapi.APIVersion, v, "the choke point pins Stripe-Version above the host transport")
	}
	require.NotContains(t, price.Providers, "stripe", "native terms have no external provider catalog link")
	require.EqualValues(t, 9_990_000, price.UnitAmount)
	require.Equal(t, "USD", price.Currency)

}

// A live-credential posture must never accept a redirected transport.
func TestEmbeddedStripeTransportSeam_RefusedOnLiveCredentials(t *testing.T) {
	_, err := New(context.Background(), Options{
		Config: &config.Config{
			TestMode: config.CredentialPostureLive,
			DB:       &config.DBConfig{URL: "postgres://unused"},
		},
		River:           RiverManagedByOpenRails(),
		StripeTransport: hostRewriteTransport{target: "http://127.0.0.1:1"},
	})
	require.ErrorContains(t, err, "test seam")
}

// hostRewriteTransport sends every request to target regardless of the original
// host (api.stripe.com), preserving method/path/query/body/headers — the shape
// an embedding host installs via Options.StripeTransport.
type hostRewriteTransport struct{ target string }

func (h hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(h.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

type fakeStripeCatalogAPI struct {
	server   *httptest.Server
	mu       sync.Mutex
	products []url.Values
	prices   []url.Values
	versions []string
	paths    []string
}

func newFakeStripeCatalogAPI(t *testing.T) *fakeStripeCatalogAPI {
	t.Helper()
	f := &fakeStripeCatalogAPI{}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	record := func(r *http.Request) {
		f.mu.Lock()
		f.versions = append(f.versions, r.Header.Get(stripeapi.VersionHeader))
		f.paths = append(f.paths, r.URL.Path)
		require.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk_test_seam_"), "fake receives runtime credential")
		require.Equal(t, http.MethodGet, r.Method, "native catalog must not write provider objects")
		f.mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/account", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer sk_test_seam_")
		writeJSON(w, map[string]any{"object": "account", "id": "acct_seam_" + key})
	})
	mux.HandleFunc("GET /v1/balance", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		writeJSON(w, map[string]any{"object": "balance", "livemode": false})
	})
	mux.HandleFunc("GET /v1/products/search", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		writeJSON(w, map[string]any{"object": "search_result", "data": []any{}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/products", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		require.NoError(t, r.ParseForm())
		f.mu.Lock()
		f.products = append(f.products, r.PostForm)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"id": "prod_seam_fake", "object": "product"})
	})
	mux.HandleFunc("GET /v1/prices", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		writeJSON(w, map[string]any{"object": "list", "data": []any{}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/prices", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		require.NoError(t, r.ParseForm())
		f.mu.Lock()
		f.prices = append(f.prices, r.PostForm)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"id": "price_seam_fake", "object": "price"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		t.Errorf("unexpected fake Stripe request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected fake request", http.StatusNotFound)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeStripeCatalogAPI) snapshot() (products, prices []url.Values, versions []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.products...),
		append([]url.Values(nil), f.prices...),
		append([]string(nil), f.versions...)
}
