//go:build integration

package embed

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestApplyMigrationsInitializesFreshBillingSchema proves the consumer-facing
// seam owns the migration source and creates a new configured billing schema.
// The shared integration database is already provisioned, so this test uses a
// unique schema and removes its exact ledger rows afterward; no AuthKit
// migration source or migratekit call appears in the consumer-facing package.
func TestApplyMigrationsInitializesFreshBillingSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.SharedSuperuserPGXPool(t)
	schema := fmt.Sprintf("openrails_embed_%d", time.Now().UnixNano())
	ident := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ident+" CASCADE")
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM public.migrations WHERE app = $1 AND database = 'postgres' AND schema = $2`,
			config.MigratekitApp, schema)
	})

	require.NoError(t, ApplyMigrations(ctx, pool, schema))

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT to_regnamespace($1) IS NOT NULL", schema).Scan(&exists))
	require.True(t, exists)

	var tableExists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT to_regclass($1) IS NOT NULL", schema+".merchants").Scan(&tableExists))
	require.True(t, tableExists)

	var riverExists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT to_regclass('public.river_job') IS NOT NULL").Scan(&riverExists))
	require.True(t, riverExists, "managed OpenRails initialization also owns River's schema")
}
