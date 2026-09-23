//go:build integration

package hosttools_test

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
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestManifestPlaneEntryPointsAcceptOwnerAndRuntime(t *testing.T) {
	owner, runtime := dbtest.SharedRLSPostgres(t)
	for name, dsn := range map[string]string{"owner": owner, "runtime": runtime} {
		t.Run(name, func(t *testing.T) { runManifestPlaneEntryPoints(t, dsn) })
	}
}

func runManifestPlaneEntryPoints(t *testing.T, appDSN string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	merchantID := uuid.New()
	slug := "rls-guard-" + strings.ReplaceAll(merchantID.String()[:8], "-", "")
	_, err = pool.Exec(ctx, `INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`, merchantID, slug)
	require.NoError(t, err)

	cfg := &config.Config{
		TestMode:             config.CredentialPostureLive,
		ProviderWriteMode:    config.ProviderWriteModeReadOnly,
		MerchantConfigSource: config.MerchantConfigSourceManifest,
		DB:                   &config.DBConfig{URL: appDSN, Schema: config.DefaultSchema},
	}

	var out bytes.Buffer
	require.NoError(t, hosttools.PruneList(ctx, hosttools.PruneListOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(merchantID), Out: &out}))

	res, err := hosttools.ConvergeMerchant(ctx, hosttools.ConvergeMerchantOptions{Config: cfg, PGXPool: pool, MerchantID: merchant.ID(merchantID)})
	require.NoError(t, err, "converge must use explicit merchant scope")
	require.Empty(t, res.Findings)

	require.NoError(t, hosttools.DumpMerchantCatalog(ctx, hosttools.CatalogDumpOptions{Config: cfg, PGXPool: pool, Merchant: slug, Out: &out}))
}
