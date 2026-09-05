//go:build integration

package embedded

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #763: embedded construction never enforced the RLS posture gate
// (db.EnforceRLSPosture, issue #227) — only the standalone boot path did.
// internal/app.buildRuntimeWithOverrides now runs the check for BOTH
// construction paths from one call site, so embedded.New (the entry point
// every real embedding host uses) inherits it automatically. These tests
// drive that through the actual public entry point, not the internal
// function directly, so a future refactor that re-splits the two paths would
// be caught here.

// TestNew_EmbeddedBootRefusesBypassRLSRoleOutsideDev proves the non-dev fail
// case: an embedded host whose injected PGXPool connects as a
// superuser/BYPASSRLS role must NOT boot.
func TestNew_EmbeddedBootRefusesBypassRLSRoleOutsideDev(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), superDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:               "staging",
		TestMode:          config.CredentialPostureLive,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		DB:                &config.DBConfig{URL: superDSN},
	}
	_, err = New(context.Background(), Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.Error(t, err, "an embedded host connected as a BYPASSRLS role must refuse to boot")
	require.ErrorContains(t, err, "bypasses RLS")
	require.ErrorContains(t, err, "openrails_app")
}

// TestNew_EmbeddedBootRefusesBypassRLSRoleInDev proves DEVELOPMENT is not
// exempt (or#782). Dev used to warn and boot; that exemption is exactly what
// made missing-merchant-scope queries invisible until production.
func TestNew_EmbeddedBootRefusesBypassRLSRoleInDev(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), superDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:      "development",
		TestMode: config.CredentialPostureSandbox,
		DB:       &config.DBConfig{URL: superDSN},
	}
	_, err = New(context.Background(), Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.Error(t, err, "development must NOT be exempt from the RLS-posture gate")
	require.ErrorContains(t, err, "bypasses RLS")
}

// TestNew_EmbeddedBootSucceedsAsAppRoleOutsideDev proves the positive case: a
// properly RLS-enforcing role boots cleanly outside development.
func TestNew_EmbeddedBootSucceedsAsAppRoleOutsideDev(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:               "staging",
		TestMode:          config.CredentialPostureLive,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		DB:                &config.DBConfig{URL: appDSN},
	}
	e, err := New(context.Background(), Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.NoError(t, err, "an RLS-enforcing role must boot outside development")
	t.Cleanup(func() { _ = e.Close(context.Background()) })
}

// or#885: `embedded.New` was the ONLY gated door. Every manifest-plane entry
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
		{"PushMerchantCatalog", func() error {
			return PushMerchantCatalog(ctx, CatalogPushOptions{Config: cfg, PGXPool: pool, Manifest: manifest, Insert: true, Overwrite: true, Prune: true})
		}},
		{"DumpMerchantCatalog", func() error {
			return DumpMerchantCatalog(ctx, CatalogDumpOptions{Config: cfg, PGXPool: pool, Merchant: "rls-posture-guard"})
		}},
		{"ConvergeMerchant", func() error {
			_, err := ConvergeMerchant(ctx, ConvergeMerchantOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New())})
			return err
		}},
		{"ImportAdminGrants", func() error {
			_, err := ImportAdminGrants(ctx, AdminGrantImportOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New()),
				Grants: []AdminGrant{{Customer: uuid.New(), Product: uuid.New(), SourceID: "src"}}})
			return err
		}},
		{"PruneList", func() error {
			return PruneList(ctx, PruneListOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New())})
		}},
		{"ImportBilling", func() error {
			_, err := ImportBilling(ctx, BillingImportOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(uuid.New()),
				Book: DeclaredBilling{AsOf: time.Now().UTC()}})
			return err
		}},
		{"PullProviderReport", func() error {
			return PullProviderReport(ctx, PullProviderReportOptions{Config: cfg, MerchantID: merchant.ID(uuid.New())})
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
	require.NoError(t, PruneList(ctx, PruneListOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(merchantID), Out: &out}))

	res, err := ConvergeMerchant(ctx, ConvergeMerchantOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(merchantID)})
	require.NoError(t, err, "converge must run on an RLS-enforcing pool")
	require.Empty(t, res.Findings)

	require.NoError(t, DumpMerchantCatalog(ctx, CatalogDumpOptions{Config: cfg, PGXPool: pool, Merchant: slug, Out: &out}))
}
