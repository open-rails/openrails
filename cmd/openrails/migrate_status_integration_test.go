//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
)

func TestMigrateStatusReportsContentDriftAndExitsNonzero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database := dbtest.SharedSuperuserPGXPool(t)
	schema := fmt.Sprintf("status_%d", time.Now().UnixNano())
	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{
		URL: dbtest.SharedSuperuserDSN(t), Schema: schema,
	}}
	t.Cleanup(func() {
		_, _ = database.Exec(context.Background(),
			"DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_, _ = database.Exec(context.Background(),
			`DELETE FROM public.migrations WHERE app = $1 AND database = 'postgres' AND schema = $2`,
			config.MigratekitApp, schema)
	})

	require.NoError(t, migrate.RunPostgres(ctx, cfg))
	clean, err := executeMigrateStatus(ctx, cfg, false)
	require.NoError(t, err)
	require.Contains(t, clean, "exact: true")

	_, err = database.Exec(ctx,
		`UPDATE public.migrations SET content_sha256 = $1
		  WHERE app = $2 AND database = 'postgres' AND schema = $3 AND name = '1'`,
		"mismatched-body", config.MigratekitApp, schema)
	require.NoError(t, err)

	raw, err := executeMigrateStatus(ctx, cfg, true)
	require.ErrorIs(t, err, migrate.ErrMigrationStatusDrift)
	var report migrate.PostgresStatus
	require.NoError(t, json.Unmarshal([]byte(raw), &report))
	require.False(t, report.Exact)
	require.NotEmpty(t, report.Drift)
	require.Equal(t, "content_hash_mismatch", report.Drift[0].Kind)
}

func executeMigrateStatus(ctx context.Context, cfg *config.Config, jsonOutput bool) (string, error) {
	cmd := newMigrateStatusCmd()
	cmd.SetContext(context.WithValue(ctx, config.ConfigContextKey, cfg))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if jsonOutput {
		cmd.SetArgs([]string{"--json"})
	}
	err := cmd.Execute()
	return out.String(), err
}
