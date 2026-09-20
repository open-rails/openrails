//go:build integration

package integrationharness

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/catalog"
)

type catalogWorkflow struct {
	surface  *Surface
	merchant OwnedMerchant
	client   *openrails.Client
	token    string
}

type catalogPublishResult struct {
	Plan   *catalog.ApplyPlan   `json:"plan"`
	Result *catalog.ApplyResult `json:"result"`
}

func newCatalogWorkflow(t *testing.T) (*Harness, catalogWorkflow) {
	t.Helper()
	h := New(t, t.Context())
	surface := h.StartStandalone("USD")
	merchant := surface.ProvisionOwnedMerchant("catalog-workflow-" + uuid.NewString()[:8])
	token := surface.MintAPIKey(merchant.MerchantSlug, "catalog-writer", []string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate})
	return h, catalogWorkflow{surface, merchant, surface.Client(openrails.WithAPIKey(token), openrails.WithMerchantID(merchant.MerchantID)), token}
}

func (f catalogWorkflow) publish(t *testing.T, manifest catalog.Manifest, options catalog.ApplyOptions) catalogPublishResult {
	t.Helper()
	status, raw := requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/publish", f.token, map[string]any{"catalog": manifest, "insert": options.Insert, "overwrite": options.Overwrite, "prune": options.Prune})
	require.Equal(t, http.StatusOK, status, string(raw))
	return decodeCatalogPublish(t, raw)
}

func decodeCatalogPublish(t *testing.T, raw []byte) catalogPublishResult {
	t.Helper()
	var out catalogPublishResult
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Plan)
	return out
}

func TestMerchantCatalogWorkflow(t *testing.T) {
	h, f := newCatalogWorkflow(t)
	ctx := t.Context()
	manifest := func(amount int64) catalog.Manifest {
		return catalog.Manifest{Version: catalog.SupportedVersion, Products: []catalog.Product{{Key: "premium", DisplayName: "Premium", Description: "Original", TierGroup: "memberships", TierRank: intPtr(1), Entitlements: []string{"premium"}, Prices: []catalog.Price{{Currency: "USD", UnitAmount: amount, Duration: "30d", AutoRenew: true}}}}}
	}
	t.Run("plan_and_apply", func(t *testing.T) {
		planned := f.publish(t, manifest(10_000_000), catalog.ApplyOptions{})
		require.Nil(t, planned.Result)
		require.Equal(t, 1, countProductActions(planned.Plan, catalog.ProductCreate))
		require.Equal(t, 1, countPriceActions(planned.Plan, catalog.PriceCreate))
		page, err := f.client.ListProducts(ctx, openrails.ProductFilter{})
		require.NoError(t, err)
		require.Empty(t, page.Items)
		applied := f.publish(t, manifest(10_000_000), catalog.ApplyOptions{Insert: true})
		require.Equal(t, 1, applied.Result.ProductsCreated)
		require.Equal(t, 1, applied.Result.PricesCreated)
		require.False(t, f.publish(t, manifest(10_000_000), catalog.ApplyOptions{}).Plan.HasChanges())
		changed := manifest(10_000_000)
		changed.Products[0].DisplayName = "Premium updated"
		changed.Products[0].TierRank = intPtr(2)
		unchanged := f.publish(t, changed, catalog.ApplyOptions{Insert: true})
		require.Zero(t, unchanged.Result.ProductsUpdated)
		product, err := f.client.GetProductByKey(ctx, "premium")
		require.NoError(t, err)
		require.Equal(t, "Premium", product.DisplayName)
		updated := f.publish(t, changed, catalog.ApplyOptions{Overwrite: true})
		require.Equal(t, 1, updated.Result.ProductsUpdated)
		require.Zero(t, updated.Result.PricesCreated)
		product, err = f.client.GetProductByKey(ctx, "premium")
		require.NoError(t, err)
		require.Equal(t, "Premium updated", product.DisplayName)
		require.Equal(t, 2, product.TierRank)
	})
	t.Run("price_versions", func(t *testing.T) {
		original, err := f.client.GetPriceByKey(ctx, "premium-monthly")
		require.NoError(t, err)
		// Historical subscriber seed: publication must never repin its accepted price.
		pool := h.MerchantPool(f.merchant.MerchantID.UUID())
		customer := uuid.New()
		subscription := uuid.New()
		_, err = pool.Exec(ctx, `INSERT INTO openrails.customers(id,merchant_id) VALUES($1,$2)`, customer, f.merchant.MerchantID.UUID())
		require.NoError(t, err)
		psp := dbtest.EnsureTestPSP(ctx, t, pool, f.merchant.MerchantID.UUID(), "nmi")
		_, err = pool.Exec(ctx, `INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,psp_id) VALUES($1,$2,$3,$4,$5,'active','nmi',$6,$7)`, subscription, f.merchant.MerchantID.UUID(), customer, original.ProductID, original.ID, "grandfather-"+subscription.String(), psp)
		require.NoError(t, err)
		all := catalog.ApplyOptions{Insert: true, Overwrite: true, Prune: true}
		result := f.publish(t, manifest(12_000_000), all)
		require.Equal(t, 1, result.Result.PricesCreated)
		require.Equal(t, 1, result.Result.PricesArchived)
		bumped, err := f.client.GetPriceByKey(ctx, "premium-monthly")
		require.NoError(t, err)
		require.NotEqual(t, original.ID, bumped.ID)
		old, err := f.client.GetPrice(ctx, original.ID)
		require.NoError(t, err)
		require.True(t, old.Archived)
		require.Equal(t, "premium-monthly", old.Key)
		var pinned uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT price_id FROM openrails.subscriptions WHERE merchant_id=$1 AND id=$2`, f.merchant.MerchantID.UUID(), subscription).Scan(&pinned))
		require.Equal(t, original.ID.UUID(), pinned)
		require.EqualValues(t, 10_000_000, old.UnitAmount)
		for _, amount := range []int64{10_000_000, 12_000_000} {
			replay := f.publish(t, manifest(amount), all)
			require.Zero(t, replay.Result.PricesCreated)
			require.Equal(t, 1, replay.Result.PricesActivated)
			require.Equal(t, 1, replay.Result.PricesArchived)
			price, err := f.client.GetPriceByKey(ctx, "premium-monthly")
			require.NoError(t, err)
			want := original.ID
			if amount == 12_000_000 {
				want = bumped.ID
			}
			require.Equal(t, want, price.ID)
		}
		prices, err := f.client.ListPrices(ctx, openrails.PriceFilter{ProductID: original.ProductID})
		require.NoError(t, err)
		require.Len(t, prices.Items, 2)
		live := false
		prices, err = f.client.ListPrices(ctx, openrails.PriceFilter{ProductID: original.ProductID, Archived: &live})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		require.Equal(t, bumped.ID, prices.Items[0].ID)
	})
}
