//go:build integration

package embedded

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// An injected Redis client is borrowed from the host: Close must leave it
// usable (cozy-art ca#198 — closing the host's shared client silently broke
// its auth ephemeral store).
func TestClose_DoesNotCloseInjectedRedisClient(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), superDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	rdb, ctx := dbtest.SharedRedisClient(t)
	require.NoError(t, rdb.Ping(ctx).Err())

	cfg := &config.Config{
		Env:      "development",
		TestMode: config.CredentialPostureSandbox,
		DB:       &config.DBConfig{URL: superDSN},
	}
	e, err := New(Options{Config: cfg, PGXPool: pool, Redis: rdb})
	require.NoError(t, err)
	require.NoError(t, e.Close(context.Background()))

	require.NoError(t, rdb.Ping(ctx).Err(),
		"embed Close must not close a Redis client it did not create")
}
