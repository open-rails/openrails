//go:build integration

package migrate_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
)

const (
	riverV026MigrationVersion = 6
	riverV047MigrationVersion = 7
	riverUpgradeQueue         = "river_upgrade"
)

type riverUpgradeArgs struct {
	Token string `json:"token"`
}

func (riverUpgradeArgs) Kind() string { return "river_upgrade_fixture" }

type riverUpgradeWorker struct {
	river.WorkerDefaults[riverUpgradeArgs]
}

func (*riverUpgradeWorker) Work(context.Context, *river.Job[riverUpgradeArgs]) error { return nil }

// TestRunPostgresUpgradesRiverV026State proves the exact production upgrade
// seam for #966. River v0.47 carries the same migrations 1..6 as v0.26, so
// stopping its real migrator at version 6 creates the old schema without a
// hand-maintained approximation. OpenRails' real migration entrypoint must
// then apply version 7 without losing an existing job, and the upgraded client
// must be able to claim and complete that pre-upgrade row.
func TestRunPostgresUpgradesRiverV026State(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	adminConfig.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)

	databaseName := fmt.Sprintf("openrails_river_upgrade_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminPool.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})

	targetDSN := dsnWithDatabase(t, dbtest.SharedSuperuserDSN(t), databaseName)
	pool, err := pgxpool.New(ctx, targetDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	legacyMigrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{
		Schema: config.RiverSchema,
	})
	require.NoError(t, err)
	legacyResult, err := legacyMigrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{
		TargetVersion: riverV026MigrationVersion,
	})
	require.NoError(t, err)
	require.Len(t, legacyResult.Versions, riverV026MigrationVersion)

	var legacyJobID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO public.river_job (args, kind, max_attempts, queue)
		VALUES ('{"token":"preserve-me"}'::jsonb, $1, 7, $2)
		RETURNING id`, riverUpgradeArgs{}.Kind(), riverUpgradeQueue).Scan(&legacyJobID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO public.river_client (id, updated_at) VALUES ('legacy-client', now())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO public.river_client_queue (river_client_id, name, updated_at)
		VALUES ('legacy-client', $1, now())`, riverUpgradeQueue)
	require.NoError(t, err)

	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: targetDSN}}
	require.NoError(t, migrate.RunPostgres(ctx, cfg))

	var versions []int
	rows, err := pool.Query(ctx, `SELECT version FROM public.river_migration WHERE line = 'main' ORDER BY version`)
	require.NoError(t, err)
	for rows.Next() {
		var version int
		require.NoError(t, rows.Scan(&version))
		versions = append(versions, version)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, riverV047MigrationVersion}, versions)

	var argsJSON, state string
	var maxAttempts int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT args::text, state::text, max_attempts
		FROM public.river_job WHERE id = $1`, legacyJobID).Scan(&argsJSON, &state, &maxAttempts))
	require.JSONEq(t, `{"token":"preserve-me"}`, argsJSON)
	require.Equal(t, "available", state)
	require.Equal(t, 7, maxAttempts)

	var notificationExists, clientExists, clientQueueExists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT
		to_regclass('public.river_notification') IS NOT NULL,
		to_regclass('public.river_client') IS NOT NULL,
		to_regclass('public.river_client_queue') IS NOT NULL`).Scan(
		&notificationExists, &clientExists, &clientQueueExists))
	require.True(t, notificationExists)
	require.False(t, clientExists)
	require.False(t, clientQueueExists)

	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &riverUpgradeWorker{}))
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{riverUpgradeQueue: {MaxWorkers: 1}},
		Workers: workers,
	})
	require.NoError(t, err)
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() { _ = client.Stop(context.Background()) })

	require.Eventually(t, func() bool {
		return pool.QueryRow(ctx, `SELECT state::text FROM public.river_job WHERE id = $1`, legacyJobID).
			Scan(&state) == nil && state == "completed"
	}, 15*time.Second, 100*time.Millisecond, "v0.47 must drain the job preserved from v0.26")
}
