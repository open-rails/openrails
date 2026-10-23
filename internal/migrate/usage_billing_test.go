package migrate

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"
)

func TestUsageBillingMigrationPreservesExistingCreditLimits(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	schema := "usage_migrate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	defer func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	}()
	chain, err := loadMigrations(schema)
	require.NoError(t, err)
	before := -1
	for i, m := range chain {
		if strings.HasPrefix(m.Name, "0114_") {
			before = i
		}
	}
	require.Positive(t, before)
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer migrator.Close()
	migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, migrator.ApplyMigrations(ctx, chain[:before]))
	sql := func(s string) string { return strings.ReplaceAll(s, "billing.", schema+".") }
	merchant, customer := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'usage-migrate','active');`), merchant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`), merchant, customer)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.money_settings(merchant_id,customer_id,currency,billing_mode,credit_limit_amount) VALUES($1,$2,'USD','prepaid',0),($1,$2,'EUR','arrears',100)`), merchant, customer)
	require.NoError(t, err)
	require.NoError(t, migrator.ApplyMigrations(ctx, chain))
	var allExplicit bool
	require.NoError(t, pool.QueryRow(ctx, sql(`SELECT bool_and(credit_limit_override) FROM billing.money_settings`)).Scan(&allExplicit))
	require.True(t, allExplicit, "old zero as well as nonzero limits stay explicit")
	_, err = pool.Exec(ctx, sql(`INSERT INTO billing.money_settings(merchant_id,customer_id,currency,billing_mode) VALUES($1,$2,'JPY','prepaid')`), merchant, customer)
	require.NoError(t, err)
	var inherited bool
	require.NoError(t, pool.QueryRow(ctx, sql(`SELECT NOT credit_limit_override FROM billing.money_settings WHERE currency='JPY'`)).Scan(&inherited))
	require.True(t, inherited, "only new rows default to inheriting the merchant policy")
}
