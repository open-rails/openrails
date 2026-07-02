//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// TestMeteredUsage_OverlappingCloses_RatedExactlyOnce proves the #672 invariant:
// threshold closes anchor `from` at the period start, so successive closes over
// one period rate OVERLAPPING prefixes ([from, mid1), [from, mid2), then the
// month-end [from, end)). Pre-#672 every close re-accrued the full prefix under
// a fresh window source_id — billing the same usage N times. With the rated-
// through watermark, the total accrued over any sequence of prefix closes must
// equal rating the period ONCE, and the watermark must be monotone (a repeated
// or stale close accrues nothing).
func TestMeteredUsage_OverlappingCloses_RatedExactlyOnce(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	productID := uuid.New()
	priceID := uuid.New()
	merchantID := dbtest.TestMerchantID.UUID()
	meterKey := "vm-seconds-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_price_metered WHERE price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE key = $1", meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)

	// Catalog: gauge meter (unit-seconds) + metered sidecar at 500_000 micros per
	// 3600s — rounding-relevant aggregates below prove rate-once semantics.
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $3, $4)`,
		productID, "metered-watermark-"+uuid.NewString(), "Metered Watermark Product", merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.prices (id, merchant_id, product_id, amount, currency, access_duration_hours, auto_renew, status) VALUES ($1, $2, $3, $4, $5, $6, true, 'active')`,
		priceID, merchantID, productID, int64(0), cur, 30)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.catalog_meters (merchant_id, key, kind) VALUES ($1, $2, 'gauge')`,
		merchantID, meterKey)
	require.NoError(t, err)
	rateMicros, perUnits, perSeconds := int64(500_000), int64(1), int64(3600)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.catalog_price_metered (price_id, merchant_id, meter_key, rate_micros, per_units, per_seconds) VALUES ($1, $2, $3, $4, $5, $6)`,
		priceID, merchantID, meterKey, rateMicros, perUnits, perSeconds)
	require.NoError(t, err)

	// Fixed period grid: all closes anchor at the same period start (the #672
	// overlapping-prefix shape produced by FinalizeThresholdInvoices +
	// FinalizeDueInvoicesForBoundary).
	t0 := time.Now().UTC()
	from := t0.Add(-2 * time.Hour)
	mid1 := t0.Add(-time.Hour)
	mid2 := t0
	end := t0.Add(time.Hour)

	report := func(unitSeconds int64, occurred time.Time) {
		_, err := svc.RecordUsage(ctx, money.RecordUsageParams{
			Payer:      &payer,
			Invoker:    "test-invoker",
			Currency:   cur,
			EventType:  meterKey,
			Dimensions: map[string]int64{meterKey: unitSeconds},
			Amount:     0,
			Source:     "test-usage",
			SourceID:   uuid.NewString(),
			OccurredAt: occurred,
		})
		require.NoError(t, err)
	}
	rate := func(aggregate int64) int64 {
		v, err := money.RateMeteredAggregate(aggregate, money.MeteredRate{
			Kind: money.MeterKindGauge, RateMicros: rateMicros, PerUnits: perUnits,
			Per: time.Duration(perSeconds) * time.Second,
		})
		require.NoError(t, err)
		return v
	}
	accrualTotals := func() (count int, total int64) {
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*), COALESCE(SUM(amount), 0)::bigint
			FROM openrails.ledger_transfers
			WHERE merchant_id = $1 AND customer_id = $2
			  AND transfer_type = 'owed_accrual' AND source = $3`,
			merchantID, payer.UUID(), "metered:"+meterKey).Scan(&count, &total))
		return count, total
	}

	// Threshold close #1 mid-period: [from, mid1) covers only the first report.
	report(1000, t0.Add(-90*time.Minute))
	_, err = svc.FinalizeInvoice(ctx, payer, cur, from, mid1)
	require.NoError(t, err)
	n, total := accrualTotals()
	require.Equal(t, 1, n)
	require.Equal(t, rate(1000), total)

	// Threshold close #2 over the LONGER prefix [from, mid2): pre-#672 this
	// re-accrued the full prefix; now it accrues only the unbilled delta.
	report(1000, t0.Add(-30*time.Minute))
	_, err = svc.FinalizeInvoice(ctx, payer, cur, from, mid2)
	require.NoError(t, err)
	n, total = accrualTotals()
	require.Equal(t, 2, n)
	require.Equal(t, rate(2000), total, "two overlapping closes must accrue exactly rate-once of the prefix")

	// Month-end full-period close [from, end): everything already billed — no
	// third accrual (pre-#672 this was the third full re-bill).
	_, err = svc.FinalizeInvoice(ctx, payer, cur, from, end)
	require.NoError(t, err)
	n, total = accrualTotals()
	require.Equal(t, 2, n)
	require.Equal(t, rate(2000), total, "month-end finalize over the same period must accrue nothing new")

	// Stale/out-of-order re-close of the shorter window: watermark is monotone.
	_, err = svc.FinalizeInvoice(ctx, payer, cur, from, mid2)
	require.NoError(t, err)
	n, total = accrualTotals()
	require.Equal(t, 2, n)
	require.Equal(t, rate(2000), total)

	// Watermark row reflects the cumulative accrued amount and the furthest close.
	var accrued int64
	var ratedThrough time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT accrued_amount, rated_through
		FROM openrails.metered_rating_watermarks
		WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3 AND source = $4 AND period_from = $5`,
		merchantID, payer.UUID(), cur, "metered:"+meterKey, from).Scan(&accrued, &ratedThrough))
	require.Equal(t, rate(2000), accrued)
	// Postgres stores microseconds; compare with tolerance. Must still point at
	// the FURTHEST close (stale re-close of mid2 cannot roll it back).
	require.WithinDuration(t, end, ratedThrough, time.Millisecond,
		"rated_through must be monotone (stale re-close cannot roll it back)")

	// The money conclusion: total outstanding equals rating the period once.
	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, rate(2000), owed)
}
