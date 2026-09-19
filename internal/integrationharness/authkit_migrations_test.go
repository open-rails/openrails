//go:build integration

package integrationharness

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	authpostgres "github.com/open-rails/authkit/migrations/postgres"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"
)

// applyAuthKitMigrations runs AuthKit's embedded migration source through
// migratekit directly. The dedicated adapter handle keeps migration session
// settings away from the harness' application pool.
func applyAuthKitMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	migrations, err := migratekit.LoadFromFS(authpostgres.FS)
	require.NoError(t, err)
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "authkit")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, migrator.Close()) })
	require.NoError(t, migrator.WithSchema(schema).ApplyMigrations(ctx, migrations))
}
