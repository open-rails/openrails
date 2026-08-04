//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

// TestFinalizeInvoice_RatesReportedUsageFromCatalogSidecar proves the #615/#707
// bridge: usage reported via RecordUsage (openrails.usage_events) is rated
// through the catalog rate card for a legacy-shaped gauge meter and lands on
// the finalized invoice. FinalizeInvoice runs the usage->aggregate->owed sweep,
// so reported VM-seconds become real AmountDue. Re-finalizing the same window
// must NOT double-accrue (idempotent on the deterministic period source_id).
func TestFinalizeInvoice_RatesReportedUsageFromCatalogSidecar(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	productID := uuid.New()
	merchantID := dbtest.TestMerchantID.UUID()
	meterKey := "vm-seconds-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE key = $1", meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Arrears so rated usage becomes an open receivable (owed accrual -> invoice).
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)

	// Publish a metered product + legacy gauge meter + the rate card the #707
	// manifest translation produces for `metered: {rate: 500_000, per: 1h}`:
	// per_unit unit_amount 500_000, divide_by 3600 (per_units 1 x 3600s).
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $3, $4)`,
		productID, "metered-bridge-"+uuid.NewString(), "Metered Bridge Product", merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.catalog_meters (merchant_id, key, kind) VALUES ($1, $2, 'gauge')`,
		merchantID, meterKey)
	require.NoError(t, err)
	rateMicros := int64(500_000)
	divideBy := int64(3600)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES ($1, $2, 1, $3, 'in_arrears', jsonb_build_object(
    'model', 'per_unit',
    'currency', 'USD',
    'per_unit', jsonb_build_object('unit_amount', $4::bigint, 'divide_by', $5::bigint)))`,
		merchantID, productID, meterKey, rateMicros, divideBy)
	require.NoError(t, err)

	// Window around now; report usage at occurred_at inside it.
	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	occurred := time.Now()

	// Report several gauge usage events with the meter_key as event_type AND as the
	// dimension key carrying unit-seconds. Amount=0: metered pricing comes from the
	// catalog, not the host-priced RecordUsage amount.
	unitSeconds := []int64{1800, 3600, 5400} // sums to 10800 unit-seconds (3 VM-hours)
	var totalUnitSeconds int64
	for i, us := range unitSeconds {
		totalUnitSeconds += us
		_, err := svc.RecordUsage(ctx, money.RecordUsageParams{
			Payer:      &payer,
			Invoker:    "test-invoker",
			Currency:   cur,
			EventType:  meterKey,
			Dimensions: map[string]int64{meterKey: us},
			Amount:     0,
			Key:        money.MustIdempotencyKey(money.UsageOperation(meterKey), "test-usage", uuid.NewString()),
			OccurredAt: occurred.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
	}

	// Expected rated amount: rate once over the full period aggregate.
	expected, err := pricing.ChargeModel{Kind: pricing.ModelPerUnit, UnitAmount: rateMicros, DivideBy: divideBy}.Rate(totalUnitSeconds)
	require.NoError(t, err)
	require.Greater(t, expected, int64(0), "expected a non-zero rated amount")

	// Finalize: the sweep must rate reported usage into the invoice.
	inv, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, expected, inv.AmountDue, "invoice AmountDue must equal reported usage x catalog rate")

	// An invoiced owed_accrual item must be attached to the invoice at the
	// deterministic period source_id.
	periodSourceID := money.MeteredPeriodSourceIDForTest(from, to)
	var invoicedCount int
	var invoicedAmount int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0)::bigint
		FROM openrails.invoice_items
		WHERE customer_id = $1
		  AND invoice_id = $2
		  AND source_type = 'owed_accrual'
		  AND status = 'invoiced'
		  AND source_id = $3
	`, payer.UUID(), inv.ID, string(money.OpMeteredRating)+":metered:"+meterKey+":"+periodSourceID).Scan(&invoicedCount, &invoicedAmount))
	require.Equal(t, 1, invoicedCount)
	require.Equal(t, expected, invoicedAmount)

	// Re-finalize the SAME window: idempotent, no double-accrue.
	inv2, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)
	require.Equal(t, inv.ID, inv2.ID)
	require.Equal(t, expected, inv2.AmountDue, "re-finalize must not double-accrue")

	// Still exactly one owed_accrual row for the period.
	var afterCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_items
		WHERE customer_id = $1
		  AND source_type = 'owed_accrual'
		  AND source_id = $2
	`, payer.UUID(), string(money.OpMeteredRating)+":metered:"+meterKey+":"+periodSourceID).Scan(&afterCount))
	require.Equal(t, 1, afterCount)
}
