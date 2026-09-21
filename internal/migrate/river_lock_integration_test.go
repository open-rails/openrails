//go:build integration

package migrate

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"
)

func TestRiverMigrationsSerializeAcrossLibraries(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("OPENRAILS_TEST_DB_DSN is required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer admin.Close()
	name := "river_lock_" + uuid.NewString()[:8]
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err)
	}()
	cfg := admin.Config()
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 1
	// A host's unrelated search_path must not redirect a declared public fleet.
	cfg.ConnConfig.RuntimeParams["search_path"] = "profiles, public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()
	start := make(chan struct{})
	results := make(chan error, 8)
	for i := range 8 {
		go func() {
			<-start
			if i%2 == 0 {
				results <- authcore.ApplyMigrations(ctx, pool, "profiles")
			} else {
				results <- runRiverMigrationsPool(ctx, pool, "public")
			}
		}()
	}
	close(start)
	for range 8 {
		require.NoError(t, <-results)
	}
	var public, profiles bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('public.river_job') IS NOT NULL,to_regclass('profiles.river_job') IS NOT NULL").Scan(&public, &profiles))
	require.True(t, public)
	require.False(t, profiles, "the lock and migrations must address the same declared River schema")
}
