//go:build integration

package integrationharness

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/stretchr/testify/require"
)

type catalogWorkflow struct {
	surface  *Surface
	merchant OwnedMerchant
	client   *openrails.Client
	token    string
}

func newCatalogWorkflow(t *testing.T) (*Harness, catalogWorkflow) {
	t.Helper()
	h := New(t, t.Context())
	surface := h.StartStandalone("USD")
	merchant := surface.ProvisionOwnedMerchant("catalog-workflow-" + uuid.NewString()[:8])
	token := surface.MintAPIKey(merchant.MerchantSlug, "catalog-writer", []string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate})
	return h, catalogWorkflow{surface, merchant, surface.Client(openrails.WithAPIKey(token), openrails.WithMerchantID(merchant.MerchantID)), token}
}

func (f catalogWorkflow) application(t *testing.T, products []catalog.ApplyProduct) *catalog.Application {
	t.Helper()
	revision, err := f.client.Catalog.Revision(t.Context())
	require.NoError(t, err)
	return &catalog.Application{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: &revision.Revision, Products: products}
}

func TestMerchantCatalogWorkflow(t *testing.T) {
	h, f := newCatalogWorkflow(t)
	ctx := t.Context()
	declaration := []catalog.ApplyProduct{{Key: "premium", DisplayName: catalog.Value("Premium"), Description: catalog.Value("Original"),
		EntitlementsSpec: catalog.Value(map[string]*int{"premium": nil}), Prices: []catalog.ApplyPrice{{Key: "premium-monthly", Currency: catalog.Value("USD"), UnitAmount: catalog.Value(int64(10_000_000)), AccessDurationHours: catalog.Value(720), AutoRenew: catalog.Value(true)}}}}
	originalRequest := f.application(t, declaration)
	first, err := f.client.Catalog.Apply(ctx, originalRequest)
	require.NoError(t, err)
	require.False(t, first.Replayed)
	product, err := f.client.Products.RetrieveByKey(ctx, "premium")
	require.NoError(t, err)
	title := "An API edit"
	_, err = f.client.Products.Update(ctx, product.ID, &openrails.ProductUpdateParams{DisplayName: &title})
	require.NoError(t, err)
	replay, err := f.client.Catalog.Apply(ctx, originalRequest)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, first.AppliedRevision, replay.AppliedRevision)
	product, err = f.client.Products.Retrieve(ctx, product.ID)
	require.NoError(t, err)
	require.Equal(t, title, product.DisplayName)

	original, err := f.client.Prices.RetrieveByKey(ctx, "premium-monthly")
	require.NoError(t, err)
	pool := h.MerchantPool(f.merchant.MerchantID.UUID())
	customer, subscription := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2)`, customer, f.merchant.MerchantID.UUID())
	require.NoError(t, err)
	psp := dbtest.EnsureTestPSP(ctx, t, pool, f.merchant.MerchantID.UUID(), "nmi")
	_, err = pool.Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,psp_id) VALUES($1,$2,$3,$4,$5,'active','nmi',$6,$7)`, subscription, f.merchant.MerchantID.UUID(), customer, sdkProductID(t, original.ProductID).UUID(), sdkPriceID(t, original.ID).UUID(), "grandfather-"+subscription.String(), psp)
	require.NoError(t, err)
	bump := f.application(t, []catalog.ApplyProduct{{Key: "premium", Prices: []catalog.ApplyPrice{{Key: "premium-monthly", UnitAmount: catalog.Value(int64(12_000_000))}}}})
	_, err = f.client.Catalog.Apply(ctx, bump)
	require.NoError(t, err)
	bumped, err := f.client.Prices.RetrieveByKey(ctx, "premium-monthly")
	require.NoError(t, err)
	require.NotEqual(t, original.ID, bumped.ID)
	old, err := f.client.Prices.Retrieve(ctx, original.ID)
	require.NoError(t, err)
	require.True(t, old.Archived)
	require.EqualValues(t, 10_000_000, old.UnitAmount)
	var pinned uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT price_id FROM billing.subscriptions WHERE merchant_id=$1 AND id=$2`, f.merchant.MerchantID.UUID(), subscription).Scan(&pinned))
	require.Equal(t, sdkPriceID(t, original.ID).UUID(), pinned)
	status, raw := requestJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/catalog/prices/by-key/premium-monthly/history", f.token, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var history struct {
		Items []struct {
			Archived bool                   `json:"archived"`
			Price    openrails.CatalogPrice `json:"price"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(raw, &history))
	require.Len(t, history.Items, 3, "binding, retirement and replacement are all recorded")
	require.False(t, history.Items[0].Archived)
	require.True(t, history.Items[1].Archived)
	require.False(t, history.Items[2].Archived)
	require.Equal(t, bumped.ID, history.Items[0].Price.ID.String())

	retire := f.application(t, []catalog.ApplyProduct{{Key: "premium", Prices: []catalog.ApplyPrice{{Key: "premium-monthly", Archived: catalog.Value(true)}}}})
	_, err = f.client.Catalog.Apply(ctx, retire)
	require.NoError(t, err)
	archived, err := f.client.Prices.Retrieve(ctx, bumped.ID)
	require.NoError(t, err)
	require.True(t, archived.Archived, "explicit retirement does not require prune")
	_, err = f.client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "api-only", DisplayName: "API only"})
	require.NoError(t, err)
	preserve := f.application(t, []catalog.ApplyProduct{{Key: "premium", Description: catalog.Value("changed")}})
	_, err = f.client.Catalog.Apply(ctx, preserve)
	require.NoError(t, err)
	other, err := f.client.Products.RetrieveByKey(ctx, "api-only")
	require.NoError(t, err)
	require.False(t, other.Archived)
	reader := f.surface.MintAPIKey(f.merchant.MerchantSlug, "catalog-reader", []string{controlplane.PermMerchantCatalogRead})
	status, raw = requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/applications", reader, f.application(t, nil))
	require.Equal(t, http.StatusForbidden, status, string(raw))
	status, raw = requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/applications", f.token, map[string]any{"schema_version": 1, "application_id": "invalid", "expected_revision": 0, "insert": true})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	require.Contains(t, string(raw), "insert")
}
