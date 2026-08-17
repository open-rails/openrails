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
VALUES ($1, $2, 'droplet.usage', 'seconds', 'sum', 'second', '{"size_slug":"metadata.size_slug","resource_id":"metadata.resource_id","region":"metadata.region"}'::jsonb),
       ($1, $3, 'bandwidth.transfer', 'bytes', 'sum', 'byte', '{}'::jsonb)`,
		merchantID, dropletMeter, bandwidthMeter)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, filter, price)
VALUES
    ($1, $2, 1, $4, 'in_arrears', '{"region":["eu"]}'::jsonb, '{
      "model":"per_unit",
      "currency":"USD",
      "per_unit":{"divide_by":3600,"matrix":{"dimension":"size_slug","cells":{"s-1vcpu-1gb":{"unit_amount":8930,"maximum_amount":6000000,"included":1}}}}
    }'::jsonb),
    ($1, $3, 1, $5, 'in_arrears', '{}'::jsonb, '{
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
FROM openrails.invoice_items
WHERE customer_id = $1
  AND invoice_id = $2
  AND status = 'invoiced'
  AND source_id LIKE 'metered_rating:metered:%:period:%'`, payer.UUID(), inv.ID).Scan(&itemCount, &itemTotal))
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

	// The catalog sidecar push is the custom_credit_types writer (#706); this
	// test seeds sidecar rows directly, so seed the type row the same way.
	_, err := pool.Exec(ctx, `
INSERT INTO openrails.custom_credit_types (id, merchant_id, name, decimals, active)
VALUES (uuidv7(), $1, 'image-credit', 0, true)
ON CONFLICT (merchant_id, name) DO UPDATE SET active = true`, merchantID)
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

	// or#896: delivery is the LIVE deposit path (the one POST
	// /v1/service/credits/deposit drives) fed by the quote — there is no second
	// catalog-specific money writer.
	sourceID := "checkout_" + uuid.NewString()
	trx, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer,
		Invoker:    payer.UUID().String(),
		Currency:   quote.Unit,
		Amount:     quote.TotalCredits,
		Source:     "credit_purchase",
		SourceID:   &sourceID,
		ExpiresAt:  quote.ExpiresAt,
	})
	require.NoError(t, err)
	require.Equal(t, quote.TotalCredits, trx.Amount)
	require.Equal(t, unit, trx.Currency)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, unit)
	require.NoError(t, err)
	require.Equal(t, quote.TotalCredits, bal.Balance)

	// Same payment source id = same deposit: a replayed settlement never
	// double-credits the quoted lot.
	_, err = svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer,
		Invoker:    payer.UUID().String(),
		Currency:   quote.Unit,
		Amount:     quote.TotalCredits,
		Source:     "credit_purchase",
		SourceID:   trx.SourceID,
		ExpiresAt:  quote.ExpiresAt,
	})
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
		Key:        money.MustIdempotencyKey(money.UsageOperation("droplet.usage"), "ratecard-selfallow", uuid.NewString()), OccurredAt: time.Now(),
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
	_, err := pool.Exec(ctx, `
INSERT INTO openrails.custom_credit_types (id, merchant_id, name, decimals, active)
VALUES (uuidv7(), $1, 'image-credit', 0, true)
ON CONFLICT (merchant_id, name) DO UPDATE SET active = true`, merchantID)
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

// or#823 money-boundary pin: the ONE rounding knob a credit purchase actually
// reads is per_unit.round, carried in the offer's `price` jsonb. Two offers
// differing in nothing else must quote different micros for the same credits,
// at the exact values below — otherwise the mode is decorative and the doctrine
// that a declared knob is read has no proof. (The retired top-level
// catalog_credit_purchase_prices.round column is dropped in 0002; it was never
// selected here.)
func TestCatalogCreditPurchase_PerUnitRoundModeChangesTheQuote(t *testing.T) {
	svc, pool, _, _, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()

	_, err := pool.Exec(ctx, `
INSERT INTO openrails.custom_credit_types (id, merchant_id, name, decimals, active)
VALUES (uuidv7(), $1, 'image-credit', 0, true)
ON CONFLICT (merchant_id, name) DO UPDATE SET active = true`, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_credit_balances (merchant_id, key, unit)
VALUES ($1, 'image-credit', 'image-credit')
ON CONFLICT (merchant_id, key) DO UPDATE SET unit = EXCLUDED.unit`, merchantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_balances WHERE merchant_id = $1 AND key = $2", merchantID, "image-credit")
	})

	// 1000 credits at 10_000 micros / 3 = 3_333_333.33 micros — a quantity whose
	// exact cost is not an integer, so the mode is the only thing that can decide
	// the last micro.
	for _, c := range []struct {
		mode string
		want int64
	}{
		{"up", 3_333_334},
		{"down", 3_333_333},
		{"half_up", 3_333_333},
	} {
		productID := uuid.New()
		productKey := "image-credit-topup-" + uuid.NewString()
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_purchase_prices WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
		})
		_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Topup', $3)`, productID, productKey, merchantID)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_credit_purchase_prices (merchant_id, product_id, ordinal, credit_key, currency, price)
VALUES ($1, $2, 1, 'image-credit', 'USD', $3::jsonb)`, merchantID, productID,
			`{"model":"per_unit","per_unit":{"unit_amount":10000,"divide_by":3,"round":"`+c.mode+`"}}`)
		require.NoError(t, err)

		quote, err := svc.QuoteCatalogCreditPurchase(ctx, money.CatalogCreditPurchaseQuoteInput{
			ProductKey: productKey,
			Credits:    1_000,
		})
		require.NoError(t, err, "round %q", c.mode)
		require.Equal(t, int64(1_000), quote.TotalCredits, "round %q", c.mode)
		require.Equal(t, c.want, quote.PaidAmount, "round %q quoted the wrong micros", c.mode)
	}
}
