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

// Non-uniform seed shape: one "fat" customer (and one fat subscription) carry
// many child rows so per-entity fan-out — sorts, group-bys, skip scans — actually
// shows up instead of hiding behind one-row-per-customer data.
const (
	perfEntitlement   = "perf-feature"
	perfCurrency      = "USD"
	perfFatProducts   = 50  // distinct products the fat customer holds an active sub in (active-sub fan-out)
	perfFatEnt        = 200 // distinct active entitlements on the fat customer
	perfFatGrants     = 200 // live credit lots on the fat customer
	perfFatUsage      = 500 // usage events on the fat customer
	perfFatPayments   = 200 // completed charges on the fat subscription
	perfFatPayMethods = 50  // stored instruments on the fat customer
)

// perfSeed carries the ids the cases bind as query args.
type perfSeed struct {
	HotCustomerID uuid.UUID // a normal (non-fat) customer near the top of the range
	HotSubjects   []string  // a few customer subjects, for the by-subject batch lookup
	FatCustomerID uuid.UUID // the whale: many entitlements / subs / grants / usage / instruments
	FatSubID      uuid.UUID // the fat subscription: many completed charges
}

// TestQueryPerformance is the scaling gate: it bulk-seeds the growable billing
// tables, ANALYZEs, then EXPLAIN (ANALYZE, BUFFERS)es the REAL generated query
// text (sourced from gen.QueryText, never hand-copied) for each distinct hot
// access pattern, asserting no sequential scan on the big table / no Sort where
// the access path must be index-ordered, plus loose time and buffer budgets.
// Every case runs inside a rolled-back transaction so the seed is never mutated.
//
// Only queries whose efficiency can degrade at scale are gated; PK / unique point
// lookups are O(1) and need no gate.
func TestQueryPerformance(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	scale := queryPerfScale(t)
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Now().UTC().Truncate(time.Second)
	seed := seedPerfData(ctx, t, pool, merchantID, scale, now)

	cases := []perfCase{
		{
			// Hot access check: EXISTS over the partial customer-active-window index.
			Name: "entitlement_exists_active", MaxExecutionMS: 75, MaxSharedReadBlocks: 64,
			SQL:           gen.QueryText["EntitlementExistsActive"],
			Args:          []any{merchantID, seed.HotCustomerID, perfEntitlement, now},
			ForbidSeqScan: []string{"entitlements"},
		},
		{
			// DISTINCT entitlement names for the fat customer (real name fan-out).
			Name: "entitlement_names_active", MaxExecutionMS: 100, MaxSharedReadBlocks: 256,
			SQL:           gen.QueryText["ListActiveEntitlementNamesMerchant"],
			Args:          []any{merchantID, seed.FatCustomerID, now},
			ForbidSeqScan: []string{"entitlements"},
		},
		{
			// Fat customer with many active subs + ORDER BY created_at DESC LIMIT 1.
			// Index-ordered via idx_subscriptions_customer_active_created (migration 042).
			Name: "active_subscription_by_customer", MaxExecutionMS: 75, MaxSharedReadBlocks: 64,
			SQL:           gen.QueryText["GetActiveSubscriptionByCustomerAt"],
			Args:          []any{seed.FatCustomerID, now},
			ForbidSeqScan: []string{"subscriptions"}, ForbidSort: true,
		},
		{
			// Resolve host subjects -> customer ids via uq_customers_merchant_subject.
			Name: "customers_by_subject", MaxExecutionMS: 75, MaxSharedReadBlocks: 64,
			SQL:           gen.QueryText["LookupCustomerIDsBySubjects"],
			Args:          []any{merchantID, seed.HotSubjects},
			ForbidSeqScan: []string{"customers"},
		},
		{
			// Stored instruments for the fat customer (ORDER BY created_at DESC, no LIMIT).
			Name: "payment_methods_by_customer", MaxExecutionMS: 75, MaxSharedReadBlocks: 64,
			SQL:           gen.QueryText["ListPaymentMethodsByCustomer"],
			Args:          []any{seed.FatCustomerID},
			ForbidSeqScan: []string{"payment_methods"},
		},
		{
			// Hot admission affordability snapshot (ledger_accounts + money_settings).
			Name: "admission_capacity", MaxExecutionMS: 75, MaxSharedReadBlocks: 64,
			SQL:           gen.QueryText["GetAdmissionCapacity"],
			Args:          []any{merchantID, seed.HotCustomerID, perfCurrency},
			ForbidSeqScan: []string{"ledger_accounts", "money_settings"},
		},
		{
			// All grant-events for the fat customer (idx_grants_customer).
			Name: "grants_by_customer", MaxExecutionMS: 100, MaxSharedReadBlocks: 256,
			SQL:           gen.QueryText["ListGrantsByCustomer"],
			Args:          []any{merchantID, seed.FatCustomerID},
			ForbidSeqScan: []string{"grants"},
		},
		{
			// Live credit lots w/ a correlated NOT EXISTS supersede check on grants.
			Name: "spendable_credit_lots", MaxExecutionMS: 150, MaxSharedReadBlocks: 512,
			SQL:           gen.QueryText["ListSpendableCreditLots"],
			Args:          []any{merchantID, seed.FatCustomerID, perfCurrency, now},
			ForbidSeqScan: []string{"grants"},
		},
		{
			// Per-event_type usage rollup over the highest-volume table for the fat customer.
			Name: "usage_totals", MaxExecutionMS: 150, MaxSharedReadBlocks: 512,
			SQL:           gen.QueryText["AggregateUsageTotals"],
			Args:          []any{merchantID, seed.FatCustomerID, perfCurrency, now.Add(-365 * 24 * time.Hour), now.Add(time.Hour)},
			ForbidSeqScan: []string{"usage_events"},
		},
		{
			// Latest completed charge for the fat subscription (ORDER BY purchased_at
			// DESC LIMIT 1). Index-backed via idx_payments_subscription_id; the planner
			// adds a cheap top-N Sort over the subscription's bounded charge set (no
			// ForbidSort — see migration 042's NOTE: an ordered index was prototyped and
			// the planner correctly declined it).
			Name: "latest_charge_by_subscription", MaxExecutionMS: 75, MaxSharedReadBlocks: 64,
			SQL:           gen.QueryText["GetLatestChargeBySubscriptionID"],
			Args:          []any{seed.FatSubID},
			ForbidSeqScan: []string{"payments"},
		},
	}

	results := make([]perfResult, 0, len(cases))
	for _, c := range cases {
		if strings.TrimSpace(c.SQL) == "" {
			t.Fatalf("%s: empty SQL (missing gen.QueryText entry?)", c.Name)
		}
		r := explainCase(ctx, t, pool, scale, c)
		results = append(results, r)
		t.Logf("%-32s exec=%.3fms read=%d hit=%d nodes=%v seqscans=%v sorted=%v",
			c.Name, r.ExecutionMS, r.SharedReadBlocks, r.SharedHitBlocks, r.Nodes, keys(r.SeqScans), r.Sorted)

		if r.ExecutionMS > c.MaxExecutionMS {
			t.Errorf("%s execution %.3fms > %.3fms", c.Name, r.ExecutionMS, c.MaxExecutionMS)
		}
		if r.SharedReadBlocks > c.MaxSharedReadBlocks {
			t.Errorf("%s shared read blocks %d > %d", c.Name, r.SharedReadBlocks, c.MaxSharedReadBlocks)
		}
		for _, rel := range c.ForbidSeqScan {
			if r.SeqScans[rel] {
				t.Errorf("%s used a sequential scan on %s (nodes=%v)", c.Name, rel, r.Nodes)
			}
		}
		if c.ForbidSort && r.Sorted {
			t.Errorf("%s used a Sort node (expected index-ordered; nodes=%v)", c.Name, r.Nodes)
		}
	}

	if path := strings.TrimSpace(os.Getenv("QUERY_PERF_REPORT")); path != "" {
		require.NoError(t, os.WriteFile(path, mustJSON(t, results), 0o644))
	}
}

type perfCase struct {
	Name                string
	SQL                 string
	Args                []any
	ForbidSeqScan       []string
	ForbidSort          bool
	MaxExecutionMS      float64
	MaxSharedReadBlocks int64
}

type perfResult struct {
	Name             string          `json:"name"`
	Scale            int             `json:"scale"`
	ExecutionMS      float64         `json:"execution_ms"`
	PlanningMS       float64         `json:"planning_ms"`
	SharedReadBlocks int64           `json:"shared_read_blocks"`
	SharedHitBlocks  int64           `json:"shared_hit_blocks"`
	SeqScans         map[string]bool `json:"seq_scans"`
	Sorted           bool            `json:"sorted"`
	Nodes            []string        `json:"nodes"`
}

type explainNode struct {
	NodeType         string        `json:"Node Type"`
	RelationName     string        `json:"Relation Name"`
	SharedReadBlocks int64         `json:"Shared Read Blocks"`
	SharedHitBlocks  int64         `json:"Shared Hit Blocks"`
	Plans            []explainNode `json:"Plans"`
}

type explainRoot struct {
	Plan          explainNode `json:"Plan"`
	PlanningTime  float64     `json:"Planning Time"`
	ExecutionTime float64     `json:"Execution Time"`
}

// explainCase EXPLAIN (ANALYZE, BUFFERS)es the REAL query text inside a
// transaction it always rolls back, so the planned/timed query never mutates the
// shared seed (mirrors the authkit harness).
func explainCase(ctx context.Context, t *testing.T, pool *pgxpool.Pool, scale int, c perfCase) perfResult {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "begin %s", c.Name)
	defer func() { _ = tx.Rollback(ctx) }()

	var raw []byte
	require.NoError(t,
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+stripQueryHeader(c.SQL), c.Args...).Scan(&raw),
		"explain %s", c.Name)
	var roots []explainRoot
	require.NoError(t, json.Unmarshal(raw, &roots), "parse explain %s", c.Name)
	require.Len(t, roots, 1, "explain %s", c.Name)

	r := perfResult{
		Name: c.Name, Scale: scale, PlanningMS: roots[0].PlanningTime,
		ExecutionMS: roots[0].ExecutionTime, SeqScans: map[string]bool{},
	}
	collectPlan(roots[0].Plan, &r)
	return r
}

func collectPlan(p explainNode, r *perfResult) {
	r.SharedReadBlocks += p.SharedReadBlocks
	r.SharedHitBlocks += p.SharedHitBlocks
	r.Nodes = append(r.Nodes, p.NodeType)
	if p.NodeType == "Seq Scan" && p.RelationName != "" {
		r.SeqScans[p.RelationName] = true
	}
	if p.NodeType == "Sort" || p.NodeType == "Incremental Sort" {
		r.Sorted = true
	}
	for _, child := range p.Plans {
		collectPlan(child, r)
	}
}

// stripQueryHeader drops the leading sqlc `-- name: ... :kind` comment (and any
// blank lines) so EXPLAIN sees only the statement.
func stripQueryHeader(sql string) string {
	lines := strings.Split(sql, "\n")
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			i++
			continue
		}
		break
	}
	return strings.Join(lines[i:], "\n")
}

// seedPerfData wipes the customer-scoped surface, bulk-loads each growable table
// at `scale` (one child row per customer) plus a fat customer / fat subscription
// carrying many rows, ANALYZEs, and returns the ids the cases bind.
func seedPerfData(ctx context.Context, t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID, scale int, now time.Time) perfSeed {
	t.Helper()

	// Clean slate. CASCADE from customers+products wipes every customer-scoped
	// child (subscriptions, payments, grants, entitlements, ledger/usage, ...).
	_, err := pool.Exec(ctx, "TRUNCATE openrails.customers, openrails.products RESTART IDENTITY CASCADE")
	require.NoError(t, err)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	// One base product/price (all scale subs+payments use it) + perfFatProducts
	// distinct products for the fat customer's active-sub fan-out. tier_group NULL
	// keeps the per-customer active-tier unique constraint out of the way.
	q := gen.New(pool)
	productIDs := make([]uuid.UUID, perfFatProducts+1)
	priceIDs := make([]uuid.UUID, perfFatProducts+1)
	for k := range productIDs {
		pid, prid := uuid.New(), uuid.New()
		productIDs[k], priceIDs[k] = pid, prid
		_, err = q.CreateProduct(ctx, gen.CreateProductParams{
			ID: pid, MerchantID: merchantID, Key: fmt.Sprintf("perf-product-%d", k),
			DisplayName: "Perf", EntitlementsSpec: []byte(`[]`), CreditsSpec: []byte(`[]`),
			TierGroup: nil, TierRank: 0, CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)
		_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
			ID: prid, ProductID: pid, MerchantID: merchantID, Amount: 1999, Currency: perfCurrency,
			AutoRenew: false, Rails: []byte(`{}`), CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)
	}

	fatIdx := 0         // the whale is customer 0
	hotIdx := scale - 1 // a normal customer near the top of the range
	fatCustomerID := perfCustomerUUID(fatIdx)
	hotCustomerID := perfCustomerUUID(hotIdx)
	fatSubID := perfSubUUID(fatIdx) // customer 0's base subscription accrues the fat charges

	// customers: id, merchant_id, subject (subject = the row id string).
	copyRows(ctx, t, pool, "customers",
		[]string{"id", "merchant_id", "subject"}, scale, func(i int) []any {
			id := perfCustomerUUID(i)
			return []any{id, merchantID, id.String()}
		})

	// entitlements: one active "perf-feature" per customer.
	copyRows(ctx, t, pool, "entitlements",
		[]string{"id", "merchant_id", "customer_id", "entitlement", "start_at", "source_id", "source_type", "created_at", "updated_at"},
		scale, func(i int) []any {
			return []any{perfEntUUID(i), merchantID, perfCustomerUUID(i), perfEntitlement, now.Add(-time.Hour), uuid.New(), "admin", now, now}
		})
	// fat customer: many DISTINCT active entitlements (DISTINCT-names fan-out).
	copyRows(ctx, t, pool, "entitlements",
		[]string{"id", "merchant_id", "customer_id", "entitlement", "start_at", "source_id", "source_type", "created_at", "updated_at"},
		perfFatEnt, func(j int) []any {
			return []any{perfEntUUID(scale + j), merchantID, fatCustomerID, fmt.Sprintf("perf-feat-%d", j), now.Add(-time.Hour), uuid.New(), "admin", now, now}
		})

	// subscriptions: one active sub per customer on the base product (created_at spread).
	copyRows(ctx, t, pool, "subscriptions",
		[]string{"id", "product_id", "price_id", "status", "rail", "merchant_id", "customer_id", "current_period_ends_at", "started_at", "created_at", "updated_at"},
		scale, func(i int) []any {
			return []any{perfSubUUID(i), productIDs[0], priceIDs[0], "active", "ccbill", merchantID, perfCustomerUUID(i), now.Add(720 * time.Hour), now, now.Add(-time.Duration(i) * time.Second), now}
		})
	// fat customer: an active sub in each extra product (active-sub fan-out for the LIMIT 1 path).
	copyRows(ctx, t, pool, "subscriptions",
		[]string{"id", "product_id", "price_id", "status", "rail", "merchant_id", "customer_id", "current_period_ends_at", "started_at", "created_at", "updated_at"},
		perfFatProducts, func(j int) []any {
			return []any{perfSubUUID(scale + j), productIDs[j+1], priceIDs[j+1], "active", "ccbill", merchantID, fatCustomerID, now.Add(720 * time.Hour), now, now.Add(-time.Duration(j) * time.Minute), now}
		})

	// payments: one completed charge per subscription on the base price.
	copyRows(ctx, t, pool, "payments",
		[]string{"id", "price_id", "rail", "transaction_id", "amount", "list_amount", "currency", "status", "subscription_id", "merchant_id", "customer_id", "purchased_at", "created_at"},
		scale, func(i int) []any {
			return []any{perfPayUUID(i), priceIDs[0], "ccbill", fmt.Sprintf("txn-%012d", i), int64(1999), int64(1999), perfCurrency, "completed", perfSubUUID(i), merchantID, perfCustomerUUID(i), now.Add(-time.Hour), now}
		})
	// fat subscription: many completed charges (purchased_at spread) for the LIMIT 1 path.
	copyRows(ctx, t, pool, "payments",
		[]string{"id", "price_id", "rail", "transaction_id", "amount", "list_amount", "currency", "status", "subscription_id", "merchant_id", "customer_id", "purchased_at", "created_at"},
		perfFatPayments, func(j int) []any {
			return []any{perfPayUUID(scale + j), priceIDs[0], "ccbill", fmt.Sprintf("txn-fat-%012d", j), int64(1999), int64(1999), perfCurrency, "completed", fatSubID, merchantID, fatCustomerID, now.Add(-time.Duration(j) * time.Minute), now}
		})

	// grants: one live credit lot per customer.
	copyRows(ctx, t, pool, "grants",
		[]string{"id", "merchant_id", "customer_id", "kind", "source_type", "event", "amount", "currency", "starts_at", "ends_at", "created_at"},
		scale, func(i int) []any {
			return []any{perfGrantUUID(i), merchantID, perfCustomerUUID(i), "credit", "admin", "grant", int64(1000), perfCurrency, now.Add(-time.Hour), now.Add(720 * time.Hour), now}
		})
	// fat customer: many live credit lots (ends_at spread for the FIFO ORDER BY).
	copyRows(ctx, t, pool, "grants",
		[]string{"id", "merchant_id", "customer_id", "kind", "source_type", "event", "amount", "currency", "starts_at", "ends_at", "created_at"},
		perfFatGrants, func(j int) []any {
			return []any{perfGrantUUID(scale + j), merchantID, fatCustomerID, "credit", "admin", "grant", int64(1000), perfCurrency, now.Add(-time.Hour), now.Add(time.Duration(j+1) * time.Hour), now.Add(-time.Duration(j) * time.Second)}
		})

	// ledger_accounts: one customer_balance account per customer.
	copyRows(ctx, t, pool, "ledger_accounts",
		[]string{"id", "merchant_id", "customer_id", "account_type", "currency", "credits_posted", "debits_posted", "created_at"},
		scale, func(i int) []any {
			return []any{perfLedgerUUID(i), merchantID, perfCustomerUUID(i), "customer_balance", perfCurrency, int64(5000), int64(1250), now}
		})

	// money_settings: one row per customer.
	copyRows(ctx, t, pool, "money_settings",
		[]string{"merchant_id", "customer_id", "billing_mode", "currency", "created_at", "updated_at"},
		scale, func(i int) []any {
			return []any{merchantID, perfCustomerUUID(i), "prepaid", perfCurrency, now, now}
		})

	// usage_events: one per customer (distinct idem tuple via source_id).
	copyRows(ctx, t, pool, "usage_events",
		[]string{"id", "merchant_id", "customer_id", "invoker_id", "currency", "event_type", "amount", "source", "source_id", "occurred_at", "created_at"},
		scale, func(i int) []any {
			return []any{perfUsageUUID(i), merchantID, perfCustomerUUID(i), "perf", perfCurrency, "api_call", int64(7), "perf", fmt.Sprintf("u-%012d", i), now.Add(-2 * time.Hour), now}
		})
	// fat customer: many usage events across a couple event types (rollup fan-out).
	copyRows(ctx, t, pool, "usage_events",
		[]string{"id", "merchant_id", "customer_id", "invoker_id", "currency", "event_type", "amount", "source", "source_id", "occurred_at", "created_at"},
		perfFatUsage, func(j int) []any {
			et := "api_call"
			if j%2 == 1 {
				et = "storage"
			}
			return []any{perfUsageUUID(scale + j), merchantID, fatCustomerID, "perf", perfCurrency, et, int64(3), "perf", fmt.Sprintf("uf-%012d", j), now.Add(-time.Duration(j) * time.Minute), now}
		})

	// payment_methods: one stored instrument per customer.
	copyRows(ctx, t, pool, "payment_methods",
		[]string{"id", "rail", "initial_transaction_id", "merchant_id", "customer_id", "rail_customer_ref", "rail_method_ref", "created_at", "updated_at"},
		scale, func(i int) []any {
			return []any{perfPmUUID(i), "ccbill", fmt.Sprintf("init-%012d", i), merchantID, perfCustomerUUID(i), "", fmt.Sprintf("pm-%012d", i), now, now}
		})
	// fat customer: many stored instruments (ORDER BY created_at DESC fan-out).
	copyRows(ctx, t, pool, "payment_methods",
		[]string{"id", "rail", "initial_transaction_id", "merchant_id", "customer_id", "rail_customer_ref", "rail_method_ref", "created_at", "updated_at"},
		perfFatPayMethods, func(j int) []any {
			return []any{perfPmUUID(scale + j), "ccbill", fmt.Sprintf("init-fat-%012d", j), merchantID, fatCustomerID, "", fmt.Sprintf("pm-fat-%012d", j), now.Add(-time.Duration(j) * time.Minute), now}
		})

	for _, tbl := range []string{
		"customers", "entitlements", "subscriptions", "payments", "grants",
		"ledger_accounts", "money_settings", "usage_events", "payment_methods",
	} {
		_, err = pool.Exec(ctx, "ANALYZE openrails."+tbl)
		require.NoError(t, err)
	}

	subjects := []string{
		perfCustomerUUID(1).String(),
		perfCustomerUUID(scale / 2).String(),
		hotCustomerID.String(),
	}
	return perfSeed{HotCustomerID: hotCustomerID, HotSubjects: subjects, FatCustomerID: fatCustomerID, FatSubID: fatSubID}
}

// copyRows bulk-inserts n rows into openrails.<table> via CopyFrom.
func copyRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string, cols []string, n int, row func(i int) []any) {
	t.Helper()
	src := pgx.CopyFromSlice(n, func(i int) ([]any, error) { return row(i), nil })
	count, err := pool.CopyFrom(ctx, pgx.Identifier{"openrails", table}, cols, src)
	require.NoError(t, err, "copy %s", table)
	require.EqualValues(t, n, count, "copy %s row count", table)
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

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	return append(b, '\n')
}

func perfCustomerUUID(i int) uuid.UUID { return perfUUID("10000000", i) }
func perfSubUUID(i int) uuid.UUID      { return perfUUID("20000000", i) }
func perfEntUUID(i int) uuid.UUID      { return perfUUID("30000000", i) }
func perfGrantUUID(i int) uuid.UUID    { return perfUUID("40000000", i) }
func perfPayUUID(i int) uuid.UUID      { return perfUUID("50000000", i) }
func perfLedgerUUID(i int) uuid.UUID   { return perfUUID("60000000", i) }
func perfUsageUUID(i int) uuid.UUID    { return perfUUID("80000000", i) }
func perfPmUUID(i int) uuid.UUID       { return perfUUID("90000000", i) }

func perfUUID(prefix string, i int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, i))
}
