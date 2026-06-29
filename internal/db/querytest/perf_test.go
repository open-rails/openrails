//go:build integration

package querytest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestQueryPerfEntitlementExistsActiveUsesIndex(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	scale := queryPerfScale(t)
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Now().UTC().Truncate(time.Second)
	targetCustomerID := perfUUID("10000000", scale/2)

	seedPerfEntitlements(ctx, t, pool, merchantID, scale, now)

	q := gen.New(pool)
	ok, err := q.EntitlementExistsActive(ctx, gen.EntitlementExistsActiveParams{
		MerchantID:  merchantID,
		CustomerID:  targetCustomerID,
		Entitlement: "perf-feature",
		At:          now,
	})
	require.NoError(t, err)
	require.True(t, ok)

	plan := explainJSON(ctx, t, pool, `
		SELECT EXISTS (
			SELECT 1 FROM openrails.entitlements ent
			WHERE ent.merchant_id = $1
			  AND ent.customer_id = $2
			  AND ent.entitlement = $3
			  AND ent.start_at <= $4::timestamptz
			  AND (ent.end_at IS NULL OR ent.end_at > $4::timestamptz)
			  AND ent.revoked_at IS NULL
			  AND ent.deleted_at IS NULL
		)
	`, merchantID, targetCustomerID, "perf-feature", now)

	nodes := planNodes(plan[0]["Plan"].(map[string]any))
	require.NotContains(t, nodes, "Seq Scan", "hot entitlement lookup must not scan the seeded table")
	require.True(t, containsNode(nodes, "Index Scan") || containsNode(nodes, "Index Only Scan") || containsNode(nodes, "Bitmap Index Scan"), "expected indexed plan, got %v", nodes)

	if reportPath := strings.TrimSpace(os.Getenv("QUERY_PERF_REPORT")); reportPath != "" {
		require.NoError(t, os.WriteFile(reportPath, mustJSON(t, map[string]any{
			"query":          "EntitlementExistsActive",
			"scale":          scale,
			"plan_nodes":     nodes,
			"execution_time": plan[0]["Execution Time"],
		}), 0o644))
	}
}

func seedPerfEntitlements(ctx context.Context, t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID, scale int, now time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, "TRUNCATE openrails.entitlements, openrails.customers RESTART IDENTITY CASCADE")
	require.NoError(t, err)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	_, err = pool.CopyFrom(ctx, pgx.Identifier{"openrails", "customers"}, []string{"id", "merchant_id", "subject"}, pgx.CopyFromSlice(scale, func(i int) ([]any, error) {
		id := perfUUID("10000000", i)
		return []any{id, merchantID, id.String()}, nil
	}))
	require.NoError(t, err)

	_, err = pool.CopyFrom(ctx, pgx.Identifier{"openrails", "entitlements"}, []string{
		"id", "merchant_id", "customer_id", "entitlement", "start_at", "source_id", "source_type", "created_at", "updated_at",
	}, pgx.CopyFromSlice(scale, func(i int) ([]any, error) {
		return []any{
			perfUUID("20000000", i),
			merchantID,
			perfUUID("10000000", i),
			"perf-feature",
			now.Add(-time.Hour),
			perfUUID("30000000", i),
			"admin",
			now,
			now,
		}, nil
	}))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ANALYZE openrails.customers; ANALYZE openrails.entitlements")
	require.NoError(t, err)
}

func explainJSON(ctx context.Context, t *testing.T, pool gen.DBTX, sql string, args ...any) []map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw))
	var out []map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func planNodes(plan map[string]any) []string {
	nodes := []string{plan["Node Type"].(string)}
	for _, key := range []string{"Plans", "InitPlan"} {
		children, _ := plan[key].([]any)
		for _, child := range children {
			nodes = append(nodes, planNodes(child.(map[string]any))...)
		}
	}
	return nodes
}

func containsNode(nodes []string, want string) bool {
	for _, node := range nodes {
		if node == want {
			return true
		}
	}
	return false
}

func queryPerfScale(t *testing.T) int {
	t.Helper()
	if raw := strings.TrimSpace(os.Getenv("QUERY_PERF_SCALE")); raw != "" {
		n, err := strconv.Atoi(raw)
		require.NoError(t, err, "QUERY_PERF_SCALE")
		require.Positive(t, n, "QUERY_PERF_SCALE")
		return n
	}
	return 100000
}

func perfUUID(prefix string, i int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, i))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	return append(b, '\n')
}
