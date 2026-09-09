//go:build integration

package postgresmigrations_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

func TestBaselineAppliesWithoutProfilesSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	adminConfig.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)

	databaseName := fmt.Sprintf("openrails_no_profiles_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminPool.Exec(context.Background(),
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
			databaseName)
		_, _ = adminPool.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize())
	})

	targetConfig, err := pgxpool.ParseConfig(dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	targetConfig.ConnConfig.Config.Database = databaseName
	targetDB := stdlib.OpenDB(*targetConfig.ConnConfig)
	t.Cleanup(func() { require.NoError(t, targetDB.Close()) })

	var profilesExists bool
	require.NoError(t, targetDB.QueryRowContext(ctx,
		"SELECT to_regnamespace('profiles') IS NOT NULL").Scan(&profilesExists))
	require.False(t, profilesExists, "test database must not carry AuthKit's sibling schema")

	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	err = migratekit.NewPostgres(targetDB, config.MigratekitApp).
		WithSchema(config.DefaultSchema).
		ApplyMigrations(ctx, migrations)
	require.NoError(t, err)
}
