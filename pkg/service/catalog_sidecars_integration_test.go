//go:build integration

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestSyncCatalogSidecars_PersistsRateCardsAndCreditPurchases(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)

	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	productKey := "sidecar-product-" + uuid.NewString()
	meterKey := "droplet-seconds-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_purchase_prices WHERE merchant_id = $1", merchantID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_credit_balances WHERE merchant_id = $1", merchantID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1", merchantID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := pool.Exec(ctx, `
INSERT INTO openrails.products (id, key, display_name, merchant_id)
VALUES ($1, $2, 'Sidecar Product', $3)`, productID, productKey, merchantID)
	require.NoError(t, err)

	svc := &Service{rt: &app.Runtime{DB: dbi}}
	require.NoError(t, svc.SyncCatalogSidecars(ctx, SyncCatalogSidecarsRequest{
		Meters: []CatalogMeterSpec{{
			Key:           meterKey,
			EventType:     "droplet.usage",
			ValueProperty: "seconds",
			Aggregation:   "sum",
			Unit:          "second",
			GroupBy:       map[string]string{"size_slug": "metadata.size_slug"},
		}},
		RateCards: []CatalogRateCardSpec{{
			ProductKey:  productKey,
			Ordinal:     1,
			MeterKey:    meterKey,
			PaymentTerm: "in_arrears",
			Price: json.RawMessage(`{
				"model":"per_unit",
				"currency":"USD",
				"per_unit":{"divide_by":3600,"matrix":{"dimension":"size_slug","cells":{"s-1vcpu-1gb":{"unit_amount":8930,"maximum_amount":6000000}}}}
			}`),
		}},
		CreditBalances: []CatalogCreditBalanceSpec{{
			Key:  "image-credit",
			Unit: "image-credit",
		}},
		CreditPurchases: []CatalogCreditPurchasePriceSpec{{
			ProductKey: productKey,
			Ordinal:    1,
			CreditKey:  "image-credit",
			Currency:   "USD",
			InputMin:   1_000_000,
			Providers:  []string{"stripe"},
			Price:      json.RawMessage(`{"model":"per_unit","per_unit":{"unit_amount":10000}}`),
		}},
	}))

	var eventType, groupBy, priceCurrency, creditUnit string
	require.NoError(t, pool.QueryRow(ctx, `
SELECT cm.event_type,
       cm.group_by ->> 'size_slug',
       rc.price ->> 'currency',
       cb.unit
FROM openrails.catalog_meters cm
JOIN openrails.catalog_rate_cards rc
  ON rc.merchant_id = cm.merchant_id AND rc.meter_key = cm.key
JOIN openrails.catalog_credit_purchase_prices cpp
  ON cpp.merchant_id = cm.merchant_id
JOIN openrails.catalog_credit_balances cb
  ON cb.merchant_id = cpp.merchant_id AND cb.key = cpp.credit_key
WHERE cm.merchant_id = $1 AND cm.key = $2`, merchantID, meterKey).
		Scan(&eventType, &groupBy, &priceCurrency, &creditUnit))
	require.Equal(t, "droplet.usage", eventType)
	require.Equal(t, "metadata.size_slug", groupBy)
	require.Equal(t, "USD", priceCurrency)
	require.Equal(t, "image-credit", creditUnit)
}
