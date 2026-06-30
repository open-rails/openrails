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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND product_id = ANY($2::uuid[])", merchantID, []uuid.UUID{dropletProductID, bandwidthProductID})
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = ANY($2::text[])", merchantID, []string{dropletMeter, bandwidthMeter})
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = ANY($1::uuid[])", []uuid.UUID{dropletProductID, bandwidthProductID})
	})

	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
INSERT INTO openrails.products (id, key, display_name, merchant_id)
VALUES ($1, 'droplet-runtime', 'Droplet Runtime', $3),
       ($2, 'bandwidth-transfer', 'Bandwidth Transfer', $3)`, dropletProductID, bandwidthProductID, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_meters (merchant_id, key, event_type, value_property, aggregation, unit, group_by)
VALUES ($1, $2, 'droplet.usage', 'seconds', 'sum', 'second', '{"size_slug":"metadata.size_slug","resource_id":"metadata.resource_id"}'::jsonb),
       ($1, $3, 'bandwidth.transfer', 'bytes', 'sum', 'byte', '{}'::jsonb)`,
		merchantID, dropletMeter, bandwidthMeter)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES
    ($1, $2, 1, $4, 'in_arrears', '{
      "model":"per_unit",
      "currency":"USD",
      "per_unit":{"divide_by":3600,"matrix":{"dimension":"size_slug","cells":{"s-1vcpu-1gb":{"unit_amount":8930,"maximum_amount":6000000,"included":1}}}}
    }'::jsonb),
    ($1, $3, 1, $5, 'in_arrears', '{
      "model":"per_unit",
      "currency":"USD",
      "per_unit":{"unit_amount":10000,"divide_by":1073741824}
    }'::jsonb)`,
		merchantID, dropletProductID, bandwidthProductID, dropletMeter, bandwidthMeter)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
UPDATE openrails.catalog_rate_cards
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
		Metadata:   map[string]any{"size_slug": "s-1vcpu-1gb", "resource_id": "droplet-1"},
		Amount:     0,
		Source:     "ratecard-test",
		SourceID:   uuid.NewString(),
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
		Source:     "ratecard-test",
		SourceID:   uuid.NewString(),
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
FROM openrails.invoice_items
WHERE customer_id = $1
  AND invoice_id = $2
  AND status = 'invoiced'
  AND source_id LIKE 'metered:%:rate_card:%'`, payer.UUID(), inv.ID).Scan(&itemCount, &itemTotal))
	require.Equal(t, 2, itemCount)
	require.Equal(t, inv.AmountDue, itemTotal)
}

func TestCatalogCreditPurchase_QuotesBonusCreditsAndDepositsLedgerBalance(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	productKey := "image-credit-topup-" + uuid.NewString()
	unit := dbtest.TestMerchantSlug + "/image-credit"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.ledger_transfers WHERE customer_id = $1 AND currency = $2", payer.UUID(), unit)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE customer_id = $1 AND currency = $2", payer.UUID(), unit)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_purchase_prices WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_balances WHERE merchant_id = $1 AND key = $2", merchantID, "image-credit")
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := svc.DefineCustomCreditType(ctx, "image-credit", 0)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.products (id, key, display_name, merchant_id)
VALUES ($1, $2, 'Image Credit Top-up', $3)`, productID, productKey, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_credit_balances (merchant_id, key, unit, expires_hours)
VALUES ($1, 'image-credit', 'image-credit', 720)
ON CONFLICT (merchant_id, key) DO UPDATE SET unit = EXCLUDED.unit, expires_hours = EXCLUDED.expires_hours`, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_credit_purchase_prices
    (merchant_id, product_id, ordinal, credit_key, currency, input_min, price)
VALUES ($1, $2, 1, 'image-credit', 'USD', 1000000, '{
  "model":"tiered",
  "tiered":{
    "mode":"graduated",
    "tiers":[
      {"up_to":2000,"unit_amount":10000},
      {"unit_amount":7500}
    ]
  }
}'::jsonb)`, merchantID, productID)
	require.NoError(t, err)

	quote, err := svc.QuoteCatalogCreditPurchase(ctx, money.CatalogCreditPurchaseQuoteInput{
		ProductKey:  productKey,
		SpendMicros: 50_000_000,
	})
	require.NoError(t, err)
	require.Equal(t, int64(50_000_000), quote.PaidAmount)
	require.Equal(t, int64(6_000), quote.TotalCredits)
	require.Equal(t, int64(5_000), quote.BaseCredits)
	require.Equal(t, int64(1_000), quote.BonusCredits)
	require.Equal(t, unit, quote.Unit)
	require.NotNil(t, quote.ExpiresAt)

	quote, trx, err := svc.DepositCatalogCreditPurchase(ctx, payer, payer.UUID().String(), money.CatalogCreditPurchaseQuoteInput{
		ProductKey:  productKey,
		SpendMicros: 50_000_000,
	}, "checkout_"+uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, quote.TotalCredits, trx.Amount)
	require.Equal(t, unit, trx.Currency)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, unit)
	require.NoError(t, err)
	require.Equal(t, quote.TotalCredits, bal.Balance)

	_, _, err = svc.DepositCatalogCreditPurchase(ctx, payer, payer.UUID().String(), money.CatalogCreditPurchaseQuoteInput{
		ProductKey:  productKey,
		SpendMicros: 50_000_000,
	}, derefString(trx.SourceID))
	require.NoError(t, err)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, unit)
	require.NoError(t, err)
	require.Equal(t, quote.TotalCredits, bal.Balance)
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, 'droplet-self-allow', 'Droplet', $2)`, productID, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_meters (merchant_id, key, event_type, value_property, aggregation, group_by)
VALUES ($1, $2, 'droplet.usage', 'seconds', 'sum', '{"size_slug":"metadata.size_slug","resource_id":"metadata.resource_id"}'::jsonb)`, merchantID, meterKey)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES ($1, $2, 1, $3, 'in_arrears', '{
  "model":"per_unit","currency":"USD",
  "per_unit":{"divide_by":3600,"matrix":{"dimension":"size_slug","cells":{"s-1vcpu-1gb":{"unit_amount":8930,"maximum_amount":6000000,"included":1000}}}}
}'::jsonb)`, merchantID, productID, meterKey)
	require.NoError(t, err)

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: payer.UUID().String(), Currency: cur,
		EventType:  "droplet.usage",
		Dimensions: map[string]int64{"seconds": 100 * 3600}, // 100h: under the cap AND under cell.included
		Metadata:   map[string]any{"size_slug": "s-1vcpu-1gb", "resource_id": "droplet-1"},
		Source:     "ratecard-selfallow", SourceID: uuid.NewString(), OccurredAt: time.Now(),
	})
	require.NoError(t, err)

	inv, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(893_000), inv.AmountDue) // 100h * $0.00893; cell.included must NOT zero it
}

// input_min/input_max are SPEND bounds in micros. A credits-entry quote must
// bound the computed charge, not compare the raw credit count to a micros bound.
func TestCatalogCreditPurchase_QuotesByCreditsEntryWithinSpendBounds(t *testing.T) {
	svc, pool, _, _, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	productKey := "image-credit-topup-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_purchase_prices WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_balances WHERE merchant_id = $1 AND key = $2", merchantID, "image-credit")
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	_, err := svc.DefineCustomCreditType(ctx, "image-credit", 0)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Topup', $3)`, productID, productKey, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_credit_balances (merchant_id, key, unit)
VALUES ($1, 'image-credit', 'image-credit')
ON CONFLICT (merchant_id, key) DO UPDATE SET unit = EXCLUDED.unit`, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_credit_purchase_prices (merchant_id, product_id, ordinal, credit_key, currency, input_min, price)
VALUES ($1, $2, 1, 'image-credit', 'USD', 1000000, '{
  "model":"tiered","tiered":{"mode":"graduated","tiers":[{"up_to":2000,"unit_amount":10000},{"unit_amount":7500}]}
}'::jsonb)`, merchantID, productID)
	require.NoError(t, err)

	// 6000 credits as a raw number is < input_min(1_000_000), but the $50 charge
	// is within bounds: the quote must succeed (regression guard for the unit mix-up).
	quote, err := svc.QuoteCatalogCreditPurchase(ctx, money.CatalogCreditPurchaseQuoteInput{
		ProductKey: productKey,
		Credits:    6000,
	})
	require.NoError(t, err)
	require.Equal(t, int64(6000), quote.TotalCredits)
	require.Equal(t, int64(50_000_000), quote.PaidAmount)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
