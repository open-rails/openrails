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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageMeterCatalogLifecycle(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	meterKey := "or805-usage-" + uuid.NewString()[:8]
	eventType := "or805.usage." + uuid.NewString()[:8]

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE merchant_id = $1 AND event_type = $2", merchantID, eventType)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := pool.Exec(ctx, `
INSERT INTO openrails.products (id, key, display_name, merchant_id)
VALUES ($1, $2, 'Usage product', $3)`, productID, "or805-product-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)

	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key:           "  " + meterKey + "  ",
		EventType:     eventType,
		ValueProperty: "$.units",
		Aggregation:   "SUM",
		Unit:          "request",
		GroupBy: map[string]string{
			" region ": " $.region ",
			"size":     "$.size",
		},
	}))

	prices := []pricing.RatePrice{
		{
			Model:    pricing.ModelPerUnit,
			Currency: currency,
			PerUnit: &pricing.PerUnitPrice{
				DivideBy: 100,
				Round:    pricing.RoundUp,
				Matrix: &pricing.Matrix{
					Dimension: "size",
					Cells: map[string]pricing.MatrixCell{
						"small": {UnitAmount: 10_000},
						"large": {UnitAmount: 20_000, MaximumAmount: 5_000_000},
					},
				},
			},
		},
		{
			Model:    pricing.ModelTiered,
			Currency: currency,
			Tiered: &pricing.TieredPrice{
				Mode: pricing.TierModeGraduated,
				Tiers: []pricing.RateTier{
					{UpTo: int64Pointer(100), UnitAmount: 10_000},
					{UpTo: nil, UnitAmount: 5_000},
				},
			},
		},
		{
			Model:    pricing.ModelPackage,
			Currency: currency,
			Package:  &pricing.PackagePrice{PackageSize: 100, Amount: 250_000, FreeUnits: 10},
		},
	}
	for _, price := range prices {
		require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
			ProductID: &productID,
			MeterKey:  meterKey,
			Filter:    map[string][]string{" region ": {" eu ", "eu"}},
			Price:     price,
			Allowance: &pricing.Allowance{Included: 5},
		}))
	}
	var ordinalBefore int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT ordinal FROM openrails.catalog_rate_cards
WHERE merchant_id = $1 AND meter_key = $2 AND customer_id IS NULL`, merchantID, meterKey).Scan(&ordinalBefore))
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID,
		MeterKey:  meterKey,
		Filter:    map[string][]string{"region": {"eu"}},
		Price:     prices[len(prices)-1],
		Allowance: &pricing.Allowance{Included: 5},
	}))
	var ordinalAfter int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT ordinal FROM openrails.catalog_rate_cards
WHERE merchant_id = $1 AND meter_key = $2 AND customer_id IS NULL`, merchantID, meterKey).Scan(&ordinalAfter))
	assert.Equal(t, ordinalBefore, ordinalAfter, "idempotent puts must not move product ordering")

	detail, err := svc.GetUsageMeter(ctx, meterKey)
	require.NoError(t, err)
	assert.Equal(t, meterKey, detail.Key)
	assert.Equal(t, eventType, detail.EffectiveEventType)
	assert.Equal(t, map[string]string{"region": "$.region", "size": "$.size"}, detail.GroupBy)
	assert.True(t, detail.BillingSupported)
	require.NotNil(t, detail.DefaultRateCard)
	assert.Equal(t, productID, detail.DefaultRateCard.ProductID)
	assert.Equal(t, map[string][]string{"region": {"eu"}}, detail.DefaultRateCard.Filter)
	assert.Equal(t, pricing.ModelPackage, detail.DefaultRateCard.Price.Model)

	page, err := svc.ListUsageMeters(ctx, 200, 0)
	require.NoError(t, err)
	assert.Equal(t, 200, page.Limit)
	assert.Contains(t, usageMeterKeys(page.Items), meterKey)

	negotiatedPrice := pricing.RatePrice{
		Model:    pricing.ModelPackage,
		Currency: currency,
		Package:  &pricing.PackagePrice{PackageSize: 100, Amount: 100_000},
	}
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer:     &payer,
		MeterKey:  meterKey,
		Price:     negotiatedPrice,
		Allowance: &pricing.Allowance{Included: 20},
	}))
	changedCurrency := prices[len(prices)-1]
	changedCurrency.Currency = "EUR"
	require.ErrorIs(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID,
		MeterKey:  meterKey,
		Filter:    map[string][]string{"region": {"eu"}},
		Price:     changedCurrency,
	}), money.ErrRateCardCurrencyMismatch)

	overrides, err := svc.ListUsageMeterOverrides(ctx, meterKey, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), overrides.Total)
	require.Len(t, overrides.Items, 1)
	assert.Equal(t, payer.UUID(), overrides.Items[0].CustomerID)
	assert.Equal(t, int64(20), overrides.Items[0].Allowance.Included)

	detail, err = svc.GetUsageMeter(ctx, meterKey)
	require.NoError(t, err)
	assert.Equal(t, int64(1), detail.OverrideCount)
	require.ErrorIs(t, svc.DeleteDefaultUsageRateCard(ctx, meterKey), money.ErrRateCardHasOverrides)

	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer:      &payer,
		Invoker:    payer.UUID().String(),
		Currency:   currency,
		EventType:  eventType,
		Dimensions: map[string]int64{"units": 10},
		Metadata:   map[string]any{"region": "eu", "size": "small"},
		Amount:     0,
		Key:        money.MustIdempotencyKey(money.UsageOperation(eventType), "or805", uuid.NewString()),
		OccurredAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	detail, err = svc.GetUsageMeter(ctx, meterKey)
	require.NoError(t, err)
	assert.True(t, detail.HasActivity)
	require.NotNil(t, detail.LastEventAt)

	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key:           meterKey,
		EventType:     eventType,
		ValueProperty: "$.units",
		Aggregation:   pricing.AggregationSum,
		Unit:          "request",
		GroupBy:       map[string]string{"region": "$.region", "size": "$.size"},
	}))
	require.ErrorIs(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key:           meterKey,
		EventType:     eventType,
		ValueProperty: "$.units",
		Aggregation:   pricing.AggregationSum,
		Unit:          "token",
		GroupBy:       map[string]string{"region": "$.region", "size": "$.size"},
	}), money.ErrMeterInUse)

	require.NoError(t, svc.DeletePayerRateCard(ctx, payer, meterKey))
	require.NoError(t, svc.DeleteDefaultUsageRateCard(ctx, meterKey))
	detail, err = svc.GetUsageMeter(ctx, meterKey)
	require.NoError(t, err)
	assert.Nil(t, detail.DefaultRateCard)
	require.ErrorIs(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer:    &payer,
		MeterKey: meterKey,
		Price:    negotiatedPrice,
	}), money.ErrDefaultRateCardRequired)
}

func TestUsageMeterCatalogRejectsForeignReferences(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	meterKey := "or805-scope-" + uuid.NewString()[:8]
	foreignMerchantID := uuid.New()
	foreignProductID := uuid.New()
	foreignCustomerID := uuid.New()
	foreignSlug := "or805-foreign-" + foreignMerchantID.String()[:8]
	adminPool := dbtest.SharedSuperuserPGXPool(t)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = adminPool.Exec(ctx, "DELETE FROM openrails.customers WHERE merchant_id = $1", foreignMerchantID)
		_, _ = adminPool.Exec(ctx, "DELETE FROM openrails.products WHERE merchant_id = $1", foreignMerchantID)
		_, _ = adminPool.Exec(ctx, "DELETE FROM openrails.merchants WHERE id = $1", foreignMerchantID)
	})

	_, err := adminPool.Exec(ctx, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, foreignMerchantID, foreignSlug)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `
INSERT INTO openrails.products (id, key, display_name, merchant_id)
VALUES ($1, 'foreign-product', 'Foreign', $2)`, foreignProductID, foreignMerchantID)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `
INSERT INTO openrails.customers (id, merchant_id, subject)
VALUES ($1, $2, $3)`, foreignCustomerID, foreignMerchantID, foreignCustomerID.String())
	require.NoError(t, err)

	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key:           meterKey,
		EventType:     meterKey,
		ValueProperty: "$.units",
		Aggregation:   pricing.AggregationSum,
	}))
	price := pricing.RatePrice{
		Model:    pricing.ModelPerUnit,
		Currency: currency,
		PerUnit:  &pricing.PerUnitPrice{UnitAmount: 10_000},
	}
	require.ErrorIs(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &foreignProductID,
		MeterKey:  meterKey,
		Price:     price,
	}), money.ErrRateCardProductNotFound)

	localProductID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", localProductID)
	})
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.products (id, key, display_name, merchant_id)
VALUES ($1, $2, 'Local', $3)`, localProductID, "or805-local-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &localProductID,
		MeterKey:  meterKey,
		Price:     price,
	}))

	foreignPayer := identity.CustomerIDFromString(foreignCustomerID.String())
	require.Error(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer:    &foreignPayer,
		MeterKey: meterKey,
		Price:    price,
	}))

	mismatched := price
	mismatched.Currency = "EUR"
	require.ErrorIs(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer:    &payer,
		MeterKey: meterKey,
		Price:    mismatched,
	}), money.ErrRateCardCurrencyMismatch)

	otherSvc := money.NewMoneyService(dbtest.OpenMerchantDB(t, foreignMerchantID))
	otherCtx := merchant.WithID(context.Background(), merchant.ID(foreignMerchantID))
	_, err = otherSvc.GetUsageMeter(otherCtx, meterKey)
	require.ErrorIs(t, err, money.ErrUsageMeterNotFound)
}

func usageMeterKeys(meters []money.UsageMeter) []string {
	keys := make([]string, 0, len(meters))
	for _, meter := range meters {
		keys = append(keys, meter.Key)
	}
	return keys
}

func int64Pointer(value int64) *int64 {
	return &value
}
