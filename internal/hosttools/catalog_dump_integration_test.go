//go:build integration

package hosttools

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/stretchr/testify/require"
)

func TestCatalogApplyDumpRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	merchantID := uuid.New()
	merchantSlug := "catalog-roundtrip-" + strings.ReplaceAll(merchantID.String()[:8], "-", "")
	_, err = pool.Exec(ctx, `INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`, merchantID, merchantSlug)
	require.NoError(t, err)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{Schema: config.DefaultSchema}}
	manifest := []byte(`schema_version: 1
application_id: roundtrip-initial
expected_revision: 0
meters:
  - key: api_calls
    event_type: api.request
    aggregation: sum
    value_property: $.count
products:
  - key: base
    display_name: Base
    entitlements_spec: {base: null, quota: 5}
    prices:
      - key: base-monthly
        currency: usd
        unit_amount: "1200000"
        access_duration_hours: 720
        auto_renew: true
    rate_cards:
      - meter: api_calls
        payment_term: in_arrears
        price:
          model: per_unit
          currency: usd
          per_unit:
            unit_amount: "10"
            divide_by: 100
  - key: bundle
    display_name: Bundle
    entitlements_spec: {bundle: null}
    prices:
      - key: bundle-monthly
        currency: usd
        unit_amount: "2500000"
        access_duration_hours: 720
        auto_renew: true
      - key: bundle-retired
        currency: usd
        unit_amount: "2000000"
        access_duration_hours: 720
        auto_renew: true
        archived: true
  - key: retired
    display_name: Retired
    archived: true
`)
	initial, err := ApplyMerchantCatalog(ctx, CatalogApplyOptions{Config: cfg, PGXPool: pool, Merchant: merchantSlug, Manifest: manifest})
	require.NoError(t, err)
	var firstDump bytes.Buffer
	require.NoError(t, DumpMerchantCatalog(ctx, CatalogDumpOptions{Config: cfg, PGXPool: pool, Merchant: merchantSlug, ApplicationID: "roundtrip-export-1", Out: &firstDump}))
	first, err := catalog.ParseApplicationYAML(firstDump.Bytes())
	require.NoError(t, err, "dump must use the same application schema")
	require.Equal(t, "roundtrip-export-1", first.ApplicationID)
	require.Equal(t, initial.AppliedRevision, *first.ExpectedRevision)
	require.Len(t, first.Products, 2, "configuration dump excludes retired products")
	for _, product := range first.Products {
		require.Len(t, product.Prices, 1, "only the active current offer is exported")
		require.Equal(t, product.Key+"-monthly", product.Prices[0].Key)
		require.Equal(t, 720, product.Prices[0].AccessDurationHours.Value)
	}
	require.NotContains(t, firstDump.String(), "bundle-retired")
	require.NotContains(t, firstDump.String(), "expiry_hours")
	applied, err := ApplyMerchantCatalog(ctx, CatalogApplyOptions{Config: cfg, PGXPool: pool, Merchant: merchantSlug, Manifest: firstDump.Bytes()})
	require.NoError(t, err)
	require.False(t, applied.Replayed)
	replayed, err := ApplyMerchantCatalog(ctx, CatalogApplyOptions{Config: cfg, PGXPool: pool, Merchant: merchantSlug, Manifest: firstDump.Bytes()})
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, applied.AppliedRevision, replayed.AppliedRevision)
	var secondDump bytes.Buffer
	require.NoError(t, DumpMerchantCatalog(ctx, CatalogDumpOptions{Config: cfg, PGXPool: pool, Merchant: merchantSlug, Out: &secondDump}))
	second, err := catalog.ParseApplicationYAML(secondDump.Bytes())
	require.NoError(t, err)
	require.NotEmpty(t, second.ApplicationID)
	require.NotEqual(t, first.ApplicationID, second.ApplicationID, "each implicit export represents a new intended application")
	require.Equal(t, applied.AppliedRevision, *second.ExpectedRevision)
	require.Equal(t, first.Products, second.Products, "reapplying an export must preserve catalog content")
	require.Equal(t, first.Meters, second.Meters)
}
