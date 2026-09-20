//go:build integration

package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
)

func TestCustomSchemaKeepsBillingValuesAndRestoreFunctions(t *testing.T) {
	ctx := t.Context()
	dsn := dbtest.SharedSuperuserDSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close(context.Background())) })
	schema := "billing_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, err := conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		_, err = conn.Exec(context.Background(), "DELETE FROM public.migrations WHERE app=$1 AND schema=$2", config.MigratekitApp, schema)
		require.NoError(t, err)
	})
	cfg := &config.Config{Env: "development", DB: &config.DBConfig{URL: dsn, Schema: schema}}
	require.NoError(t, migrate.RunPostgres(ctx, cfg))
	require.NoError(t, migrate.RunPostgres(ctx, cfg), "fresh schema replay is exact")
	status, err := migrate.InspectPostgres(ctx, cfg)
	require.NoError(t, err)
	require.True(t, status.Exact, status.Report())

	mid, cid, psp := uuid.New(), uuid.New(), uuid.New()
	_, err = conn.Exec(ctx, "INSERT INTO "+schema+".merchants(id,slug) VALUES($1,$2)", mid, "relocation-"+mid.String())
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "INSERT INTO "+schema+".customers(id,merchant_id) VALUES($1,$2)", cid, mid)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "INSERT INTO "+schema+".psps(id,merchant_id,rail,environment,account_id) VALUES($1,$2,'nmi','test',$3)", psp, mid, psp.String())
	require.NoError(t, err)
	for _, driver := range []string{"openrails", "provider"} {
		_, err = conn.Exec(ctx, "INSERT INTO "+schema+".payment_methods(merchant_id,customer_id,psp_id,rail,initial_transaction_id,rebill_driver,rail_customer_ref) VALUES($1,$2,$3,'nmi','fixture',$4,$4)", mid, cid, psp, driver)
		require.NoError(t, err, "schema relocation must preserve the billing enum")
	}
	_, err = conn.Exec(ctx, "INSERT INTO "+schema+".payment_methods(merchant_id,customer_id,psp_id,rail,initial_transaction_id,rebill_driver,rail_customer_ref) VALUES($1,$2,$3,'nmi','fixture',$4,$4)", mid, cid, psp, schema)
	require.ErrorContains(t, err, "payment_methods_rebill_driver_check")

	var badPaths, relocatedPaths int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON p.pronamespace=n.oid
WHERE n.nspname=$1 AND EXISTS(SELECT 1 FROM unnest(p.proconfig) AS value WHERE value LIKE 'search_path=%' AND value LIKE '%openrails%')`, schema).Scan(&badPaths))
	require.Zero(t, badPaths, "security-definer search paths must use the relocated schema")
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON p.pronamespace=n.oid WHERE n.nspname=$1 AND EXISTS(SELECT 1 FROM unnest(p.proconfig) AS value WHERE value LIKE 'search_path=%' AND position($1 in value)>0)`, schema).Scan(&relocatedPaths))
	require.Positive(t, relocatedPaths)
	// begin_billing_restore resolves its own merchants table through regclass.
	// An empty restore proves the function body follows the custom namespace.
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(context.Background())
	emptyMerchant := uuid.New()
	_, err = tx.Exec(ctx, "INSERT INTO "+schema+".merchants(id,slug) VALUES($1,$2)", emptyMerchant, "empty-"+emptyMerchant.String())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "SELECT set_config('app.merchant_id',$1,true)", emptyMerchant.String())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "SELECT "+schema+".begin_billing_restore($1)", emptyMerchant)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "SELECT "+schema+".finish_billing_restore($1,$2,0)", emptyMerchant, strings.Repeat("a", 64))
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
}
