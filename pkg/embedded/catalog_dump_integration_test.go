//go:build integration

package embedded

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestCatalogPushDumpRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	merchantID := uuid.New()
	merchantSlug := "catalog-roundtrip-" + strings.ReplaceAll(merchantID.String()[:8], "-", "")
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		merchantID, merchantSlug)
	require.NoError(t, err)

	cfg := &config.Config{DB: &config.DBConfig{Schema: config.DefaultSchema}}
	manifest := []byte(`version: 1
catalogs:
  - merchant: ` + merchantSlug + `
    meters:
      - key: api_calls
        aggregation: sum
    credit_balances:
      - key: image_credits
        unit: credit
        expires_default: 720h
    usage_limits:
      - key: daily_generations
        measure: generations
        windows:
          - window: 1d
            amount: 50
    products:
      - key: base
        display_name: Base
        entitlements: [base]
        credits:
          - key: image_credits
            amount: 100
            expires: 720h
        usage_limits: [daily_generations]
        prices:
          - currency: usd
            unit_amount: 1200000
            duration: 30d
            auto_renew: true
        rate_cards:
          - meter: api_calls
            payment_term: in_arrears
            price:
              model: per_unit
              currency: usd
              per_unit:
                unit_amount: 10
                divide_by: 100
      - key: bundle
        display_name: Bundle
        includes: [base]
        prices:
          - currency: usd
            unit_amount: 2500000
            duration: 30d
            auto_renew: true
`)

	var applyOut bytes.Buffer
	require.NoError(t, PushMerchantCatalog(ctx, CatalogPushOptions{
		Config: cfg, PGXPool: pool, Manifest: manifest, Out: &applyOut,
		Insert: true, Overwrite: true, Prune: true,
	}))

	var firstDump bytes.Buffer
	require.NoError(t, DumpMerchantCatalog(ctx, CatalogDumpOptions{
		Config: cfg, PGXPool: pool, Merchant: merchantSlug, Out: &firstDump,
	}))
	require.Contains(t, firstDump.String(), "expires: 30d")
	require.NotContains(t, firstDump.String(), "expiry_hours")
	targets, err := loadCatalogPushTargets(CatalogPushOptions{Manifest: firstDump.Bytes()})
	require.NoError(t, err, "dump should parse as push-merchant-catalog YAML")
	require.Len(t, targets, 1)
	require.Equal(t, merchantSlug, targets[0].Merchant)

	var secondApply bytes.Buffer
	require.NoError(t, PushMerchantCatalog(ctx, CatalogPushOptions{
		Config: cfg, PGXPool: pool, Manifest: firstDump.Bytes(), Out: &secondApply,
		Insert: true, Overwrite: true, Prune: true,
	}))

	var secondDump bytes.Buffer
	require.NoError(t, DumpMerchantCatalog(ctx, CatalogDumpOptions{
		Config: cfg, PGXPool: pool, Merchant: merchantSlug, Out: &secondDump,
	}))
	require.Equal(t, firstDump.String(), secondDump.String(), "push -> dump -> push should not drift")
}

// TestExampleCatalogRoundTripsThroughDump was deleted (#694): the example
// manifest's publish path is dominated by the integrationharness HTTP publish
// test (TestExampleCatalogPublishesOverHTTP) and dump stability by
// TestCatalogPushDumpRoundTrip above.
