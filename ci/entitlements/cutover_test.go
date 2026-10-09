//go:build e2e && integration

package entitlements_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/migrate"
)

// The cutover to product access runs only as its preflight listed and an
// operator approved: it lists exactly whose access changes, refuses while a
// change is unapproved, and then converts per-key windows to the products
// that grant them.
func TestAccessCutoverAppliesOnlyTheApprovedPreflight(t *testing.T) {
	ctx := t.Context()
	pool, schema := cutoverSchema(t, os.Getenv("OPENRAILS_E2E_DSN"))
	empty, err := migrate.AccessCutoverPreflight(ctx, pool, schema, "")
	require.NoError(t, err, "the preflight brings a fresh database to the cutover")
	require.Empty(t, empty.Changes)

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, strings.ReplaceAll(sql, "billing.", pgx.Identifier{schema}.Sanitize()+"."), args...)
		require.NoError(t, err)
	}
	merchant, buyer, comped, product, price, payment := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	bought := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Microsecond)
	exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'cutover','active')`, merchant)
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2),($1,$3)`, merchant, buyer, comped)
	exec(`INSERT INTO billing.products(merchant_id,id,key,display_name,tier_rank,archived) VALUES($1,$2,'course-bundle','Course bundle',0,false)`, merchant, product)
	exec(`INSERT INTO billing.product_entitlements(merchant_id,product_id,entitlement,added_at,added_by)
		VALUES($1,$2,'course:101',$3,'test'),($1,$2,'course:102',$3,'test'),($1,$2,'course:103',$3,'test')`, merchant, product, bought.Add(-time.Hour))
	exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived) VALUES($1,$2,$3,'buy',1000000,'USD',false)`, merchant, price, product)
	exec(`INSERT INTO billing.payments(merchant_id,id,customer_id,price_id,channel,transaction_id,amount,list_amount,currency,status,purchased_at)
		VALUES($1,$2,$3,$4,'manual','bundle-1',1000000,1000000,'USD','completed',$5)`, merchant, payment, buyer, price, bought)
	window := func(customer uuid.UUID, sourceType string, source uuid.UUID, productID *uuid.UUID, keys ...string) {
		t.Helper()
		grant := uuid.New()
		var paymentID *uuid.UUID
		if sourceType == "purchase" {
			paymentID = &source
		}
		spec := `{"entitlements":["` + strings.Join(keys, `","`) + `"]}`
		exec(`INSERT INTO billing.grants(merchant_id,id,customer_id,product_id,payment_id,kind,source_type,source_id,event,spec_snapshot,starts_at)
			VALUES($1,$2,$3,$4,$5,'entitlement',$6,$7::text,'grant',$8::jsonb,$9)`, merchant, grant, customer, productID, paymentID, sourceType, source.String(), spec, bought)
		for _, key := range keys {
			exec(`INSERT INTO billing.entitlements(merchant_id,customer_id,entitlement,starts_at,source_id,source_type,grant_id)
				VALUES($1,$2,$3,$4,$5,$6,$7)`, merchant, customer, key, bought, source, sourceType, grant)
		}
	}
	// The buyer kept course:099, which the bundle has since dropped, and lacks
	// course:103, which it has since added. The comp's key has no product.
	window(buyer, "purchase", payment, &product, "course:101", "course:102", "course:099")
	window(comped, "admin", uuid.New(), nil, "vip")

	report, err := migrate.AccessCutoverPreflight(ctx, pool, schema, "")
	require.NoError(t, err)
	type change struct {
		customer uuid.UUID
		key      string
		change   string
	}
	var listed []change
	for _, c := range report.Changes {
		require.Equal(t, merchant, c.MerchantID)
		require.False(t, c.Approved)
		listed = append(listed, change{c.CustomerID, c.Entitlement, c.Change})
	}
	require.ElementsMatch(t, []change{{buyer, "course:099", "lost"}, {buyer, "course:103", "gained"}, {comped, "vip", "lost"}}, listed)
	require.True(t, slices.ContainsFunc(report.Notes, func(n migrate.AccessNote) bool {
		return n.CustomerID == comped && n.Note == "unmapped" && slices.Equal(n.Entitlements, []string{"vip"})
	}), "%+v", report.Notes)

	err = migrateAll(ctx, pool, schema)
	require.ErrorContains(t, err, "unapproved access changes", "the cutover refuses a change no one approved")
	var converted int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema}.Sanitize()+".product_access").Scan(&converted))
	require.Zero(t, converted, "a refused cutover leaves nothing converted")

	approved, err := migrate.AccessCutoverPreflight(ctx, pool, schema, "operator@example.test")
	require.NoError(t, err)
	require.Zero(t, approved.Unapproved())
	require.NoError(t, migrateAll(ctx, pool, schema))
	_, err = migrate.AccessCutoverPreflight(ctx, pool, schema, "")
	require.ErrorContains(t, err, "already ran")

	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	held := func(customer uuid.UUID, keys ...string) map[string]bool {
		t.Helper()
		rows, err := database.GenDirectory().CheckDerivedEntitlements(ctx, gen.CheckDerivedEntitlementsParams{MerchantID: merchant, CustomerID: customer, AtTime: time.Now(), Entitlements: keys})
		require.NoError(t, err)
		out := map[string]bool{}
		for _, row := range rows {
			out[row.Entitlement] = row.HasAccess
		}
		return out
	}
	require.Equal(t, map[string]bool{"course:101": true, "course:102": true, "course:103": true, "course:099": false}, held(buyer, "course:101", "course:102", "course:103", "course:099"))
	require.Equal(t, map[string]bool{"vip": false}, held(comped, "vip"))
	var access []string
	rows, err := pool.Query(ctx, "SELECT source_type||':'||source_id FROM "+pgx.Identifier{schema}.Sanitize()+".product_access WHERE customer_id=$1", buyer)
	require.NoError(t, err)
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		access = append(access, row)
	}
	require.Equal(t, []string{"purchase:" + payment.String()}, access, "the purchase is one window of its product")
}

// cutoverSchema is a pool and an empty schema of its own, dropped after t.
func cutoverSchema(t *testing.T, dsn string) (*pgxpool.Pool, string) {
	t.Helper()
	dsn = strings.TrimSpace(dsn)
	require.NotEmpty(t, dsn, "OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	schema := "e2e_cutover_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	return pool, schema
}

func migrateAll(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	return openrails.Migrate(ctx, pool, openrails.Config{Schema: schema, RiverSchema: schema})
}
