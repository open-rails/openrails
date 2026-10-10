//go:build e2e && integration

package entitlements_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// timing is p50 and p99 of a path's wall time, through the embedded client.
type timing struct{ p50, p99 time.Duration }

func measure(t *testing.T, runs int, before func(), fn func()) timing {
	t.Helper()
	samples := make([]time.Duration, 0, runs)
	for range runs {
		if before != nil {
			before()
		}
		start := time.Now()
		fn()
		samples = append(samples, time.Since(start))
	}
	slices.Sort(samples)
	return timing{samples[len(samples)/2], samples[min(len(samples)-1, len(samples)*99/100)]}
}

func (tm timing) String() string {
	return fmt.Sprintf("p50 %8.2f ms  p99 %8.2f ms", float64(tm.p50.Microseconds())/1000, float64(tm.p99.Microseconds())/1000)
}

// The read and write paths at 1k, 10k and 50k held products per customer, in
// a merchant with 50,000 other customers. Set OPENRAILS_SCALE_BENCH=1 to run;
// the numbers are logged, not asserted (TestHeavyBuyerReadsStayBounded guards
// the work in CI).
func TestEntitlementScaleBenchmark(t *testing.T) {
	if os.Getenv("OPENRAILS_SCALE_BENCH") == "" {
		t.Skip("set OPENRAILS_SCALE_BENCH=1 to measure the entitlement paths at scale")
	}
	f := newFixture(t)
	ctx := t.Context()
	client := f.client
	start := f.clock.Now().Add(-time.Hour)
	sizes := []int{1_000, 10_000, 50_000}
	whales := make([]billing.CustomerID, len(sizes))
	for i, n := range sizes {
		whales[i] = billing.CustomerID(uuid.New())
		f.seed(whales[i], fmt.Sprintf("w%d:", i), n, 1, start)
	}
	f.crowd(50_000)
	common, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "common", DisplayName: "Common", Entitlements: []string{"common:1"}})
	require.NoError(t, err)
	s := pgx.Identifier{f.schema}.Sanitize()
	_, err = f.pool.Exec(ctx, `WITH holders AS (SELECT id FROM `+s+`.customers WHERE merchant_id = $1 LIMIT 20000
	), gr AS (
		INSERT INTO `+s+`.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, event, starts_at, actor, grant_reason)
		SELECT $1, id, $2, 'access', 'grant', 'common:' || id, 'grant', $3, 'seed', 'comp' FROM holders RETURNING id, customer_id, source_id
	) INSERT INTO `+s+`.product_access (merchant_id, customer_id, product_id, grant_id, source_type, source_id, starts_at)
	SELECT $1, customer_id, $2, id, 'grant', source_id, $3 FROM gr`, client.MerchantID().UUID(), common.ID.UUID(), start)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, "VACUUM ANALYZE "+s+".product_access, "+s+".product_entitlements, "+s+".customers, "+s+".products")
	require.NoError(t, err)
	// invalidate lets the last background rebuild finish, then makes the
	// cache stale.
	invalidate := func(c billing.CustomerID) func() {
		return func() {
			f.cached(c)
			_, err := f.pool.Exec(ctx, "UPDATE "+s+".customers SET access_version = access_version + 1 WHERE merchant_id = $1 AND id = $2", client.MerchantID().UUID(), c.UUID())
			require.NoError(t, err)
		}
	}
	var report strings.Builder
	row := func(path string, n int, tm timing) { fmt.Fprintf(&report, "| %-44s | %6d | %s |\n", path, n, tm) }
	for i, n := range sizes {
		whale := whales[i]
		prefix := fmt.Sprintf("w%d:", i)
		keys := make([]string, billing.MaxBatchItems)
		for k := range keys {
			keys[k] = fmt.Sprintf("%s%06d:1", prefix, (k*97)%n+1)
		}
		read := func(p billing.EntitlementListParams) func() {
			p.CustomerIDs = []billing.CustomerID{whale}
			return func() {
				_, err := client.ListEntitlements(ctx, p)
				require.NoError(t, err)
			}
		}
		exact := read(billing.EntitlementListParams{Entitlements: keys})
		held := read(billing.EntitlementListParams{Prefix: prefix + "0", PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}})
		held()
		f.cached(whale)
		row("ListEntitlements exact 100, stale cache (live)", n, measure(t, 5, invalidate(whale), exact))
		row("ListEntitlements prefix page, stale cache (live)", n, measure(t, 5, invalidate(whale), held))
		if os.Getenv("OPENRAILS_SCALE_BENCH_PLANS") != "" {
			plan, used := f.executedPlan("ListDerivedEntitlementsPage")
			t.Logf("%d held: live prefix read touched %d blocks: %s", n, used, plan)
		}
		row("cache rebuild (stale read to valid cache)", n, measure(t, 5, invalidate(whale), func() { held(); f.cached(whale) }))
		f.cached(whale)
		row("ListEntitlements exact 100, cached", n, measure(t, 100, nil, exact))
		row("ListEntitlements prefix page, cached", n, measure(t, 100, nil, held))
		cursor := ""
		row("ListEntitlements page 100", n, measure(t, 50, nil, func() {
			page, err := client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{whale}, PageRequest: billing.PageRequest{Limit: 100, Cursor: cursor}})
			require.NoError(t, err)
			cursor = page.Next
		}))
		accessCursor := ""
		row("ListProductAccess page 100", n, measure(t, 50, nil, func() {
			page, err := client.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{whale}, PageRequest: billing.PageRequest{Limit: 100, Cursor: accessCursor}})
			require.NoError(t, err)
			accessCursor = page.Next
		}))
		products, err := client.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{whale}, PageRequest: billing.PageRequest{Limit: billing.MaxBatchItems}})
		require.NoError(t, err)
		ids := make([]billing.ProductID, len(products.Items))
		for k, item := range products.Items {
			ids[k] = item.ProductID
		}
		row("ListProductAccess of 100 products, live", n, measure(t, 100, nil, func() {
			_, err := client.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{whale}, ProductIDs: ids, LiveOnly: true})
			require.NoError(t, err)
		}))
		row("GetCustomer (console)", n, measure(t, 20, nil, func() {
			_, err := client.GetCustomer(ctx, whale)
			require.NoError(t, err)
		}))
		day := 24
		var granted []billing.ProductAccessGrant
		row("CreateProductAccess (grant, hours)", n, measure(t, 20, nil, func() {
			out, err := client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: whale, ProductID: common.ID, Hours: &day}}})
			require.NoError(t, err)
			granted = append(granted, out...)
		}))
		next := 0
		row("DeleteProductAccess (revoke)", n, measure(t, 20, nil, func() {
			require.NoError(t, client.DeleteProductAccess(ctx, whale, granted[next].ID))
			next++
		}))
		row("ListEntitlements prefix page after a write", n, measure(t, 5, func() {
			held()
			f.cached(whale)
			_, err := client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: whale, ProductID: common.ID, Hours: &day}}})
			require.NoError(t, err)
		}, held))
	}
	reverse := ""
	row("ListEntitlements holders page 100 (20k hold)", 20_000, measure(t, 50, nil, func() {
		page, err := client.ListEntitlements(ctx, billing.EntitlementListParams{Entitlements: []string{"common:1"}, PageRequest: billing.PageRequest{Limit: 100, Cursor: reverse}})
		require.NoError(t, err)
		reverse = page.Next
	}))
	edits := 0
	row("UpdateProduct key edit (20k holders)", 20_000, measure(t, 10, nil, func() {
		edits++
		_, err := client.UpdateProduct(ctx, common.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"common:1", fmt.Sprintf("common:extra:%d", edits)})})
		require.NoError(t, err)
	}))
	t.Logf("\n| path | held | time |\n|---|---|---|\n%s", report.String())
}

// The cutover at scale: per-key windows of 50,000 customers (10 keys each)
// and one customer with 50,000, converted by the preflight's dry run and by
// the migration.
func TestAccessCutoverScaleBenchmark(t *testing.T) {
	if os.Getenv("OPENRAILS_SCALE_BENCH") == "" {
		t.Skip("set OPENRAILS_SCALE_BENCH=1 to measure the cutover at scale")
	}
	ctx := t.Context()
	pool, schema := cutoverSchema(t, os.Getenv("OPENRAILS_E2E_DSN"))
	database := openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}
	_, err := openrails.AccessCutoverPreflight(ctx, pool, database, "")
	require.NoError(t, err)
	s := pgx.Identifier{schema}.Sanitize()
	merchant := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	at := time.Now().UTC().Add(-24 * time.Hour)
	exec(`INSERT INTO `+s+`.merchants(id,slug,status) VALUES($1,'cutover-bench','active')`, merchant)
	exec(`SELECT set_config('openrails.catalog_batch_merchant_id', $1, false)`, merchant.String())
	// 5,000 products of 10 keys and one of 50,000.
	exec(`INSERT INTO `+s+`.products(merchant_id,id,key,display_name,tier_rank,archived)
		SELECT $1, uuidv7(), 'p-' || g, 'P', 0, false FROM generate_series(1, 5001) g`, merchant)
	exec(`INSERT INTO `+s+`.product_entitlements(merchant_id,product_id,entitlement,added_at,added_by)
		SELECT p.merchant_id, p.id, p.key || ':' || k, $2, 'seed' FROM `+s+`.products p, generate_series(1, 10) k WHERE p.merchant_id = $1 AND p.key <> 'p-5001'`, merchant, at.Add(-time.Hour))
	exec(`INSERT INTO `+s+`.product_entitlements(merchant_id,product_id,entitlement,added_at,added_by)
		SELECT p.merchant_id, p.id, 'big:' || k, $2, 'seed' FROM `+s+`.products p, generate_series(1, 50000) k WHERE p.merchant_id = $1 AND p.key = 'p-5001'`, merchant, at.Add(-time.Hour))
	exec(`INSERT INTO `+s+`.prices(merchant_id,id,product_id,key,amount,currency,archived)
		SELECT merchant_id, uuidv7(), id, 'buy', 1000000, 'USD', false FROM `+s+`.products WHERE merchant_id = $1`, merchant)
	exec(`INSERT INTO `+s+`.customers(merchant_id,id) SELECT $1, gen_random_uuid() FROM generate_series(1, 50001)`, merchant)
	// One purchase per customer: the 50,001st buys the big product.
	exec(`WITH c AS (SELECT id, row_number() OVER (ORDER BY id) AS n FROM `+s+`.customers WHERE merchant_id = $1
	), pp AS (SELECT p.id AS product_id, pr.id AS price_id, row_number() OVER (ORDER BY p.key) AS n FROM `+s+`.products p JOIN `+s+`.prices pr ON pr.product_id = p.id WHERE p.merchant_id = $1
	) INSERT INTO `+s+`.payments(merchant_id,id,customer_id,price_id,channel,transaction_id,amount,list_amount,currency,status,purchased_at)
	SELECT $1, uuidv7(), c.id, pp.price_id, 'manual', 'bench-' || c.n, 1000000, 1000000, 'USD', 'succeeded', $2
	FROM c JOIN pp ON pp.n = CASE WHEN c.n = 50001 THEN (SELECT max(n) FROM pp) ELSE 1 + c.n % 5000 END`, merchant, at)
	exec(`INSERT INTO `+s+`.grants(merchant_id,id,customer_id,product_id,payment_id,kind,source_type,source_id,event,spec_snapshot,starts_at)
		SELECT pay.merchant_id, uuidv7(), pay.customer_id, pr.product_id, pay.id, 'entitlement', 'purchase', pay.id::text, 'grant', '{"entitlements":["seed"]}', pay.purchased_at
		FROM `+s+`.payments pay JOIN `+s+`.prices pr ON pr.id = pay.price_id WHERE pay.merchant_id = $1`, merchant)
	exec(`INSERT INTO `+s+`.entitlements(merchant_id,customer_id,entitlement,starts_at,source_id,source_type,grant_id)
		SELECT g.merchant_id, g.customer_id, pe.entitlement, g.starts_at, g.payment_id, 'purchase', g.id
		FROM `+s+`.grants g JOIN `+s+`.product_entitlements pe ON pe.product_id = g.product_id WHERE g.merchant_id = $1`, merchant)
	exec("VACUUM ANALYZE " + s + ".entitlements, " + s + ".grants, " + s + ".payments, " + s + ".product_entitlements")
	var windows int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+s+".entitlements").Scan(&windows))
	began := time.Now()
	report, err := openrails.AccessCutoverPreflight(ctx, pool, database, "")
	require.NoError(t, err)
	preflight := time.Since(began)
	require.Empty(t, report.Changes)
	began = time.Now()
	require.NoError(t, migrateAll(ctx, pool, schema))
	t.Logf("cutover of %d per-key windows (50,001 customers, one with 50,000 keys): preflight %s, migration %s", windows, preflight, time.Since(began))
}
