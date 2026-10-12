//go:build e2e && integration

package entitlements_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
)

// seed gives customer n products, each granting keys keys named
// <prefix><product>:<key>, through free grants valid from from. It writes the
// rows directly: building a whale through the API takes minutes.
func (f *fixture) seed(customer billing.CustomerID, prefix string, n, keys int, from time.Time) {
	f.t.Helper()
	ctx := f.t.Context()
	_, err := f.client.UpdateCustomer(ctx, customer, billing.UpdateCustomerParams{})
	require.NoError(f.t, err)
	s := pgx.Identifier{f.schema}.Sanitize()
	tx, err := f.pool.Begin(ctx)
	require.NoError(f.t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('openrails.catalog_batch_merchant_id', $1, true)", f.client.MerchantID().String())
	require.NoError(f.t, err)
	_, err = tx.Exec(ctx, `WITH p AS (
		INSERT INTO `+s+`.products (merchant_id, id, key, display_name, tier_rank, archived)
		SELECT $1, uuidv7(), 'seed-' || replace(gen_random_uuid()::text, '-', ''), 'Seeded', 0, false FROM generate_series(1, $3::int) g
		RETURNING id
	), numbered AS (SELECT id, row_number() OVER (ORDER BY id) AS n FROM p
	), k AS (
		INSERT INTO `+s+`.product_entitlements (merchant_id, product_id, entitlement, added_at, added_by)
		SELECT $1, numbered.id, $4::text || lpad(numbered.n::text, 6, '0') || ':' || kk, $6, 'seed'
		FROM numbered, generate_series(1, $5::int) kk
		RETURNING 1
	), gr AS (
		INSERT INTO `+s+`.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, event, starts_at, actor, grant_reason)
		SELECT $1, $2, p.id, 'access', 'grant', 'seed:' || p.id, 'grant', $6, 'seed', 'comp' FROM p
		RETURNING id, product_id, source_id
	)
	INSERT INTO `+s+`.product_access (merchant_id, customer_id, product_id, grant_id, source_type, source_id, starts_at)
	SELECT $1, $2, gr.product_id, gr.id, 'grant', gr.source_id, $6 FROM gr`,
		f.client.MerchantID().UUID(), customer.UUID(), n, prefix, keys, from)
	require.NoError(f.t, err)
	require.NoError(f.t, tx.Commit(ctx))
	_, err = f.pool.Exec(ctx, "ANALYZE "+s+".product_access, "+s+".product_entitlements, "+s+".customer_entitlement_cache")
	require.NoError(f.t, err)
}

// crowd gives n more customers a few of the merchant's products each, so the
// planner sees a merchant's real spread of customers.
func (f *fixture) crowd(n int) {
	f.t.Helper()
	ctx := f.t.Context()
	s := pgx.Identifier{f.schema}.Sanitize()
	_, err := f.pool.Exec(ctx, `WITH products AS (
		SELECT array_agg(id ORDER BY id) AS ids FROM `+s+`.products WHERE merchant_id = $1
	), c AS (
		INSERT INTO `+s+`.customers (merchant_id, id) SELECT $1, gen_random_uuid() FROM generate_series(1, $2::int) RETURNING id
	), numbered AS (SELECT id, row_number() OVER () AS n FROM c
	), picks AS (
		SELECT numbered.id AS customer_id, products.ids[1 + ((numbered.n * 7 + k * 131) % cardinality(products.ids))] AS product_id
		FROM numbered, products, generate_series(0, 2) k
	), gr AS (
		INSERT INTO `+s+`.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, event, starts_at, actor, grant_reason)
		SELECT $1, customer_id, product_id, 'access', 'grant', 'crowd:' || customer_id || ':' || product_id, 'grant', $3, 'seed', 'comp' FROM picks
		RETURNING id, customer_id, product_id, source_id
	)
	INSERT INTO `+s+`.product_access (merchant_id, customer_id, product_id, grant_id, source_type, source_id, starts_at)
	SELECT $1, customer_id, product_id, id, 'grant', source_id, $3 FROM gr`, f.client.MerchantID().UUID(), n, f.clock.Now().Add(-time.Hour))
	require.NoError(f.t, err)
	_, err = f.pool.Exec(ctx, "ANALYZE "+s+".product_access, "+s+".customers")
	require.NoError(f.t, err)
}

// executedPlan re-runs the last execution of a statement, with its own
// arguments, under EXPLAIN (ANALYZE, BUFFERS): the plan the engine runs (it
// plans every entitlement read for its parameters) and the shared blocks it
// touched, a measure of its work that does not depend on the machine.
func (f *fixture) executedPlan(name string) (string, int) {
	f.t.Helper()
	f.executed.mu.Lock()
	sql, args := f.executed.sql[name], f.executed.args[name]
	f.executed.mu.Unlock()
	require.NotEmpty(f.t, sql, "%s never ran", name)
	var raw []byte
	require.NoError(f.t, f.pool.QueryRow(f.t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw))
	var plan []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &plan))
	hit, _ := plan[0].Plan["Shared Hit Blocks"].(float64)
	read, _ := plan[0].Plan["Shared Read Blocks"].(float64)
	return string(raw), int(hit + read)
}

// plannedFor is the plan of the last execution of a write, for its own
// arguments, without running it again.
func (f *fixture) plannedFor(name string) string {
	f.t.Helper()
	f.executed.mu.Lock()
	sql, args := f.executed.sql[name], f.executed.args[name]
	f.executed.mu.Unlock()
	require.NotEmpty(f.t, sql, "%s never ran", name)
	var raw []byte
	require.NoError(f.t, f.pool.QueryRow(f.t.Context(), "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw))
	return string(raw)
}

// scannedWhole is the tables a JSON plan reads with a sequential scan.
func scannedWhole(t *testing.T, raw string) []string {
	t.Helper()
	var plan []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &plan))
	var out []string
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Node Type"] == "Seq Scan" {
			out = append(out, fmt.Sprint(node["Relation Name"]))
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if c, ok := child.(map[string]any); ok {
				walk(c)
			}
		}
	}
	walk(plan[0].Plan)
	return out
}

// rescannedCTEs is the CTEs a JSON plan scans once per outer row of a nested
// loop: quadratic in the rows of both sides.
func rescannedCTEs(t *testing.T, raw string) []string {
	t.Helper()
	var plan []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &plan))
	var out []string
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			c, ok := child.(map[string]any)
			if !ok {
				continue
			}
			if node["Node Type"] == "Nested Loop" && c["Parent Relationship"] == "Inner" {
				inner := c
				for inner["Node Type"] == "Materialize" || inner["Node Type"] == "Subquery Scan" {
					next, _ := inner["Plans"].([]any)
					if len(next) == 0 {
						break
					}
					inner, _ = next[0].(map[string]any)
				}
				if inner["Node Type"] == "CTE Scan" {
					out = append(out, fmt.Sprint(inner["CTE Name"]))
				}
			}
			walk(c)
		}
	}
	walk(plan[0].Plan)
	return out
}

func (f *fixture) buffers(name string) int {
	f.t.Helper()
	_, used := f.executedPlan(name)
	return used
}

// cached waits for the background rebuild a live read of a heavy buyer
// starts, and answers whether their cache is valid now.
func (f *fixture) cached(customer billing.CustomerID) bool {
	f.t.Helper()
	s := pgx.Identifier{f.schema}.Sanitize()
	valid := false
	require.Eventually(f.t, func() bool {
		err := f.pool.QueryRow(f.t.Context(), `SELECT EXISTS (SELECT 1 FROM `+s+`.customer_entitlement_cache_stamps st
			JOIN `+s+`.merchants m ON m.id = st.merchant_id JOIN `+s+`.customers c ON c.merchant_id = st.merchant_id AND c.id = st.customer_id
			WHERE st.customer_id = $1 AND st.entitlement_generation = m.entitlement_generation AND st.access_version = c.access_version)`, customer.UUID()).Scan(&valid)
		return err == nil && valid
	}, 30*time.Second, 20*time.Millisecond)
	return valid
}

// under is every key the customer holds under prefix at at (zero: now), in
// byte order, read page by page.
func (f *fixture) under(customer billing.CustomerID, prefix string, at time.Time) []string {
	f.t.Helper()
	var out []string
	for cursor := ""; ; {
		page, err := f.client.ListEntitlements(f.t.Context(), billing.EntitlementListParams{
			CustomerIDs: []billing.CustomerID{customer}, Prefix: prefix, At: at, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit, Cursor: cursor},
		})
		require.NoError(f.t, err)
		for _, row := range page.Items {
			out = append(out, row.Entitlement)
		}
		if page.Next == "" {
			return out
		}
		cursor = page.Next
	}
}

// held answers which of keys the customer holds at at (zero: now).
func (f *fixture) held(customer billing.CustomerID, at time.Time, keys ...string) map[string]bool {
	f.t.Helper()
	got, err := heldKeys(f.t.Context(), f.client, customer, at, keys...)
	require.NoError(f.t, err)
	return got
}

// A heavy buyer's keys come from their cache, and the cache never answers
// stale: a key edit, a grant, a revocation and a window's end each show at
// once; a past instant reads history live.
func TestHeavyBuyerCacheNeverAnswersStale(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	whale, small := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	start := f.clock.Now()
	f.seed(whale, "w:", entitlements.HeavyBuyerProducts, 1, start.Add(-time.Hour))
	f.seed(small, "s:", 3, 1, start.Add(-time.Hour))
	held := func() []string { return f.under(whale, "w:", time.Time{}) }

	f.executed.ran()
	require.Len(t, held(), entitlements.HeavyBuyerProducts)
	ran := f.executed.ran()
	require.Equal(t, 1, ran["ListDerivedEntitlementsPage"], "a cold read derives live: %v", ran)
	require.True(t, f.cached(whale), "and caches the heavy buyer in the background")
	f.executed.ran()
	require.Len(t, held(), entitlements.HeavyBuyerProducts)
	ran = f.executed.ran()
	require.Equal(t, 1, ran["ListCachedEntitlementsPage"], "a warm read is one range of the cache: %v", ran)
	require.Zero(t, ran["ListDerivedEntitlementsPage"])
	f.under(small, "s:", time.Time{})
	require.Zero(t, f.executed.ran()["SyncEntitlementCache"], "a light buyer is never cached")

	// A key edit of a product they hold.
	extra, err := f.client.CreateProduct(ctx, billing.CreateProductParams{Key: "extra", DisplayName: "Extra", Entitlements: []string{"w:zz:1"}})
	require.NoError(t, err)
	_, err = f.client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: whale, ProductID: extra.ID}}})
	require.NoError(t, err)
	require.Contains(t, held(), "w:zz:1", "a grant shows at once")
	before := f.clock.Now()
	f.clock.Advance(time.Minute)
	_, err = f.client.UpdateProduct(ctx, extra.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"w:zz:2"})})
	require.NoError(t, err)
	keys := held()
	require.Contains(t, keys, "w:zz:2", "a key added to a held product shows at once")
	require.NotContains(t, keys, "w:zz:1", "a key removed from it goes at once")
	require.Equal(t, map[string]bool{"w:zz:1": true, "w:zz:2": false}, f.held(whale, before, "w:zz:1", "w:zz:2"), "a past instant reads the product as it was")

	// A revocation, then a window that ends with no write at all.
	access, err := f.client.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{whale}, PageRequest: billing.PageRequest{Limit: 1}})
	require.NoError(t, err)
	require.NoError(t, revokeAccess(f.client, ctx, access.Items[0].ID))
	require.NotContains(t, held(), "w:zz:2", "a revoked window's keys go at once")
	rental, err := f.client.CreateProduct(ctx, billing.CreateProductParams{Key: "rental", DisplayName: "Rental", Entitlements: []string{"w:rent:1"}})
	require.NoError(t, err)
	ends := f.clock.Now().Add(time.Hour)
	_, err = f.client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: whale, ProductID: rental.ID, EndsAt: &ends}}})
	require.NoError(t, err)
	require.Contains(t, held(), "w:rent:1")
	require.True(t, f.cached(whale))
	f.executed.ran()
	require.Contains(t, held(), "w:rent:1")
	ran = f.executed.ran()
	require.NotZero(t, ran["ListCachedEntitlementsPage"], "the rental is cached: %v", ran)
	require.Zero(t, ran["ListDerivedEntitlementsPage"])
	f.clock.Advance(2 * time.Hour)
	require.NotContains(t, held(), "w:rent:1", "the cache ends with the next window boundary")
}

// Racing edits and grants never produce a stale or mixed answer: a read
// after a write returns sees it, and a read during writes sees one product
// definition, never a mix.
func TestHeavyBuyerCacheRaces(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	whale := billing.CustomerID(uuid.New())
	f.seed(whale, "w:", entitlements.HeavyBuyerProducts, 1, f.clock.Now().Add(-time.Hour))
	product, err := f.client.CreateProduct(ctx, billing.CreateProductParams{Key: "race", DisplayName: "Race", Entitlements: []string{"race:0"}})
	require.NoError(t, err)
	_, err = f.client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: whale, ProductID: product.ID}}})
	require.NoError(t, err)
	const versions = 25
	all := make([]string, versions+1)
	for i := range all {
		all[i] = fmt.Sprintf("race:%d", i)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var mixed sync.Map
	for range 3 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				exact, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{whale}, Entitlements: all})
				if err != nil {
					mixed.Store(err.Error(), true)
					continue
				}
				ranged, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{whale}, Prefix: "race:"})
				if err != nil {
					mixed.Store(err.Error(), true)
					continue
				}
				if len(exact.Items) != 1 || len(ranged.Items) != 1 {
					mixed.Store(fmt.Sprintf("%v %v", exact.Items, ranged.Items), true)
				}
			}
		}()
	}
	for i := 1; i <= versions; i++ {
		_, err := f.client.UpdateProduct(ctx, product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{all[i]})})
		require.NoError(t, err)
		require.Equal(t, map[string]bool{all[i-1]: false, all[i]: true}, f.held(whale, time.Time{}, all[i-1], all[i]), "a read after an edit returns sees it")
		if i%5 == 0 {
			key := fmt.Sprintf("grant:%d", i)
			granted, err := f.client.CreateProduct(ctx, billing.CreateProductParams{Key: fmt.Sprintf("race-grant-%d", i), DisplayName: "Grant", Entitlements: []string{key}})
			require.NoError(t, err)
			windows, err := f.client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: whale, ProductID: granted.ID}}})
			require.NoError(t, err)
			require.True(t, f.held(whale, time.Time{}, key)[key], "a grant shows at once")
			require.NoError(t, revokeAccess(f.client, ctx, windows[0].ID))
			require.False(t, f.held(whale, time.Time{}, key)[key], "a refund shows at once")
		}
	}
	close(stop)
	readers.Wait()
	var problems []string
	mixed.Range(func(k, _ any) bool { problems = append(problems, k.(string)); return true })
	require.Empty(t, problems, "no reader saw a mixed or failed answer")
}

// A whale's reads stay bounded by their work, not their holdings: live reads
// touch a few blocks per held product, cached reads a few blocks in all.
func TestHeavyBuyerReadsStayBounded(t *testing.T) {
	f := newFixture(t)
	const owned = 10_000
	whale := billing.CustomerID(uuid.New())
	f.seed(whale, "w:", owned, 1, f.clock.Now().Add(-time.Hour))
	f.crowd(40_000)
	keys := make([]string, billing.MaxBatchItems)
	for i := range keys {
		keys[i] = fmt.Sprintf("w:%06d:1", i*97+1)
	}
	read := func() (map[string]bool, *billing.ListPage[billing.CustomerEntitlement]) {
		t.Helper()
		exact := f.held(whale, time.Time{}, keys...)
		page, err := f.client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{whale}, Prefix: "w:00", PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}})
		require.NoError(t, err)
		return exact, page
	}

	exact, page := read()
	for _, key := range keys {
		require.True(t, exact[key], key)
	}
	require.Len(t, page.Items, billing.MaxPageLimit)
	require.NotEmpty(t, page.Next, "more are held than one page")
	plan, live := f.executedPlan("ListDerivedEntitlementsPage")
	require.LessOrEqual(t, live, 4*owned, "a live prefix read touches at most 4 blocks per held product")
	require.NotContains(t, plan, "Seq Scan", plan)
	require.NotContains(t, plan, "product_entitlements_entitlement_idx", "the read probes each held product, never the catalog keyspace: %s", plan)
	require.Regexp(t, `product_access_customer_(created_)?idx`, plan, "the held products come from the customer's own index")
	require.Contains(t, plan, "product_entitlements_product_idx", plan)

	require.True(t, f.cached(whale))
	rebuild := f.plannedFor("SyncEntitlementCache")
	require.NotContains(t, scannedWhole(t, rebuild), "product_access", "a rebuild reads the customer's own windows: %s", rebuild)
	require.NotContains(t, scannedWhole(t, rebuild), "product_entitlements", "a rebuild probes each held product: %s", rebuild)
	require.Contains(t, rebuild, "product_entitlements_product_idx", rebuild)
	require.Empty(t, rescannedCTEs(t, rebuild), "a rebuild compares held and cached keys in one pass: %s", rebuild)
	_, err := f.pool.Exec(t.Context(), "ANALYZE "+pgx.Identifier{f.schema}.Sanitize()+".customer_entitlement_cache")
	require.NoError(t, err)
	warmExact, warmPage := read()
	require.Equal(t, exact, warmExact, "the cache answers exactly what the live read did")
	require.Equal(t, page, warmPage)
	// A cached read costs a few blocks per key it answers, whatever the
	// customer holds: one index descent per key, one block per row.
	for name, bound := range map[string]int{"ListValidEntitlementCaches": 12, "CheckCachedEntitlements": 4 * billing.MaxBatchItems, "ListCachedEntitlementsPage": 2*(billing.MaxPageLimit+1) + 60} {
		used := f.buffers(name)
		require.LessOrEqual(t, used, bound, "%s touched %d blocks", name, used)
	}
	first, err := f.client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{whale}, Prefix: "w:", PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(t, err)
	require.Len(t, first.Items, 100)
	require.NotEmpty(t, first.Next)
	require.LessOrEqual(t, f.buffers("ListCachedEntitlementsPage"), 2*101+20, "a cached page is one short range")
	next, err := f.client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{whale}, Prefix: "w:", PageRequest: billing.PageRequest{Limit: 100, Cursor: first.Next}})
	require.NoError(t, err)
	require.Greater(t, next.Items[0].Entitlement, first.Items[99].Entitlement, "keyset pages follow on")
	require.True(t, strings.HasPrefix(next.Items[0].Entitlement, "w:"))
}

// A page of customers reads in one query per path: the host minting tokens
// for many users reads all of their keys at once, cached and live alike.
func TestEntitlementsOfManyCustomersAreOneRead(t *testing.T) {
	f := newFixture(t)
	whale := billing.CustomerID(uuid.New())
	f.seed(whale, "w:", entitlements.HeavyBuyerProducts, 1, f.clock.Now().Add(-time.Hour))
	f.under(whale, "w:", time.Time{})
	require.True(t, f.cached(whale))
	customers := []billing.CustomerID{whale}
	for i := range 40 {
		c := billing.CustomerID(uuid.New())
		f.seed(c, fmt.Sprintf("c%02d:", i), 2, 1, f.clock.Now().Add(-time.Hour))
		customers = append(customers, c)
	}
	f.executed.ran()
	var rows []billing.CustomerEntitlement
	pages := 0
	for cursor := ""; ; {
		page, err := f.client.ListEntitlements(f.t.Context(), billing.EntitlementListParams{CustomerIDs: customers, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit, Cursor: cursor}})
		require.NoError(t, err)
		pages++
		rows = append(rows, page.Items...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	ran := f.executed.ran()
	require.Len(t, rows, entitlements.HeavyBuyerProducts+40*2, "every key of every customer")
	require.Equal(t, pages, ran["ListValidEntitlementCaches"], "one cache check a page: %v", ran)
	require.LessOrEqual(t, ran["ListDerivedEntitlementsPage"], pages, "the light customers are one derived read a page: %v", ran)
	require.LessOrEqual(t, ran["ListCachedEntitlementsPage"], pages, "the whale is one cached read a page: %v", ran)
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		require.True(t, a.CustomerID.String() < b.CustomerID.String() || a.CustomerID == b.CustomerID && a.Entitlement < b.Entitlement, "by customer, then key")
	}
}
