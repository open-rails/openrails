//go:build integration

package controlplane

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func newExternalPostgresTestPool(t *testing.T, adminDSN, prefix string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("%s_%d", strings.ToLower(prefix), time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
	require.NoError(t, err)

	testCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	testCfg.ConnConfig.Config.Database = dbName
	testPool, err := pgxpool.NewWithConfig(ctx, testCfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		testPool.Close()
		_, _ = adminPool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", dbName)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize())
		adminPool.Close()
	})

	return testPool
}
