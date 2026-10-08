package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"
)

func TestEntitlementNamesMigrationPreservesPurchasedWindows(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required for the populated migration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "names_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	migrations, err := loadMigrations(schema)
	require.NoError(t, err)
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer migrator.Close()
	migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:8]))
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, strings.ReplaceAll(sql, "billing.", schema+"."), args...)
		require.NoError(t, err)
	}
	merchant, customer, product := uuid.New(), uuid.New(), uuid.New()
	indefinite, bounded, payment := uuid.New(), uuid.New(), uuid.New()
	const legacy = `{"service:z":null,"permanent":0,"article:42":48}`
	const names = `["article:42","permanent","service:z"]`
	exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'names','active')`, merchant)
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, merchant, customer)
	exec(`INSERT INTO billing.products(merchant_id,id,key,display_name,tier_rank,archived,entitlements_spec)
		VALUES($1,$2,'names','Opaque names',0,false,$3::jsonb)`, merchant, product, legacy)
	exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived)
		VALUES($1,$2,$3,'permanent',1000000,'USD',false)`, merchant, indefinite, product)
	exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived,access_duration_hours)
		VALUES($1,$2,$3,'rental',1000000,'USD',false,72)`, merchant, bounded, product)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, row := range []struct {
		id, price uuid.UUID
		key       string
		snapshot  *string
	}{
		{payment, indefinite, "timed", new(legacy)},
		{uuid.New(), bounded, "bounded", new(legacy)},
		{uuid.New(), indefinite, "unknown", nil},
		{uuid.New(), indefinite, "empty", new("{}")},
	} {
		exec(`INSERT INTO billing.payments(merchant_id,id,customer_id,price_id,channel,transaction_id,amount,list_amount,currency,status,purchased_at,entitlements_spec_snapshot)
			VALUES($1,$2,$3,$4,'manual',$5,1000000,1000000,'USD','completed',$6,$7::jsonb)`, merchant, row.id, customer, row.price, row.key, start, row.snapshot)
	}
	grant := uuid.New()
	end := start.Add(48 * time.Hour)
	exec(`INSERT INTO billing.grants(merchant_id,id,customer_id,product_id,payment_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at)
		VALUES($1,$2,$3,$4,$5::uuid,'entitlement','purchase',$5::text,'grant','{"entitlements":["article:42"]}',$6,$7)`, merchant, grant, customer, product, payment, start, end)
	var catalogRevision, productRevision int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT catalog_revision FROM "+schema+".merchants WHERE id=$1", merchant).Scan(&catalogRevision))
	require.NoError(t, pool.QueryRow(ctx, "SELECT revision FROM "+schema+".products WHERE id=$1", product).Scan(&productRevision))
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations))
	var raw string
	var revision int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT entitlements::text,revision FROM "+schema+".products WHERE id=$1", product).Scan(&raw, &revision))
	require.JSONEq(t, names, raw)
	require.Equal(t, productRevision, revision, "normalization is not a product edit")
	require.NoError(t, pool.QueryRow(ctx, "SELECT catalog_revision FROM "+schema+".merchants WHERE id=$1", merchant).Scan(&revision))
	require.Equal(t, catalogRevision, revision)
	var historical *string
	require.NoError(t, pool.QueryRow(ctx, "SELECT entitlements_snapshot::text,legacy_entitlement_hours::text FROM "+schema+".payments WHERE id=$1", payment).Scan(&raw, &historical))
	require.JSONEq(t, names, raw)
	require.NotNil(t, historical)
	require.JSONEq(t, `{"article:42":48}`, *historical)
	require.NoError(t, pool.QueryRow(ctx, "SELECT legacy_entitlement_hours::text FROM "+schema+".payments WHERE transaction_id='bounded'").Scan(&historical))
	require.Nil(t, historical, "a price-wide duration already overrode feature hours")
	var unknown *string
	require.NoError(t, pool.QueryRow(ctx, "SELECT entitlements_snapshot::text FROM "+schema+".payments WHERE transaction_id='unknown'").Scan(&unknown))
	require.Nil(t, unknown)
	require.NoError(t, pool.QueryRow(ctx, "SELECT entitlements_snapshot::text FROM "+schema+".payments WHERE transaction_id='empty'").Scan(&raw))
	require.JSONEq(t, `[]`, raw, "accepted empty must not become unknown")
	var retainedStart, retainedEnd time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT starts_at,ends_at FROM "+schema+".grants WHERE id=$1", grant).Scan(&retainedStart, &retainedEnd))
	require.True(t, start.Equal(retainedStart) && end.Equal(retainedEnd), "the previously purchased window is unchanged")
}
