//go:build integration

package embed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestRuntimeRegistersRestoreDestinationBeforeClient(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	id, slug := merchant.ID(uuid.New()), "restore-host-"+uuid.NewString()[:8]
	_, err = rt.RegisterMerchantForRestore(ctx, merchant.ID{}, slug)
	require.ErrorContains(t, err, "merchant_id is required")
	registered, err := rt.RegisterMerchantForRestore(ctx, id, slug)
	require.NoError(t, err)
	require.Equal(t, id, registered)
	again, err := rt.RegisterMerchantForRestore(ctx, id, slug)
	require.NoError(t, err)
	require.Equal(t, id, again)
	_, err = rt.RegisterMerchantForRestore(ctx, merchant.ID(uuid.New()), slug+"-other")
	require.ErrorIs(t, err, embed.ErrMerchantRestoreConflict)
	_, err = rt.RegisterMerchantForRestore(ctx, id, slug+"-other")
	require.ErrorIs(t, err, embed.ErrMerchantRestoreConflict)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var group, host *string
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT permission_group_id, api_host FROM openrails.merchants WHERE id=$1`, id.UUID()).Scan(&group, &host))
	require.Nil(t, group)
	require.Nil(t, host)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, id.String())
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM openrails.merchant_configurations) + (SELECT count(*) FROM openrails.psps) + (SELECT count(*) FROM openrails.merchant_secrets)`).Scan(&count))
	require.Zero(t, count, "registration creates directory identity only")
	require.NoError(t, tx.Rollback(ctx))
	client, err := rt.Client()
	require.NoError(t, err)
	_, err = client.GetMerchantSettings(ctx)
	require.NoError(t, err, "the runtime's client is bound to the registered UUID")
	bound, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{})
	require.NoError(t, err)
	require.Equal(t, id, bound, "subsequent ordinary manifest configuration preserves identity")
}
