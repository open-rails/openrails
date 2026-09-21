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

func TestSyncCatalogSidecars_PersistsRateCards(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)

	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	productKey := "sidecar-product-" + uuid.NewString()
	meterKey := "droplet-seconds-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_rate_cards WHERE merchant_id = $1", merchantID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	_, err := pool.Exec(ctx, `
INSERT INTO billing.products (id, key, display_name, merchant_id)
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
				"per_unit":{"divide_by":3600,"matrix":{"dimension":"size_slug","cells":{"s-1vcpu-1gb":{"unit_amount":"8930","maximum_amount":"6000000"}}}}
			}`),
		}},
	}, CatalogMutationOptions{Insert: true, Overwrite: true, Prune: true}))

	var eventType, groupBy, priceCurrency string
	require.NoError(t, pool.QueryRow(ctx, `
SELECT cm.event_type,
       cm.group_by ->> 'size_slug',
       rc.price ->> 'currency'
FROM billing.catalog_meters cm
JOIN billing.catalog_rate_cards rc
  ON rc.merchant_id = cm.merchant_id AND rc.meter_key = cm.key
WHERE cm.merchant_id = $1 AND cm.key = $2`, merchantID, meterKey).
		Scan(&eventType, &groupBy, &priceCurrency))
	require.Equal(t, "droplet.usage", eventType)
	require.Equal(t, "metadata.size_slug", groupBy)
	require.Equal(t, "USD", priceCurrency)
}
