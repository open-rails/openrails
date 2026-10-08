//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/stretchr/testify/require"
)

// A dump is a reusable declaration of current offers. In particular, prepaid
// products must not become empty ownership products when copied to a merchant.
func TestCatalogYAMLDumpRoundTrip(t *testing.T) {
	source := newWorld(t)
	original, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
meters:
- key: api-calls
  event_type: api.call
  aggregation: count
  unit: calls
  group_by: {model: model}
products:
- key: premium
  display_name: Premium
  description: Feature access
  entitlements_spec: {premium: null, welcome: 72}
  tier_group: membership
  tier_rank: 2
  prices:
  - key: monthly
    currency: USD
    unit_amount: 9990000
    auto_renew: true
    access_duration_hours: 720
    trial_unit_amount: 0
    trial_duration_hours: 24
  rate_cards:
  - ordinal: 3
    meter: api-calls
    payment_term: in_arrears
    filter: {model: [fast]}
    allowance: {included: 10}
    price:
      model: per_unit
      currency: USD
      per_unit: {unit_amount: "1000", divide_by: 2, round: up, maximum_amount: "1000000"}
- key: api-pack
  display_name: $100 prepaid balance
  credit_grant: {currency: USD, amount: 100000000, expires_after_days: 90}
  prices:
  - key: buy
    currency: USD
    unit_amount: 80000000
- key: api-deposit
  display_name: Prepaid deposit
  credit_grant: {currency: USD, from_payment: true}
  prices:
  - key: deposit
    currency: USD
    unit_amount: 0
    customer_amount: {min_amount: 1000000, max_amount: 1000000000}
- key: video
  display_name: Video
  prices:
  - key: buy
    currency: USD
    unit_amount: 4990000
  - key: rent
    currency: USD
    unit_amount: 1990000
    access_duration_hours: 72
- key: retired
  display_name: Retired product
  archived: true
- key: old-offer
  display_name: No current offers
  prices:
  - key: retired
    currency: USD
    unit_amount: 1000000
    archived: true
`))
	require.NoError(t, err)
	_, err = source.client[embedded].ApplyCatalog(t.Context(), original)
	require.NoError(t, err)
	pack, err := source.client[embedded].GetProductByKey(t.Context(), "api-pack")
	require.NoError(t, err)
	currentPack, err := source.client[embedded].CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID: pack.ID, Key: "buy", Currency: "USD", UnitAmount: 90000000,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, currentPack.Revision)

	dump := func(w *world) ([]byte, *catalog.Application) {
		t.Helper()
		var out bytes.Buffer
		require.NoError(t, hosttools.DumpMerchantCatalog(t.Context(), hosttools.CatalogDumpOptions{
			Config: &config.Config{Schema: w.schema}, PGXPool: w.pool, Merchant: w.slug, Out: &out,
		}))
		doc, err := catalog.ParseApplicationYAML(out.Bytes())
		require.NoError(t, err)
		return out.Bytes(), doc
	}
	raw, exported := dump(source)
	require.False(t, exported.Prune)
	products := map[string]catalog.ApplyProduct{}
	for _, product := range exported.Products {
		products[product.Key] = product
		for _, price := range product.Prices {
			require.Empty(t, price.ID, "local IDs cannot pin an empty destination")
			require.False(t, price.Archived.Value)
			require.True(t, price.CustomerAmount.Set, "explicit null clears optional terms")
		}
		require.True(t, product.CreditGrant.Set)
	}
	require.NotContains(t, products, "retired", "this command exports active offers, not archived history")
	require.Empty(t, products["old-offer"].Prices)
	require.EqualValues(t, 100000000, *products["api-pack"].CreditGrant.Value.Amount)
	require.Equal(t, 90, *products["api-pack"].CreditGrant.Value.ExpiresAfterDays)
	require.Len(t, products["api-pack"].Prices, 1, "the archived revision is not a current offer")
	require.EqualValues(t, 90000000, products["api-pack"].Prices[0].UnitAmount.Value)
	require.True(t, products["api-deposit"].CreditGrant.Value.FromPayment)
	require.Equal(t, 365, *products["api-deposit"].CreditGrant.Value.ExpiresAfterDays)
	require.EqualValues(t, 1000000000, products["api-deposit"].Prices[0].CustomerAmount.Value.MaxAmount)
	require.True(t, products["video"].CreditGrant.Null)
	require.Len(t, exported.Meters, 1)
	require.Len(t, products["premium"].RateCards.Value, 1)

	target := newWorld(t)
	receipt, err := target.client[remote].ApplyCatalog(t.Context(), exported)
	require.NoError(t, err)
	require.False(t, receipt.Replayed)
	restoredRaw, _ := dump(target)
	require.Equal(t, string(raw), string(restoredRaw), "all exported declarations survive an empty-merchant round trip")
	restoredPack, err := target.client[embedded].GetPriceByKey(t.Context(), "api-pack", "buy")
	require.NoError(t, err)
	require.NotEqual(t, currentPack.ID, restoredPack.ID, "semantic copy uses destination identities")
	require.EqualValues(t, 0, restoredPack.Revision, "full historical revision numbers require the billing archive")

	// Content-addressed apply is a one-time batch, not a rollback command.
	product, err := target.client[embedded].GetProductByKey(t.Context(), "premium")
	require.NoError(t, err)
	_, err = target.client[embedded].UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Later edit")})
	require.NoError(t, err)
	replay, err := target.client[remote].ApplyCatalog(t.Context(), exported)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	product, err = target.client[embedded].GetProductByKey(t.Context(), "premium")
	require.NoError(t, err)
	require.Equal(t, "Later edit", product.DisplayName)
}
