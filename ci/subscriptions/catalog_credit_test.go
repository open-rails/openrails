//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestCatalogCreditEvolution(t *testing.T) {
	w := prepareWorld(t, 12)
	w.start()
	key := "deposit-" + uuid.NewString()[:8]
	apply := func(raw string) *billing.CatalogApplicationReceipt {
		t.Helper()
		doc, err := catalog.ParseApplicationYAML([]byte(raw))
		require.NoError(t, err)
		receipt, err := w.client[remote].ApplyCatalog(t.Context(), doc)
		require.NoError(t, err)
		return receipt
	}
	initial := fmt.Sprintf(`schema_version: 1
products:
  %s:
    display_name: API deposit
    credit_grant: {currency: usd, from_payment: true}
    prices:
      deposit:
        currency: USD
        unit_amount: 0
        customer_amount: {min_amount: 1000000, max_amount: 100000000}
`, key)
	apply(initial)
	p, err := productByKey(t.Context(), w.client[embedded], key)
	require.NoError(t, err)
	require.Equal(t, "USD", p.CreditGrant.Currency)
	require.Equal(t, 365, *p.CreditGrant.ExpiresAfterDays)
	first, err := priceByKey(t.Context(), w.client[embedded], key, "deposit")
	require.NoError(t, err)
	require.EqualValues(t, 0, first.Revision)
	apply(fmt.Sprintf(`schema_version: 1
products:
  %s:
    prices:
      deposit:
        customer_amount: {min_amount: 1000000, max_amount: 500000000}
`, key))
	second, err := priceByKey(t.Context(), w.client[embedded], key, "deposit")
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	require.EqualValues(t, 1, second.Revision)
	old, err := w.client[embedded].GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.True(t, old.Archived)
	require.EqualValues(t, 100000000, old.CustomerAmount.MaxAmount)
	require.True(t, apply(initial).Replayed)
	current, err := priceByKey(t.Context(), w.client[embedded], key, "deposit")
	require.NoError(t, err)
	require.Equal(t, second.ID, current.ID)
	apply(fmt.Sprintf(`schema_version: 1
products:
  %s:
    credit_grant: {currency: USD, from_payment: true, expires_after_days: 30}
    prices:
      deposit:
        customer_amount: {min_amount: 1000000, max_amount: 100000000}
`, key))
	restored, err := priceByKey(t.Context(), w.client[embedded], key, "deposit")
	require.NoError(t, err)
	require.Equal(t, first.ID, restored.ID)
	require.EqualValues(t, 0, restored.Revision)
	for _, tp := range []topology{embedded, remote} {
		_, err := w.client[tp].UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{CreditGrant: catalog.Null[catalog.CreditGrantSpec]()})
		require.Error(t, err)
		_, err = w.client[tp].CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: p.ID, Key: "bad", Currency: "USD", UnitAmount: 1000000, BillingIntervalHours: ptrCreditHours(720), AccessDurationHours: ptrCreditHours(720)})
		require.Error(t, err)
	}
	// Partial batches may archive the incompatible offer and change the product
	// together. History never prevents a future definition, but restore validates it.
	apply(fmt.Sprintf(`schema_version: 1
products:
  %s:
    credit_grant: null
    prices:
      deposit:
        archived: true
`, key))
	_, err = w.client[remote].UpdatePrice(t.Context(), first.ID, billing.UpdatePriceParams{Archived: catalog.Value(false)})
	require.Error(t, err, "a restored deposit must still have a funding benefit")
	p, err = w.client[embedded].GetProduct(t.Context(), p.ID)
	require.NoError(t, err)
	require.Nil(t, p.CreditGrant)
	_, err = w.client[remote].UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{CreditGrant: catalog.Value(catalog.CreditGrantSpec{Currency: "USD", FromPayment: true})})
	require.NoError(t, err)
	_, err = w.client[remote].UpdatePrice(t.Context(), first.ID, billing.UpdatePriceParams{Archived: catalog.Value(false)})
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.sql(`UPDATE billing.prices SET customer_amount='{"min_amount":"2000000","max_amount":"100000000"}'::jsonb WHERE id=$1`), first.ID.UUID())
	require.Error(t, err, "deposit bounds are immutable in the database")
	for _, bounds := range []string{`{"min_amount":null,"max_amount":"100000000"}`, `{"min_amount":"1000000","max_amount":null}`} {
		_, err = w.pool.Exec(t.Context(), w.sql(`INSERT INTO billing.prices(id,merchant_id,product_id,key,amount,currency,billing_interval_hours,customer_amount)
   SELECT gen_random_uuid(),merchant_id,product_id,'invalid',0,currency,NULL,$2::jsonb FROM billing.prices WHERE id=$1`), first.ID.UUID(), bounds)
		require.Error(t, err, "null bounds must fail a database check")
	}
}
func ptrCreditHours(v int) *int { return &v }
