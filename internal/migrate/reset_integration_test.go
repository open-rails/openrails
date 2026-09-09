//go:build integration

package migrate_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
)

func TestEmbeddedResetIsTransactionalAndLedgerScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	adminConfig.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)

	databaseName := fmt.Sprintf("openrails_reset_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminPool.Exec(context.Background(),
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
			databaseName)
		_, _ = adminPool.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize())
	})

	targetDSN := dsnWithDatabase(t, dbtest.SharedSuperuserDSN(t), databaseName)
	target, err := pgx.Connect(ctx, targetDSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close(context.Background())) })
	_, err = target.Exec(ctx, `
		CREATE SCHEMA openrails;
		CREATE TABLE public.migrations (
			app text NOT NULL,
			database text NOT NULL,
			schema text NOT NULL,
			name text NOT NULL
		);
		INSERT INTO public.migrations (app, database, schema, name) VALUES
			('openrails', 'postgres', 'openrails', '1'),
			('openrails', 'postgres', 'another_schema', 'keep-schema'),
			('another_app', 'postgres', 'openrails', 'keep-app');
	`)
	require.NoError(t, err)

	plan, err := migrate.PlanEmbeddedReset(ctx, targetDSN)
	require.NoError(t, err)
	require.True(t, plan.SchemaExists)
	require.Equal(t, []string{"1"}, plan.LedgerRows)
	require.NotContains(t, plan.Report(), "admin_password")

	_, err = migrate.ApplyEmbeddedReset(ctx, targetDSN, "other:5432/db",
		migrate.EmbeddedResetConfirmation(plan.Target))
	require.ErrorContains(t, err, "not allow-listed")
	var schemaExists bool
	require.NoError(t, target.QueryRow(ctx, `SELECT to_regnamespace('openrails') IS NOT NULL`).Scan(&schemaExists))
	require.True(t, schemaExists, "a refused reset must not mutate the target")

	_, err = target.Exec(ctx, `ALTER TABLE public.migrations RENAME COLUMN schema TO migration_schema`)
	require.NoError(t, err)
	_, err = migrate.ApplyEmbeddedReset(ctx, targetDSN, plan.Target,
		migrate.EmbeddedResetConfirmation(plan.Target))
	require.ErrorContains(t, err, "delete openrails migration ledger")
	require.NoError(t, target.QueryRow(ctx, `SELECT to_regnamespace('openrails') IS NOT NULL`).Scan(&schemaExists))
	require.True(t, schemaExists, "a ledger failure must roll back the schema drop")
	_, err = target.Exec(ctx, `ALTER TABLE public.migrations RENAME COLUMN migration_schema TO schema`)
	require.NoError(t, err)

	result, err := migrate.ApplyEmbeddedReset(ctx, targetDSN, plan.Target,
		migrate.EmbeddedResetConfirmation(plan.Target))
	require.NoError(t, err)
	require.Equal(t, int64(1), result.DeletedLedgerRows)
	require.NoError(t, target.QueryRow(ctx, `SELECT to_regnamespace('openrails') IS NOT NULL`).Scan(&schemaExists))
	require.False(t, schemaExists)
	var retained int
	require.NoError(t, target.QueryRow(ctx, `SELECT count(*) FROM public.migrations`).Scan(&retained))
	require.Equal(t, 2, retained, "reset must preserve other apps and schemas")
}

func dsnWithDatabase(t *testing.T, dsn, database string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Scheme, "integration DSN must use URL form")
	parsed.Path = "/" + database
	return parsed.String()
}
