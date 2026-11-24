//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// executedStatements keeps the SQL and arguments of each sqlc statement's
// last execution, by its "-- name:".
type executedStatements struct {
	mu   sync.Mutex
	sql  map[string]string
	args map[string][]any
}

func (s *executedStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if _, rest, ok := strings.Cut(data.SQL, "-- name: "); ok {
		name, _, _ := strings.Cut(rest, " ")
		s.mu.Lock()
		s.sql[name], s.args[name] = data.SQL, data.Args
		s.mu.Unlock()
	}
	return ctx
}

func (*executedStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// explain runs the last execution of a statement again, with its own
// arguments and the engine's custom plans, under EXPLAIN (ANALYZE, BUFFERS):
// the plan it runs and the shared blocks it touches.
func (s *executedStatements) explain(t *testing.T, pool *pgxpool.Pool, name string) (map[string]any, int) {
	t.Helper()
	s.mu.Lock()
	sql, args := s.sql[name], s.args[name]
	s.mu.Unlock()
	require.NotEmpty(t, sql, "%s never ran", name)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = tx.Exec(t.Context(), "SET LOCAL plan_cache_mode = force_custom_plan")
	require.NoError(t, err)
	var raw []byte
	require.NoError(t, tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	require.Len(t, plans, 1)
	hit, _ := plans[0].Plan["Shared Hit Blocks"].(float64)
	read, _ := plans[0].Plan["Shared Read Blocks"].(float64)
	return plans[0].Plan, int(hit + read)
}

// planScans is a plan's table scans in the order they start: init plans, then
// each join's outer side before its inner.
func planScans(node map[string]any) []map[string]any {
	var out []map[string]any
	if _, ok := node["Relation Name"]; ok {
		out = append(out, node)
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		if c, ok := child.(map[string]any); ok {
			out = append(out, planScans(c)...)
		}
	}
	return out
}

// scanIndex is the index a table scan reads; a bitmap scan's is its child's.
func scanIndex(scan map[string]any) string {
	if name, ok := scan["Index Name"].(string); ok {
		return name
	}
	children, _ := scan["Plans"].([]any)
	for _, child := range children {
		if c, ok := child.(map[string]any); ok {
			if name := scanIndex(c); name != "" {
				return name
			}
		}
	}
	return ""
}

func requirePlanShape(t *testing.T, plan map[string]any, table, index string) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	scans := planScans(plan)
	require.NotEmpty(t, scans, "%s", raw)
	for _, scan := range scans {
		require.NotEqual(t, "Seq Scan", scan["Node Type"], "no read scans a table: %s", raw)
	}
	require.Equal(t, table, scans[0]["Relation Name"], "%s", raw)
	require.Equal(t, index, scanIndex(scans[0]), "%s", raw)
	return scans
}

func TestEntitlementReadPlans(t *testing.T) {
	t.Run("OR-J27-S16/exact-checks-and-prefix-pages-stay-on-their-indexes", func(t *testing.T) {
		const catalog, held, crowd = 40_000, 10_000, 10_000
		executed := &executedStatements{sql: map[string]string{}, args: map[string][]any{}}
		w := newWorld(t, func(w *world) {
			pc := w.pool.Config().Copy()
			pc.ConnConfig.Tracer = executed
			traced, err := pgxpool.NewWithConfig(t.Context(), pc)
			require.NoError(t, err)
			t.Cleanup(traced.Close)
			w.deps.Postgres = traced
		})
		ctx := t.Context()
		mid := w.alpha.MerchantID().UUID()
		whale := uuid.New()
		from := w.clock.Now().Add(-time.Hour).Truncate(time.Microsecond)
		table := func(name string) string { return pgx.Identifier{w.schema, name}.Sanitize() }

		// A catalog of 40,000 products, one key each. The whale holds every
		// fourth; 10,000 other customers hold three each. Windows are written
		// in a scattered order, as years of purchases leave them.
		tx, err := w.pool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, "SELECT set_config('openrails.catalog_batch_merchant_id', $1, true)", mid.String())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `WITH p AS (
			INSERT INTO `+table("products")+` (merchant_id, key, display_name, tier_rank, archived)
			SELECT $1, 'plan-' || lpad(n::text, 6, '0'), 'Plan', 0, false FROM generate_series(0, $2::int - 1) n
			RETURNING id, key)
			INSERT INTO `+table("product_entitlements")+` (merchant_id, product_id, entitlement, added_at, added_by)
			SELECT $1, p.id, 'plan:' || substr(p.key, 6), $3, 'critical' FROM p`, mid, catalog, from)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO `+table("customers")+` (merchant_id, id)
			SELECT $1::uuid, $2::uuid UNION ALL SELECT $1::uuid, gen_random_uuid() FROM generate_series(1, $3::int)`, mid, whale, crowd)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `WITH others AS (
			SELECT id, row_number() OVER (ORDER BY id) AS n FROM `+table("customers")+` WHERE merchant_id = $1 AND id <> $2
		), holdings AS (
			SELECT $2::uuid AS customer_id, p.id AS product_id FROM `+table("products")+` p
			WHERE p.merchant_id = $1 AND substr(p.key, 6)::int % ($4::int / $5::int) = 0
			UNION ALL
			SELECT o.id, p.id FROM others o CROSS JOIN generate_series(0, 2) k
			JOIN `+table("products")+` p ON p.merchant_id = $1 AND p.key = 'plan-' || lpad(((o.n * 7 + k * 13331) % $4::int)::text, 6, '0')
		), gr AS (
			INSERT INTO `+table("grants")+` (merchant_id, customer_id, product_id, kind, source_type, source_id, event, starts_at, actor, grant_reason)
			SELECT $1, customer_id, product_id, 'access', 'grant', 'critical:' || customer_id || ':' || product_id, 'grant', $3, 'critical', 'comp'
			FROM holdings ORDER BY md5(customer_id::text || product_id::text)
			RETURNING id, customer_id, product_id, source_id)
			INSERT INTO `+table("product_access")+` (merchant_id, customer_id, product_id, grant_id, source_type, source_id, starts_at)
			SELECT $1, customer_id, product_id, id, 'grant', source_id, $3 FROM gr`, mid, whale, from, catalog, held)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		// Steady state, as autovacuum keeps it: statistics current and every
		// page all-visible, so index-only scans skip the heap.
		for attempt := 1; ; attempt++ {
			_, err := w.pool.Exec(ctx, "VACUUM (ANALYZE) "+table("products")+", "+table("product_entitlements")+", "+table("customers")+", "+table("grants")+", "+table("product_access"))
			require.NoError(t, err)
			var pending int
			require.NoError(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE oid IN ($1::text::regclass, $2::text::regclass) AND relallvisible < relpages`,
				table("product_entitlements"), table("product_access")).Scan(&pending))
			if pending == 0 {
				break
			}
			require.Less(t, attempt, 100, "the seeded tables never became all-visible")
			time.Sleep(100 * time.Millisecond)
		}

		keys := make([]string, 0, billing.MaxEntitlementChecks)
		for i := range billing.MaxEntitlementChecks - 1 {
			keys = append(keys, fmt.Sprintf("plan:%06d", i*400))
		}
		keys = append(keys, "plan:000001") // another customer's product
		check, err := w.alpha.CheckEntitlements(ctx, billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(whale), Entitlements: keys, Prefixes: []string{"plan:"}})
		require.NoError(t, err)
		for _, key := range keys[:len(keys)-1] {
			require.True(t, check.Entitlements[key], key)
		}
		require.False(t, check.Entitlements["plan:000001"])
		page := check.Held["plan:"]
		require.Len(t, page.Keys, billing.DefaultHeldEntitlements)
		require.True(t, page.Truncated)
		require.Equal(t, "plan:000000", page.Keys[0])
		require.Equal(t, fmt.Sprintf("plan:%06d", 4*(billing.DefaultHeldEntitlements-1)), page.Keys[len(page.Keys)-1])
		valid, err := w.alpha.CheckEntitlements(ctx, billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(whale), Entitlements: keys, IncludeValidity: true})
		require.NoError(t, err)
		require.Equal(t, check.Entitlements, valid.Entitlements)
		require.NotNil(t, valid.EvaluatedAt)
		require.Nil(t, valid.ValidUntil, "no held window ends")

		// An exact check probes the products granting each key, then the
		// customer's windows of each: a few blocks a key, whatever they hold.
		for _, name := range []string{"CheckDerivedEntitlements", "CheckDerivedEntitlementsWithBoundary"} {
			plan, blocks := executed.explain(t, w.pool, name)
			requirePlanShape(t, plan, "product_entitlements", "product_entitlements_entitlement_idx")
			require.LessOrEqual(t, blocks, 16*len(keys), "%s touched %d blocks", name, blocks)
		}
		// A prefix page walks the customer's live windows once, seats
		// included, then each held product's keys; never the catalog's keys.
		plan, blocks := executed.explain(t, w.pool, "ListDerivedEntitlementsPage")
		scans := requirePlanShape(t, plan, "product_access", "product_access_customer_idx")
		probes := map[string]bool{}
		for _, scan := range scans[1:] {
			probes[scanIndex(scan)] = true
		}
		require.Equal(t, map[string]bool{"product_entitlements_product_idx": true}, probes)
		require.LessOrEqual(t, blocks, 4*held, "a prefix page touched %d blocks for %d held products", blocks, held)
	})
}
