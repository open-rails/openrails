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

type catalogPublishResult = openrails.CatalogPublishResponse

func newCatalogWorkflow(t *testing.T) (*Harness, catalogWorkflow) {
	t.Helper()
	h := New(t, t.Context())
	surface := h.StartStandalone("USD")
	merchant := surface.ProvisionOwnedMerchant("catalog-workflow-" + uuid.NewString()[:8])
	token := surface.MintAPIKey(merchant.MerchantSlug, "catalog-writer", []string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate})
	return h, catalogWorkflow{surface, merchant, surface.Client(openrails.WithAPIKey(token), openrails.WithMerchantID(merchant.MerchantID)), token}
}

func (f catalogWorkflow) publish(t *testing.T, manifest catalog.Manifest, options openrails.CatalogPublishRequest) catalogPublishResult {
	t.Helper()
	options.Catalog = manifest
	out, err := f.client.PublishCatalog(t.Context(), options)
	require.NoError(t, err)
	require.NotNil(t, out.Plan)
	return *out
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
		planned := f.publish(t, manifest(10_000_000), openrails.CatalogPublishRequest{})
		require.Nil(t, planned.Result)
		require.Equal(t, 1, countProductActions(planned.Plan, openrails.CatalogProductCreate))
		require.Equal(t, 1, countPriceActions(planned.Plan, openrails.CatalogPriceCreate))
		page, err := f.client.Products.List(ctx, &openrails.ProductListParams{})
		require.NoError(t, err)
		require.Empty(t, page.Items)
		applied := f.publish(t, manifest(10_000_000), openrails.CatalogPublishRequest{Insert: true})
		require.Equal(t, 1, applied.Result.ProductsCreated)
		require.Equal(t, 1, applied.Result.PricesCreated)
		require.False(t, f.publish(t, manifest(10_000_000), openrails.CatalogPublishRequest{}).Plan.HasChanges())
		changed := manifest(10_000_000)
		changed.Products[0].DisplayName = "Premium updated"
		changed.Products[0].TierRank = intPtr(2)
		changed.Products[0].Description = "Updated description"
		changed.Products[0].Entitlements = []string{"premium", "feature"}
		unchanged := f.publish(t, changed, openrails.CatalogPublishRequest{Insert: true})
		require.Zero(t, unchanged.Result.ProductsUpdated)
		product, err := f.client.Products.RetrieveByKey(ctx, "premium")
		require.NoError(t, err)
		require.Equal(t, "Premium", product.DisplayName)
		updated := f.publish(t, changed, openrails.CatalogPublishRequest{Overwrite: true})
		require.Equal(t, 1, updated.Result.ProductsUpdated)
		require.Zero(t, updated.Result.PricesCreated)
		product, err = f.client.Products.RetrieveByKey(ctx, "premium")
		require.NoError(t, err)
		require.Equal(t, "Premium updated", product.DisplayName)
		require.Equal(t, 2, product.TierRank)
		require.Equal(t, "Updated description", product.Description)
		require.Equal(t, map[string]*int{"premium": nil, "feature": nil}, product.EntitlementsSpec)
	})
	t.Run("price_versions", func(t *testing.T) {
		original, err := f.client.Prices.RetrieveByKey(ctx, "premium-monthly")
		require.NoError(t, err)
		// Historical subscriber seed: publication must never repin its accepted price.
		pool := h.MerchantPool(f.merchant.MerchantID.UUID())
		customer := uuid.New()
		subscription := uuid.New()
		_, err = pool.Exec(ctx, `INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2)`, customer, f.merchant.MerchantID.UUID())
		require.NoError(t, err)
		psp := dbtest.EnsureTestPSP(ctx, t, pool, f.merchant.MerchantID.UUID(), "nmi")
		_, err = pool.Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,psp_id) VALUES($1,$2,$3,$4,$5,'active','nmi',$6,$7)`, subscription, f.merchant.MerchantID.UUID(), customer, original.ProductID, original.ID, "grandfather-"+subscription.String(), psp)
		require.NoError(t, err)
		history := func() []openrails.CatalogPrice {
			t.Helper()
			status, raw := requestJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/catalog/prices/by-key/premium-monthly/history", f.token, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var page struct {
				Items []struct {
					Price openrails.CatalogPrice `json:"price"`
				} `json:"items"`
			}
			require.NoError(t, json.Unmarshal(raw, &page))
			var prices []openrails.CatalogPrice
			for _, item := range page.Items {
				prices = append(prices, item.Price)
			}
			return prices
		}
		all := openrails.CatalogPublishRequest{Insert: true, Overwrite: true, Prune: true}
		result := f.publish(t, manifest(12_000_000), all)
		require.Equal(t, 1, result.Result.PricesCreated)
		require.Equal(t, 1, result.Result.PricesArchived)
		bumped, err := f.client.Prices.RetrieveByKey(ctx, "premium-monthly")
		require.NoError(t, err)
		require.NotEqual(t, original.ID, bumped.ID)
		old, err := f.client.Prices.Retrieve(ctx, original.ID)
		require.NoError(t, err)
		require.True(t, old.Archived)
		require.Equal(t, "premium-monthly", old.Key)
		var pinned uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT price_id FROM billing.subscriptions WHERE merchant_id=$1 AND id=$2`, f.merchant.MerchantID.UUID(), subscription).Scan(&pinned))
		require.Equal(t, sdkPriceID(t, original.ID).UUID(), pinned)
		require.EqualValues(t, 10_000_000, old.UnitAmount)
		require.Len(t, history(), 2, "initial binding and version bump are recorded")
		for _, amount := range []int64{10_000_000, 12_000_000} {
			replay := f.publish(t, manifest(amount), all)
			require.Zero(t, replay.Result.PricesCreated)
			require.Equal(t, 1, replay.Result.PricesActivated)
			require.Equal(t, 1, replay.Result.PricesArchived)
			price, err := f.client.Prices.RetrieveByKey(ctx, "premium-monthly")
			require.NoError(t, err)
			want := original.ID
			if amount == 12_000_000 {
				want = bumped.ID
			}
			require.Equal(t, want, price.ID)
		}
		prices, err := f.client.Prices.List(ctx, &openrails.PriceListParams{ProductID: original.ProductID})
		require.NoError(t, err)
		require.Len(t, prices.Items, 2)
		require.Len(t, history(), 4, "each real flip records one pointer movement")
		live := false
		prices, err = f.client.Prices.List(ctx, &openrails.PriceListParams{ProductID: original.ProductID, Archived: &live})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		require.Equal(t, bumped.ID, prices.Items[0].ID)
	})
	t.Run("authority_and_refusals", func(t *testing.T) {
		path := f.surface.BaseURL + "/v1/merchant/catalog/products"
		reader := f.surface.MintAPIKey(f.merchant.MerchantSlug, "catalog-reader", []string{controlplane.PermMerchantCatalogRead})
		unscoped := f.surface.MintAPIKey(f.merchant.MerchantSlug, "customer-settings-only", []string{controlplane.PermMerchantCustomerSettingsRead})
		for _, row := range []struct {
			method, token string
			want          int
		}{
			{http.MethodGet, "", http.StatusUnauthorized}, {http.MethodGet, unscoped, http.StatusForbidden}, {http.MethodPost, reader, http.StatusForbidden},
		} {
			status, raw := requestJSON(t, row.method, path, row.token, map[string]any{"key": "denied", "display_name": "Denied"})
			require.Equal(t, row.want, status, string(raw))
		}
		product, err := f.client.Products.RetrieveByKey(ctx, "premium")
		require.NoError(t, err)
		for _, field := range []string{"credits_spec", "set_credits"} {
			status, raw := requestJSON(t, http.MethodPatch, path+"/"+product.ID, f.token, map[string]any{field: true})
			assertCatalogUnknownField(t, status, raw, field)
		}
		for _, field := range []string{"credits", "includes", "usage_limits"} {
			status, raw := requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/publish", f.token, map[string]any{
				"catalog": map[string]any{"version": 1, "products": []any{map[string]any{"key": "retired", "display_name": "Retired", field: []any{}}}}, "insert": true,
			})
			assertCatalogUnknownField(t, status, raw, field)
		}
		status, raw := requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/publish", f.token, map[string]any{"catalog": manifest(12_000_000), "plan_only": true})
		assertCatalogUnknownField(t, status, raw, "plan_only")
		status, raw = requestJSON(t, http.MethodPatch, path+"/"+product.ID, f.token, map[string]any{"display_name": 7})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"invalid_param"`)
		require.Contains(t, string(raw), `"param":"display_name"`)
		require.NotContains(t, string(raw), "json:")
		status, raw = requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/publish", reader, map[string]any{"catalog": manifest(12_000_000), "insert": true})
		require.Equal(t, http.StatusForbidden, status, string(raw))
	})
	t.Run("mutation_classes", func(t *testing.T) {
		premium, err := f.client.Products.RetrieveByKey(ctx, "premium")
		require.NoError(t, err)
		group := "memberships"
		extra, err := f.client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "omitted", DisplayName: "Omitted", TierGroup: &group})
		require.NoError(t, err)
		extraPrice, err := f.client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: premium.ID, Key: "premium-oneoff", UnitAmount: 3_000_000, Currency: "USD"})
		require.NoError(t, err)
		yearly, err := f.client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: premium.ID, Key: "premium-yearly", UnitAmount: 100_000_000, Currency: "USD", AccessDurationHours: intPtr(365 * 24), AutoRenew: true, Archived: true})
		require.NoError(t, err)
		desired := manifest(12_000_000)
		desired.Products[0].DisplayName = "Overwrite applies"
		desired.Products[0].Prices = append(desired.Products[0].Prices, catalog.Price{Currency: "USD", UnitAmount: 100_000_000, Duration: "365d", AutoRenew: true})
		desired.Products = append(desired.Products, catalog.Product{Key: "new-plan", DisplayName: "New plan", TierGroup: group, TierRank: intPtr(2), Prices: []catalog.Price{{Currency: "USD", UnitAmount: 1_000_000, Duration: "30d", AutoRenew: true}}})
		planned := f.publish(t, desired, openrails.CatalogPublishRequest{})
		require.Nil(t, planned.Result)
		require.Equal(t, 1, countProductActions(planned.Plan, openrails.CatalogProductCreate))
		require.Equal(t, 1, countPriceActions(planned.Plan, openrails.CatalogPriceActivate))
		require.Equal(t, 1, countPriceActions(planned.Plan, openrails.CatalogPriceArchive))
		result := f.publish(t, desired, openrails.CatalogPublishRequest{Overwrite: true}).Result
		require.Equal(t, 1, result.ProductsUpdated)
		require.Equal(t, 1, result.PricesActivated)
		require.Zero(t, result.ProductsCreated)
		require.Zero(t, result.PricesArchived)
		require.Zero(t, result.ProductsArchived)
		_, err = f.client.Products.RetrieveByKey(ctx, "new-plan")
		require.ErrorIs(t, err, openrails.ErrNotFound)
		stillLive, err := f.client.Prices.Retrieve(ctx, extraPrice.ID)
		require.NoError(t, err)
		require.False(t, stillLive.Archived)
		activeYear, err := f.client.Prices.Retrieve(ctx, yearly.ID)
		require.NoError(t, err)
		require.False(t, activeYear.Archived)
		desired.Products[0].DisplayName = "Insert and prune cannot overwrite"
		result = f.publish(t, desired, openrails.CatalogPublishRequest{Prune: true}).Result
		require.Equal(t, 1, result.ProductsArchived)
		require.Equal(t, 1, result.PricesArchived)
		require.Zero(t, result.ProductsCreated)
		require.Zero(t, result.ProductsUpdated)
		retired, err := f.client.Products.Retrieve(ctx, extra.ID)
		require.NoError(t, err)
		require.True(t, retired.Archived)
		retiredPrice, err := f.client.Prices.Retrieve(ctx, extraPrice.ID)
		require.NoError(t, err)
		require.True(t, retiredPrice.Archived)
		_, err = f.client.Products.RetrieveByKey(ctx, "new-plan")
		require.ErrorIs(t, err, openrails.ErrNotFound)
		result = f.publish(t, desired, openrails.CatalogPublishRequest{Insert: true}).Result
		require.Equal(t, 1, result.ProductsCreated)
		require.Equal(t, 1, result.PricesCreated)
		require.Zero(t, result.ProductsUpdated)
		require.Zero(t, result.PricesActivated)
		require.Zero(t, result.PricesArchived)
		premium, err = f.client.Products.Retrieve(ctx, premium.ID)
		require.NoError(t, err)
		require.Equal(t, "Overwrite applies", premium.DisplayName)
	})

	t.Run("financial_matching", func(t *testing.T) {
		m := manifest(12_000_000)
		m.Products[0].Prices[0].PSPs = []string{"stripe", "nmi"}
		require.NoError(t, m.Validate())
		// Provider attachment is not price identity. No provider write is made
		// here: the real API supplies current rows to the production planner.
		plan := f.publish(t, m, openrails.CatalogPublishRequest{}).Plan
		require.Equal(t, 1, countPriceActions(plan, openrails.CatalogPriceUnchanged))
		require.Zero(t, countPriceActions(plan, openrails.CatalogPriceCreate))
	})

	t.Run("provider_choices_survive_read_only_plan", func(t *testing.T) {
		m := catalog.Manifest{Version: catalog.SupportedVersion, Products: []catalog.Product{{Key: "provider-plan", DisplayName: "Provider plan", TierGroup: "provider-plans", Prices: []catalog.Price{{Currency: "USD", UnitAmount: 23_000_000, Duration: "30d", AutoRenew: true, PSPs: []string{"stripe", "ccbill"}}}}}}
		plan := f.publish(t, m, openrails.CatalogPublishRequest{}).Plan
		require.Len(t, plan.Groups, 1)
		require.Len(t, plan.Groups[0].Products, 1)
		product := plan.Groups[0].Products[0]
		require.Equal(t, openrails.CatalogProductCreate, product.Action)
		require.Len(t, product.Prices, 1)
		require.Equal(t, openrails.CatalogPriceCreate, product.Prices[0].Action)
		require.Equal(t, []string{"stripe", "ccbill"}, product.Prices[0].CreateReq.PSPs)
		_, err := f.client.Products.RetrieveByKey(ctx, "provider-plan")
		require.ErrorIs(t, err, openrails.ErrNotFound)
	})

	t.Run("keys_and_trials", func(t *testing.T) {
		t.Run("collision", func(t *testing.T) {
			collision := catalog.Manifest{Version: catalog.SupportedVersion, Products: []catalog.Product{{Key: "ambiguous", DisplayName: "Ambiguous", Prices: []catalog.Price{
				{Currency: "USD", UnitAmount: 9_990_000, Duration: "30d", AutoRenew: true},
				{Currency: "USD", UnitAmount: 4_990_000, Duration: "30d", AutoRenew: true},
			}}}}
			status, raw := requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/publish", f.token, map[string]any{"catalog": collision, "insert": true})
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			require.Contains(t, string(raw), "ambiguous-monthly")
			require.Contains(t, string(raw), "disambiguate")
			var response struct {
				Error openrails.ErrorDetails `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &response))
			require.Equal(t, "invalid_param", response.Error.Code)
			require.NotNil(t, response.Error.Param)
			require.Equal(t, "key", *response.Error.Param)
			_, err := f.client.Products.RetrieveByKey(ctx, "ambiguous")
			require.ErrorIs(t, err, openrails.ErrNotFound, "colliding keys refuse before product creation")
		})

		m := catalog.Manifest{Version: catalog.SupportedVersion, Products: []catalog.Product{{Key: "intro", DisplayName: "Intro", Prices: []catalog.Price{
			{Key: "intro-step", Currency: "USD", UnitAmount: 14_950_000, Duration: "30d", AutoRenew: true, Trial: &catalog.PriceTrial{UnitAmount: 19_950_000, Duration: "30d"}},
			{Key: "intro-free", Currency: "USD", UnitAmount: 15_000_000, Duration: "30d", AutoRenew: true, Trial: &catalog.PriceTrial{UnitAmount: 0, Duration: "7d"}},
			{Key: "intro-flat", Currency: "USD", UnitAmount: 15_000_000, Duration: "30d", AutoRenew: true},
		}}}}
		result := f.publish(t, m, openrails.CatalogPublishRequest{Insert: true}).Result
		require.Equal(t, 3, result.PricesCreated, "trial terms distinguish otherwise identical recurring amounts")
		ids := map[string]openrails.PriceID{}
		for _, tc := range []struct {
			key           string
			amount, hours *int64
		}{
			{"intro-step", ptrI64(19_950_000), ptrI64(30 * 24)}, {"intro-free", ptrI64(0), ptrI64(7 * 24)}, {"intro-flat", nil, nil},
		} {
			price, err := f.client.Prices.RetrieveByKey(ctx, tc.key)
			require.NoError(t, err)
			ids[tc.key] = sdkPriceID(t, price.ID)
			require.Equal(t, tc.amount, price.TrialUnitAmount)
			if tc.hours == nil {
				require.Nil(t, price.TrialDurationHours)
			} else {
				require.NotNil(t, price.TrialDurationHours)
				require.EqualValues(t, *tc.hours, *price.TrialDurationHours)
			}
		}
		require.NotEqual(t, ids["intro-free"], ids["intro-flat"])
		m.Products[0].Prices[2].Key = "intro-renamed"
		planned := f.publish(t, m, openrails.CatalogPublishRequest{}).Plan
		require.Equal(t, 3, countPriceActions(planned, openrails.CatalogPriceUnchanged))
		require.True(t, planned.HasChanges(), "relabel is a change without new substance")
		f.publish(t, m, openrails.CatalogPublishRequest{Overwrite: true})
		renamed, err := f.client.Prices.RetrieveByKey(ctx, "intro-renamed")
		require.NoError(t, err)
		require.Equal(t, ids["intro-flat"], renamed.ID)
		_, err = f.client.Prices.RetrieveByKey(ctx, "intro-flat")
		require.ErrorIs(t, err, openrails.ErrNotFound)
		require.False(t, f.publish(t, m, openrails.CatalogPublishRequest{}).Plan.HasChanges())
	})
	t.Run("archived_declarations", func(t *testing.T) {
		m := catalog.Manifest{Version: catalog.SupportedVersion, Products: []catalog.Product{{Key: "historical", DisplayName: "Historical", TierGroup: "history", Archived: true, Prices: []catalog.Price{{Currency: "USD", UnitAmount: 1_000_000, Duration: "30d", AutoRenew: true, Archived: true}}}}}
		f.publish(t, m, openrails.CatalogPublishRequest{Insert: true})
		product, err := f.client.Products.RetrieveByKey(ctx, "historical")
		require.NoError(t, err)
		require.True(t, product.Archived)
		prices, err := f.client.Prices.List(ctx, &openrails.PriceListParams{ProductID: product.ID})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		price := prices.Items[0]
		require.True(t, price.Archived)
		require.False(t, f.publish(t, m, openrails.CatalogPublishRequest{}).Plan.HasChanges(), "archived price is matched, not recreated")
		m.Products[0].Archived = false
		m.Products[0].Prices[0].Archived = false
		result := f.publish(t, m, openrails.CatalogPublishRequest{Overwrite: true}).Result
		require.Equal(t, 1, result.ProductsUpdated)
		require.Equal(t, 1, result.PricesActivated)
		require.Zero(t, result.PricesCreated)
		active, err := f.client.Prices.RetrieveByKey(ctx, "historical-monthly")
		require.NoError(t, err)
		require.Equal(t, price.ID, active.ID)
		require.False(t, active.Archived)
	})

}

func assertCatalogUnknownField(t *testing.T, status int, raw []byte, field string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	var refusal struct {
		Error openrails.ErrorDetails `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &refusal))
	require.Equal(t, "invalid_request_error", refusal.Error.Type)
	require.Equal(t, "invalid_param", refusal.Error.Code)
	require.NotNil(t, refusal.Error.Param)
	require.Equal(t, field, *refusal.Error.Param)
	require.Equal(t, "unknown field "+field, refusal.Error.Message)
	require.NotEmpty(t, refusal.Error.RequestID)
}
