//go:build integration

package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// OpenAppDB returns a pool-backed *db.DB on the given DSN, closed on test
// cleanup.
func OpenAppDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	d, err := db.NewDB(t.Context(), &config.DBConfig{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	BindRiver(t, d)
	return d
}

// OpenMerchantDB returns a *db.DB on the RLS-enforcing default role with
// app.merchant_id pinned on every connection.
//
// Use it when the test drives a MODULE SERVICE directly — below the layer that
// opens the merchant connection in production (the HTTP router / River worker).
// The test stands in for that layer, so it must supply what that layer supplies.
// Tests that drive the full entry point must NOT use this: proving the code pins
// the merchant itself is their whole point.
func OpenMerchantDB(t *testing.T, merchantID uuid.UUID) *db.DB {
	t.Helper()
	d, err := db.NewWithPGXPool(SharedMerchantPool(t, merchantID), config.DefaultSchema)
	require.NoError(t, err)
	BindRiver(t, d)
	return d
}

// OpenOneConnAppDB is a ONE-connection pool on the RLS-enforcing app role with
// no ambient merchant pin — the production request shape at its tightest.
// Merchant rows are visible only on a connection the code pinned itself
// (WithMerchantConn), and any path that needs a second connection while the
// request holds its pin stalls until the acquire bound fails it.
func OpenOneConnAppDB(t *testing.T) *db.DB {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(SharedPostgresDSN(t))
	require.NoError(t, err, "parse shared app dsn")
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err, "open one-connection app pool")
	t.Cleanup(pool.Close)
	d, err := db.NewWithPGXPool(pool, config.DefaultSchema)
	require.NoError(t, err)
	BindRiver(t, d)
	return d
}

// BindRiver supplies the real insert-only client in module-level fixtures. Tests
// do not run background workers unless explicitly requested.
func BindRiver(t *testing.T, d *db.DB) {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
}
