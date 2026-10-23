//go:build e2e && integration

package ci_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

func TestUsageBillingMigrationPreservesExistingCreditLimits(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_E2E_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_E2E_DSN must point at a disposable PostgreSQL server")
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	schema := "usage_migrate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	defer func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	}()
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	before := fstest.MapFS{}
	for _, name := range files {
		if name >= "0114_" {
			continue
		}
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		before[name] = &fstest.MapFile{Data: body}
	}
	require.NotEmpty(t, before)
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Chain: before}))
	sql := func(s string) string { return strings.ReplaceAll(s, "billing.", schema+".") }
	merchant, customer := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'usage-migrate','active');`), merchant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`), merchant, customer)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.money_settings(merchant_id,customer_id,currency,billing_mode,credit_limit_amount) VALUES($1,$2,'USD','prepaid',0),($1,$2,'EUR','arrears',100)`), merchant, customer)
	require.NoError(t, err)
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema}))
	var allExplicit bool
	require.NoError(t, pool.QueryRow(ctx, sql(`SELECT bool_and(credit_limit_override) FROM billing.money_settings`)).Scan(&allExplicit))
	require.True(t, allExplicit, "old zero as well as nonzero limits stay explicit")
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.money_settings(merchant_id,customer_id,currency,billing_mode) VALUES($1,$2,'JPY','prepaid')`), merchant, customer)
	require.NoError(t, err)
	var inherited bool
	require.NoError(t, pool.QueryRow(ctx, sql(`SELECT NOT credit_limit_override FROM billing.money_settings WHERE currency='JPY'`)).Scan(&inherited))
	require.True(t, inherited, "only new rows default to inheriting the merchant policy")
}
