//go:build integration

package embedded

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func TestEmbedded_DeclarePSP(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	suffix := strings.ToLower(uuid.NewString()[:8])
	cfg := &config.Config{
		Env:               "development",
		TestMode:          config.CredentialPostureSandbox,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		MerchantSource:    config.MerchantSourceAPI,
		SecretBackend:     config.SecretBackendDB,
		DB:                &config.DBConfig{URL: appDSN},
		Auth:              &config.AuthConfig{Issuer: "https://declare-psp-" + suffix + ".openrails.test"},
	}
	engine, err := New(Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	ctx := context.Background()
	require.NoError(t, embcp.Attach(ctx, engine.App(), cfg, pool))
	merchantResult, err := embcp.ProvisionMerchant(ctx, engine.App(), embcp.ProvisionMerchantRequest{Slug: "declare-psp-" + suffix})
	require.NoError(t, err)

	declaration := PSPDeclaration{
		Key:       " Platform ",
		Rail:      " PLATFORM ",
		AccountID: " internal-platform-" + suffix,
	}
	firstID, err := engine.DeclarePSP(ctx, merchantResult.MerchantID, declaration)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, firstID)
	secondID, err := engine.DeclarePSP(ctx, merchantResult.MerchantID, declaration)
	require.NoError(t, err)
	require.Equal(t, firstID, secondID, "re-declaration must preserve the natural-key identity")

	err = engine.RunInMerchantConn(ctx, merchantResult.MerchantID, func(ctx context.Context) error {
		rows, queryErr := engine.App().Runtime.DB.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{
			MerchantID: merchantResult.MerchantID.UUID(),
		})
		require.NoError(t, queryErr)
		require.Len(t, rows, 1, "re-declaration must not create a duplicate")
		require.Equal(t, firstID, rows[0].ID)
		require.Equal(t, "platform", rows[0].Rail)
		require.Equal(t, config.ProviderEnvironmentTest, rows[0].Environment)
		require.Equal(t, strings.TrimSpace(declaration.AccountID), rows[0].AccountID)
		require.NotNil(t, rows[0].Key)
		require.Equal(t, "platform", *rows[0].Key)
		return nil
	})
	require.NoError(t, err)

	otherMerchant, err := embcp.ProvisionMerchant(ctx, engine.App(), embcp.ProvisionMerchantRequest{Slug: "declare-psp-other-" + suffix})
	require.NoError(t, err)
	_, err = engine.DeclarePSP(ctx, otherMerchant.MerchantID, declaration)
	require.ErrorContains(t, err, "owned by another merchant")
}
