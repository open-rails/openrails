//go:build integration

package hosttools_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#885: `embed.New` was the ONLY gated door. Every manifest-plane entry
// point (catalog push/dump, converge, prune, admin grants, billing import,
// provider pull) takes the host's pool — or opens its own from Config.DB —
// separately, so a host refused at boot could still run those against the very
// privileged connection the boot gate exists to reject. Posture is a property
// of the connected ROLE, so it is checked wherever a connection is accepted.
func TestManifestPlaneEntryPointsRefuseBypassRLSPool(t *testing.T) {
	ctx := context.Background()
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:               "staging",
		TestMode:          config.CredentialPostureLive,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		MerchantSource:    config.MerchantSourceManifest,
		DB:                &config.DBConfig{URL: superDSN, Schema: config.DefaultSchema},
	}
	manifest := []byte(`version: 1
catalogs:
  - merchant: rls-posture-guard
    products:
      - key: base
        display_name: Base
        prices:
          - currency: usd
            unit_amount: 1200000
            duration: 30d
            auto_renew: true
`)

	cases := []struct {
		name string
		call func() error
	}{
		{"hosttools.PushMerchantCatalog", func() error {
			return hosttools.PushMerchantCatalog(ctx, hosttools.CatalogPushOptions{Config: cfg, PGXPool: pool, Manifest: manifest, Insert: true, Overwrite: true, Prune: true})
		}},
		{"hosttools.DumpMerchantCatalog", func() error {
			return hosttools.DumpMerchantCatalog(ctx, hosttools.CatalogDumpOptions{Config: cfg, PGXPool: pool, Merchant: "rls-posture-guard"})
		}},
		{"hosttools.ConvergeMerchant", func() error {
			_, err := hosttools.ConvergeMerchant(ctx, hosttools.ConvergeMerchantOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New())})
			return err
		}},
		{"ImportAdminGrants", func() error {
			_, err := billingimport.Import(ctx, billingimport.Options{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New()),
				Book: billingimport.DeclaredBilling{AsOf: time.Now().UTC(),
					AdminGrants: []billingimport.DeclaredAdminGrant{{Customer: openrails.CustomerID(uuid.New()), Product: openrails.ProductID(uuid.New()), SourceID: "src", StartsAt: time.Now().UTC()}}}})
			return err
		}},
		{"hosttools.PruneList", func() error {
			return hosttools.PruneList(ctx, hosttools.PruneListOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New())})
		}},
		{"ImportBilling", func() error {
			_, err := billingimport.Import(ctx, billingimport.Options{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New()),
				Book: billingimport.DeclaredBilling{AsOf: time.Now().UTC()}})
			return err
		}},
		{"hosttools.PullProviderReport", func() error {
			return hosttools.PullProviderReport(ctx, hosttools.PullProviderReportOptions{Config: cfg, MerchantID: merchant.ID(uuid.New())})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err, "%s must refuse a BYPASSRLS connection", tc.name)
			require.ErrorContains(t, err, "bypasses RLS")
			require.ErrorContains(t, err, "openrails_app")
		})
	}
}

// TestManifestPlaneEntryPointsRunAsAppRole is the positive half: the SAME entry
// points do real work on an RLS-enforcing pool, so the guard rejects the role,
// not the call.
func TestManifestPlaneEntryPointsRunAsAppRole(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	merchantID := uuid.New()
	slug := "rls-guard-" + strings.ReplaceAll(merchantID.String()[:8], "-", "")
	_, err = pool.Exec(ctx, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, merchantID, slug)
	require.NoError(t, err)

	cfg := &config.Config{
		Env:               "staging",
		TestMode:          config.CredentialPostureLive,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		MerchantSource:    config.MerchantSourceManifest,
		DB:                &config.DBConfig{URL: appDSN, Schema: config.DefaultSchema},
	}

	var out bytes.Buffer
	require.NoError(t, hosttools.PruneList(ctx, hosttools.PruneListOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(merchantID), Out: &out}))

	res, err := hosttools.ConvergeMerchant(ctx, hosttools.ConvergeMerchantOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(merchantID)})
	require.NoError(t, err, "converge must run on an RLS-enforcing pool")
	require.Empty(t, res.Findings)

	require.NoError(t, hosttools.DumpMerchantCatalog(ctx, hosttools.CatalogDumpOptions{Config: cfg, PGXPool: pool, Merchant: slug, Out: &out}))
}
