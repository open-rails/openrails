package openrails

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	internalmigrate "github.com/open-rails/openrails/internal/migrate"
	"github.com/stretchr/testify/require"
)

func TestSkipMigrations(t *testing.T) {
	url := os.Getenv("SQLC_DATABASE_URL")
	if url == "" {
		t.Skip("SQLC_DATABASE_URL is required")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	database := "skip_migrations_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
	require.NoError(t, err)
	runtimeConfig := admin.Config()
	runtimeConfig.ConnConfig.Database = database
	pool, err := pgxpool.NewWithConfig(ctx, runtimeConfig)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err)
	})
	schema := "skip_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	jobs := schema + "_river"
	cfg := Config{ProviderWriteMode: ProviderWritesReadOnly, Database: DatabaseConfig{Schema: schema, RiverSchema: jobs, SkipMigrations: true}}
	engine, err := New(ctx, cfg, Deps{Postgres: pool})
	require.Nil(t, engine)
	require.ErrorIs(t, err, ErrMigrationsRequired)
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL OR to_regnamespace($2) IS NOT NULL", schema, jobs).Scan(&exists))
	require.False(t, exists, "startup created schemas")
	require.NoError(t, Migrate(ctx, pool, cfg))
	readonlyConfig := pool.Config()
	readonlyConfig.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	readonly, err := pgxpool.NewWithConfig(ctx, readonlyConfig)
	require.NoError(t, err)
	t.Cleanup(readonly.Close)
	require.NoError(t, internalmigrate.Validate(ctx, readonly, internalmigrate.Options{Schema: schema, RiverSchema: jobs}))
	_, err = pool.Exec(ctx, `CREATE FUNCTION public.forbid_runtime_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'runtime attempted DDL'; END $$;
CREATE EVENT TRIGGER forbid_runtime_ddl ON ddl_command_start EXECUTE FUNCTION public.forbid_runtime_ddl()`)
	require.NoError(t, err)
	engine, err = New(ctx, cfg, Deps{Postgres: pool})
	require.NoError(t, err)
	require.NoError(t, engine.Close(ctx))
	wrongJobs := schema + "_absent"
	require.ErrorIs(t, internalmigrate.Validate(ctx, pool, internalmigrate.Options{Schema: schema, RiverSchema: wrongJobs}), ErrMigrationsRequired)
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", wrongJobs).Scan(&exists))
	require.False(t, exists)
	_, err = pool.Exec(ctx, "UPDATE public.migrations SET status='failed' WHERE app='openrails' AND schema=$1 AND sequence=(SELECT max(sequence) FROM public.migrations WHERE app='openrails' AND schema=$1)", schema)
	require.NoError(t, err)
	engine, err = New(ctx, cfg, Deps{Postgres: pool})
	require.Nil(t, engine)
	require.ErrorIs(t, err, ErrMigrationsRequired)
	var failed int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM public.migrations WHERE app='openrails' AND schema=$1 AND status='failed'", schema).Scan(&failed))
	require.Equal(t, 1, failed, "startup repaired the ledger")
}
