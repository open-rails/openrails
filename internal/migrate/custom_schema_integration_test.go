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
	cfg := &config.Config{DB: &config.DBConfig{URL: dsn, Schema: schema}}
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
	method := uuid.New()
	_, err = conn.Exec(ctx, "INSERT INTO "+schema+".payment_methods(id,merchant_id,customer_id,psp_id,rail,rail_customer_ref,initial_transaction_id) VALUES($1,$2,$3,$4,'nmi','relocation-vault','fixture')", method, mid, cid, psp)
	require.NoError(t, err)
	var product, subscription uuid.UUID
	for _, policy := range []string{"provider", "provider_dunning", "engine"} {
		product = uuid.New()
		_, err = conn.Exec(ctx, "INSERT INTO "+schema+".products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Policy')", product, mid, product.String())
		require.NoError(t, err)
		err = conn.QueryRow(ctx, "INSERT INTO "+schema+".subscriptions(merchant_id,customer_id,product_id,psp_id,rail,collection_policy,payment_method_id) VALUES($1,$2,$3,$4,'nmi',$5,$6) RETURNING id", mid, cid, product, psp, policy, method).Scan(&subscription)
		require.NoError(t, err, "schema relocation preserves collection policy")
	}
	_, err = conn.Exec(ctx, "INSERT INTO "+schema+".subscriptions(merchant_id,customer_id,product_id,psp_id,rail,collection_policy,payment_method_id) VALUES($1,$2,$3,$4,'nmi',$5,$6)", mid, cid, product, psp, schema, method)
	require.ErrorContains(t, err, "subscriptions_collection_policy_check")

	// Removing a credential must not cancel the agreement or invent a remote ID.
	_, err = conn.Exec(ctx, "UPDATE "+schema+".subscriptions SET status='active',current_period_starts_at=now()-interval '1 month',current_period_ends_at=now() WHERE id=$1", subscription)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "DELETE FROM "+schema+".payment_methods WHERE id=$1", method)
	require.NoError(t, err)
	var linked *uuid.UUID
	require.NoError(t, conn.QueryRow(ctx, "SELECT payment_method_id FROM "+schema+".subscriptions WHERE id=$1", subscription).Scan(&linked))
	require.Nil(t, linked)
	_, err = conn.Exec(ctx, "UPDATE "+schema+".subscriptions SET collection_policy='provider' WHERE id=$1", subscription)
	require.ErrorContains(t, err, "collection policy is immutable")

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
