//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

// or#909: per-customer negotiated price override — the included allowance is
// netted before overage, the override replaces the merchant default for that
// payer only, and the read side (ListPayerRateCards) reports exactly what
// rating will use. Merchant scoping is asserted on both read and delete.

func TestPayerRateOverride_AllowanceNettedBeforeOverage(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	meter := "or909-storage-" + uuid.NewString()[:8]
	eventType := "or909.usage." + uuid.NewString()[:8]
	defaultPayer := identity.CustomerIDFromString(uuid.NewString())

	t.Cleanup(func() {
		for _, p := range []uuid.UUID{payer.UUID(), defaultPayer.UUID()} {
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", p)
		}
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Storage', $3)`,
		productID, "or909-storage-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)
	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key: meter, EventType: eventType, ValueProperty: "gb", Aggregation: "sum", Unit: "gb",
	}))
	// Merchant default: $0.10/GB, no allowance.
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID, MeterKey: meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 100_000}},
	}))
	// Negotiated override: $0.04/GB with 10 GB included before overage.
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer: &payer, MeterKey: meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 40_000}},
		Allowance: &pricing.Allowance{Included: 10},
	}))

	// The read side reports the contract exactly.
	cards, err := svc.ListPayerRateCards(ctx, payer)
	require.NoError(t, err)
	require.Len(t, cards, 1)
	require.Equal(t, meter, cards[0].MeterKey)
	require.Equal(t, int64(40_000), cards[0].Price.PerUnit.UnitAmount)
	require.NotNil(t, cards[0].Allowance)
	require.Equal(t, int64(10), cards[0].Allowance.Included)

	// Identical 25 GB usage for both payers.
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	for _, p := range []identity.CustomerID{payer, defaultPayer} {
		pp := p
		_, err = svc.UpsertAccountSettings(ctx, pp, cur, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
		require.NoError(t, err)
		_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
			Payer: &pp, Invoker: pp.UUID().String(), Currency: cur, EventType: eventType,
			Dimensions: map[string]int64{"gb": 25}, Amount: 0,
			Key:        money.MustIdempotencyKey(money.UsageOperation(eventType), "or909", uuid.NewString()),
			OccurredAt: time.Now(),
		})
		require.NoError(t, err)
	}

	invDefault, err := svc.FinalizeInvoice(ctx, defaultPayer, cur, from, to)
	require.NoError(t, err)
	invNegotiated, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)

	// Default payer: 25 GB x $0.10 = $2.50. Negotiated: (25-10) x $0.04 = $0.60
	// — the included allowance nets BEFORE overage, then the override price.
	require.Equal(t, int64(2_500_000), invDefault.AmountDue, "default card, no allowance")
	require.Equal(t, int64(600_000), invNegotiated.AmountDue, "override: allowance netted, then negotiated unit price")

	// Dropping the override restores the default and empties the read side.
	require.NoError(t, svc.DeletePayerRateCard(ctx, payer, meter))
	cards, err = svc.ListPayerRateCards(ctx, payer)
	require.NoError(t, err)
	require.Empty(t, cards)
}

func TestPayerRateOverride_MerchantScoped(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	meter := "or909-scope-" + uuid.NewString()[:8]
	productID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Scope', $3)`,
		productID, "or909-scope-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)
	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key: meter, EventType: "or909.scope.evt." + uuid.NewString()[:8], ValueProperty: "gb", Aggregation: "sum", Unit: "gb",
	}))
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID,
		MeterKey:  meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 20_000}},
	}))
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer: &payer, MeterKey: meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 12_345}},
	}))

	otherID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active') ON CONFLICT (slug) WHERE deleted_at IS NULL AND permission_group_id IS NULL DO NOTHING`,
		otherID, "or909-other-"+otherID.String()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.merchants WHERE id = $1", otherID)
	})
	otherSvc := money.NewMoneyService(dbtest.OpenMerchantDB(t, otherID))
	otherCtx := merchant.WithID(context.Background(), merchant.ID(otherID))

	// Merchant B sees nothing and its delete touches nothing.
	cards, err := otherSvc.ListPayerRateCards(otherCtx, payer)
	require.NoError(t, err)
	require.Empty(t, cards, "merchant B must not see merchant A's negotiated override")
	require.NoError(t, otherSvc.DeletePayerRateCard(otherCtx, payer, meter))

	cards, err = svc.ListPayerRateCards(ctx, payer)
	require.NoError(t, err)
	require.Len(t, cards, 1, "merchant A's override survives merchant B's delete")
}
