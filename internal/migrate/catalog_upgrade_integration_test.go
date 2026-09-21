//go:build integration

package migrate_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

func TestCreatorCatalogUpgradePreservesExistingPurchases(t *testing.T) {
	ctx := t.Context()
	pool := dbtest.SharedSuperuserPGXPool(t)
	schema := "catalog_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(migrations), 2)
	baseline := migrations[0]
	require.Equal(t, postgresmigrations.BaselineName, baseline.Name)
	baseline.Content, err = postgresmigrations.RewriteSchema(baseline.Content, schema)
	require.NoError(t, err)
	old, err := migratekit.NewPostgresFromPGXPool(pool, config.MigratekitApp)
	require.NoError(t, err)
	defer old.Close()
	require.NoError(t, old.WithSchema(schema).ApplyMigrations(ctx, []migratekit.Migration{baseline}))
	var before json.RawMessage
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(m) FROM public.migrations m WHERE app=$1 AND schema=$2 AND sequence='1'`, config.MigratekitApp, schema).Scan(&before))
	mid, customer, product, price, payment, grant := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "SET LOCAL search_path TO "+pgx.Identifier{schema}.Sanitize()+",public")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id',$1,true)`, mid.String())
	require.NoError(t, err)
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO merchants(id,slug) VALUES($1,'existing-merchant')`, []any{mid}},
		{`INSERT INTO customers(merchant_id,id) VALUES($1,$2)`, []any{mid, customer}},
		{`INSERT INTO products(id,merchant_id,key,display_name) VALUES($1,$2,'existing-product','Existing product')`, []any{product, mid}},
		{`INSERT INTO prices(id,merchant_id,product_id,amount,currency) VALUES($1,$2,$3,5000000,'USD')`, []any{price, mid, product}},
		{`INSERT INTO payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status) VALUES($1,$2,$3,$4,'manual','existing-payment',5000000,5000000,'USD','completed')`, []any{payment, mid, customer, price}},
		{`INSERT INTO grants(id,merchant_id,customer_id,product_id,payment_id,kind,source_type,source_id) VALUES($1,$2,$3,$4,$5,'ownership','purchase','existing-grant')`, []any{grant, mid, customer, product, payment}},
	} {
		_, err = tx.Exec(ctx, seed.sql, seed.args...)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit(ctx))
	for range 2 {
		require.NoError(t, migrate.ApplyPostgresMigrations(ctx, pool, migrate.Options{Schema: schema, HostRiver: true}))
	}
	var after json.RawMessage
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(m) FROM public.migrations m WHERE app=$1 AND schema=$2 AND sequence='1'`, config.MigratekitApp, schema).Scan(&after))
	require.JSONEq(t, string(before), string(after), "an additive catalog upgrade must not restamp its baseline")
	wrapped := db.WrapPool(pool, schema)
	var preserved int
	require.NoError(t, wrapped.QueryRow(ctx, `SELECT count(*) FROM openrails.grants g
		JOIN openrails.payments payment ON payment.id=g.payment_id AND payment.merchant_id=g.merchant_id
		JOIN openrails.prices price ON price.id=payment.price_id AND price.merchant_id=payment.merchant_id
		JOIN openrails.products product ON product.id=price.product_id AND product.merchant_id=price.merchant_id
		JOIN openrails.catalogs catalog ON catalog.id=product.catalog_id AND catalog.merchant_id=product.merchant_id
		WHERE g.id=$1 AND payment.id=$2 AND price.id=$3 AND product.id=$4 AND g.customer_id=$5
		AND g.merchant_id=$6 AND catalog.owner_subject IS NULL AND price.amount=5000000
		AND g.ends_at IS NULL AND g.event='grant'`, grant, payment, price, product, customer, mid).Scan(&preserved))
	require.Equal(t, 1, preserved, "existing purchase identities, terms and permanent access survive the default-catalog backfill")
}
