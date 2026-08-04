//go:build integration

package embedded

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
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// #814 gap 1 — the SUPPORTED fake-Stripe seam for embedding hosts.
//
// StripeService.SetBaseURLForTest and stripeapi.SetBaseTransport both live in
// internal/ (the latter used to be `integration`-tagged as well), so a host
// embedding OpenRails could not integration-test ANY rail-push path against a
// fake Stripe — it had to trust engine-side tests. Options.StripeTransport is
// that seam: a host-supplied RoundTripper installed UNDER the stripeapi choke
// point, so the readonly guard and the pinned Stripe-Version still run above it.
//
// This drives the real catalog rail-push (CreatePrice -> the Stripe adapter's
// find-or-create) through an embedded engine onto a fake wire server.
func TestEmbeddedStripeTransportSeam_DrivesCatalogRailPush(t *testing.T) {
	fake := newFakeStripeCatalogAPI(t)

	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sfx := strings.ToLower(uuid.NewString()[:8])
	cfg := &config.Config{
		Env:            "development",
		TestMode:       config.CredentialPostureSandbox,
		MerchantSource: config.MerchantSourceAPI,
		// The seam exists to exercise the WRITE path; readonly is proven above
		// the transport by the stripeapi choke-point tests.
		ProviderWriteMode: config.ProviderWriteModeFull,
		DB:                &config.DBConfig{URL: appDSN},
		Auth:              &config.AuthConfig{Issuer: "https://stripe-seam-" + sfx + ".openrails.test"},
	}
	e, err := New(Options{
		Config:          cfg,
		PGXPool:         pool,
		River:           RiverManagedByOpenRails(),
		StripeTransport: hostRewriteTransport{target: fake.server.URL},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	ctx := context.Background()
	require.NoError(t, embcp.Attach(ctx, e.App(), cfg, pool))
	provisioned, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: "seam-" + sfx})
	require.NoError(t, err)
	// Bind the engine to its merchant — what an embedding host's provisioning
	// does, and what makes the no-argument RunInMerchant pin below possible.
	e.App().Runtime.SetConfiguredMerchant(provisioned.MerchantID)

	require.NoError(t, e.App().Runtime.EnsureMerchantsService(ctx))
	require.NotNil(t, e.App().Runtime.Merchants, "the merchant credential plane must be armed")
	_, err = e.App().Runtime.Merchants.UpsertPaymentProviderConfig(ctx, provisioned.MerchantID, "stripe",
		merchants.UpsertPaymentProviderConfigRequest{
			AccountID:   "acct_seam_" + sfx,
			Credentials: map[string]string{"secret_key": "sk_test_seam_" + sfx},
		})
	require.NoError(t, err)

	svc, err := e.Service()
	require.NoError(t, err)

	var price *billingservice.CatalogPrice
	require.NoError(t, e.RunInMerchant(ctx, func(mctx context.Context) error {
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

	// The push actually reached the host's fake wire server — through the choke
	// point, which stamped the pinned API version on every request.
	products, prices, versions := fake.snapshot()
	require.NotEmpty(t, products, "the Stripe Product was created on the fake")
	require.Equal(t, "Seam Product", products[0].Get("name"))
	require.NotEmpty(t, prices, "the Stripe Price was created on the fake")
	require.Equal(t, "999", prices[0].Get("unit_amount"), "micros -> Stripe cents")
	require.Equal(t, "usd", prices[0].Get("currency"))
	for _, v := range versions {
		require.Equal(t, stripeapi.APIVersion, v, "the choke point still pins Stripe-Version above the host transport")
	}

	// The link the catalog persisted is the fake's id — proof the host can
	// assert on rail-push results, which is the whole point of the seam.
	state, ok := price.Providers["stripe"]
	require.True(t, ok, "the stripe provider slot is populated")
	require.Equal(t, "price_seam_fake", state.IDs["price_id"])
	require.Equal(t, "prod_seam_fake", state.IDs["product_id"])
}

// A live-credential posture must never accept a redirected transport.
func TestEmbeddedStripeTransportSeam_RefusedOnLiveCredentials(t *testing.T) {
	_, err := New(Options{
		Config: &config.Config{
			Env:      "production",
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
		f.mu.Unlock()
	}
	mux := http.NewServeMux()
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
		writeJSON(w, map[string]any{"object": "list", "data": []any{}, "has_more": false})
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
