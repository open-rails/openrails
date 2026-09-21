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

func TestFinalizeInvoice_RatesCatalogRateCardsWithMatrixCapAndAllowance(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	dropletProductID := uuid.New()
	bandwidthProductID := uuid.New()
	dropletMeter := "droplet-seconds-" + uuid.NewString()
	bandwidthMeter := "bandwidth-bytes-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_rate_cards WHERE merchant_id = $1 AND product_id = ANY($2::uuid[])", merchantID, []uuid.UUID{dropletProductID, bandwidthProductID})
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_meters WHERE merchant_id = $1 AND key = ANY($2::text[])", merchantID, []string{dropletMeter, bandwidthMeter})
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = ANY($1::uuid[])", []uuid.UUID{dropletProductID, bandwidthProductID})
	})

	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
INSERT INTO billing.products (id, key, display_name, merchant_id)
VALUES ($1, 'droplet-runtime', 'Droplet Runtime', $3),
       ($2, 'bandwidth-transfer', 'Bandwidth Transfer', $3)`, dropletProductID, bandwidthProductID, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO billing.catalog_meters (merchant_id, key, event_type, value_property, aggregation, unit, group_by)
VALUES ($1, $2, 'droplet.usage', 'seconds', 'sum', 'second', '{"size_slug":"metadata.size_slug","resource_id":"metadata.resource_id","region":"metadata.region"}'::jsonb),
       ($1, $3, 'bandwidth.transfer', 'bytes', 'sum', 'byte', '{}'::jsonb)`,
		merchantID, dropletMeter, bandwidthMeter)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO billing.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, filter, price)
VALUES
    ($1, $2, 1, $4, 'in_arrears', '{"region":["eu"]}'::jsonb, '{
      "model":"per_unit",
      "currency":"USD",
      "per_unit":{"divide_by":3600,"matrix":{"dimension":"size_slug","cells":{"s-1vcpu-1gb":{"unit_amount":"8930","maximum_amount":"6000000","included":1}}}}
    }'::jsonb),
    ($1, $3, 1, $5, 'in_arrears', '{}'::jsonb, '{
      "model":"per_unit",
      "currency":"USD",
      "per_unit":{"unit_amount":"10000","divide_by":1073741824}
    }'::jsonb)`,
		merchantID, dropletProductID, bandwidthProductID, dropletMeter, bandwidthMeter)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
UPDATE billing.catalog_rate_cards
SET allowance = jsonb_build_object(
	'accrue_from', $3::text,
	'cap', '28d',
	'pool', 'customer'
)
WHERE merchant_id = $1 AND product_id = $2`, merchantID, bandwidthProductID, dropletMeter)
	require.NoError(t, err)

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	occurred := time.Now()

	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer:      &payer,
		Invoker:    payer.UUID().String(),
		Currency:   cur,
		EventType:  "droplet.usage",
		Dimensions: map[string]int64{"seconds": 10_000 * 3600},
		Metadata:   map[string]any{"size_slug": "s-1vcpu-1gb", "resource_id": "droplet-1", "region": "eu"},
		Amount:     0,
		Key:        money.MustIdempotencyKey(money.UsageOperation("droplet.usage"), "ratecard-test", uuid.NewString()),
		OccurredAt: occurred,
	})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer:      &payer,
		Invoker:    payer.UUID().String(),
		Currency:   cur,
		EventType:  "droplet.usage",
		Dimensions: map[string]int64{"seconds": 10_000 * 3600},
		Metadata:   map[string]any{"size_slug": "s-1vcpu-1gb", "resource_id": "droplet-filtered", "region": "us"},
		Amount:     0,
		Key:        money.MustIdempotencyKey(money.UsageOperation("droplet.usage"), "ratecard-test", uuid.NewString()),
		OccurredAt: occurred,
	})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer:      &payer,
		Invoker:    payer.UUID().String(),
		Currency:   cur,
		EventType:  "bandwidth.transfer",
		Dimensions: map[string]int64{"bytes": 3 * 1024 * 1024 * 1024},
		Amount:     0,
		Key:        money.MustIdempotencyKey(money.UsageOperation("bandwidth.transfer"), "ratecard-test", uuid.NewString()),
		OccurredAt: occurred,
	})
	require.NoError(t, err)

	inv, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(6_020_000), inv.AmountDue)

	var itemCount int
	var itemTotal int64
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*), COALESCE(sum(amount), 0)::bigint
FROM billing.invoice_items
WHERE customer_id = $1
  AND invoice_id = $2
  AND status = 'invoiced'
  AND source_id LIKE 'metered_rating:metered:%:period:%'`, payer.UUID(), inv.ID).Scan(&itemCount, &itemTotal))
	require.Equal(t, 2, itemCount)
	require.Equal(t, inv.AmountDue, itemTotal)
}

// A matrix cell's `included` is the source value other cards' allowances draw
// from (the droplet plan's included transfer), NOT an allowance against the
// card's own runtime. A droplet that runs fewer hours than its `included` value
// must still bill its runtime — not be zeroed out.
func TestFinalizeInvoice_MatrixCellIncludedIsNotSelfAllowance(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	meterKey := "droplet-seconds-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_rate_cards WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO billing.products (id, key, display_name, merchant_id) VALUES ($1, 'droplet-self-allow', 'Droplet', $2)`, productID, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO billing.catalog_meters (merchant_id, key, event_type, value_property, aggregation, group_by)
VALUES ($1, $2, 'droplet.usage', 'seconds', 'sum', '{"size_slug":"metadata.size_slug","resource_id":"metadata.resource_id"}'::jsonb)`, merchantID, meterKey)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO billing.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES ($1, $2, 1, $3, 'in_arrears', '{
  "model":"per_unit","currency":"USD",
  "per_unit":{"divide_by":3600,"matrix":{"dimension":"size_slug","cells":{"s-1vcpu-1gb":{"unit_amount":"8930","maximum_amount":"6000000","included":1000}}}}
}'::jsonb)`, merchantID, productID, meterKey)
	require.NoError(t, err)

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: payer.UUID().String(), Currency: cur,
		EventType:  "droplet.usage",
		Dimensions: map[string]int64{"seconds": 100 * 3600}, // 100h: under the cap AND under cell.included
		Metadata:   map[string]any{"size_slug": "s-1vcpu-1gb", "resource_id": "droplet-1"},
		Key:        money.MustIdempotencyKey(money.UsageOperation("droplet.usage"), "ratecard-selfallow", uuid.NewString()), OccurredAt: time.Now(),
	})
	require.NoError(t, err)

	inv, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(893_000), inv.AmountDue) // 100h * $0.00893; cell.included must NOT zero it
}
