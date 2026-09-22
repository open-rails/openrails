//go:build integration

package embed

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
	"github.com/open-rails/openrails/internal/merchants"
)

func TestEmbedded_DeclarePSP(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	suffix := strings.ToLower(uuid.NewString()[:8])
	cfg := &config.Config{
		Env:                  "development",
		TestMode:             config.CredentialPostureSandbox,
		ProviderWriteMode:    config.ProviderWriteModeReadOnly,
		MerchantConfigSource: config.MerchantConfigSourceAPI,
		SecretBackend:        config.SecretBackendDB,
		DB:                   &config.DBConfig{URL: appDSN},
		Auth:                 &config.AuthConfig{Issuer: "https://declare-psp-" + suffix + ".openrails.test"},
	}
	ctx := context.Background()
	declaration := PSPDeclaration{Key: " Platform ", Rail: " PLATFORM ", AccountID: " internal-platform-" + suffix}
	opts := Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails(), Merchant: &MerchantDeclaration{Slug: "declare-psp-" + suffix, PSPs: []PSPDeclaration{declaration}}}
	engine, err := New(ctx, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	client, err := engine.Client()
	require.NoError(t, err)
	merchantID := client.MerchantID()
	readID := func(runtime *Runtime) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		err := runtime.app.Runtime.DB.RunInMerchantScope(ctx, merchantID, "inspect declared PSP", func(ctx context.Context) error {
			rows, err := runtime.app.Runtime.DB.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{MerchantID: merchantID.UUID()})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, "platform", rows[0].Rail)
			require.Equal(t, config.ProviderEnvironmentTest, rows[0].Environment)
			require.Equal(t, strings.TrimSpace(declaration.AccountID), rows[0].AccountID)
			require.Equal(t, "platform", *rows[0].Key)
			id = rows[0].ID
			return nil
		})
		require.NoError(t, err)
		return id
	}
	firstID := readID(engine)
	require.NotEqual(t, uuid.Nil, firstID)
	require.NoError(t, engine.Close(ctx))
	second, err := New(ctx, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	require.Equal(t, firstID, readID(second), "constructor restart preserves natural-key identity")
	// A constructor cannot attribute an existing provider account to another merchant.
	opts.Merchant = &MerchantDeclaration{Slug: "declare-psp-other-" + suffix, PSPs: []PSPDeclaration{declaration}}
	rejected, err := New(ctx, opts)
	require.ErrorContains(t, err, "owned by another merchant")
	require.Nil(t, rejected)
	require.NoError(t, pool.Ping(ctx), "failed construction must preserve the borrowed pool")
	_, lookupErr := second.app.Runtime.Merchants.GetBySlug(ctx, opts.Merchant.Slug)
	require.ErrorIs(t, lookupErr, merchants.ErrMerchantNotFound, "foreign account preflight must not provision a merchant")
}
