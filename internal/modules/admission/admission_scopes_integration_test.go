//go:build integration

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// scopeEnv builds an admitter WITH hierarchical budget scopes (#473) wired,
// plus the budget-policy store + tier-policy store, a fresh payer, and a
// generously funded USD money balance so the money axis never blocks the
// budget-scope tests. Money has no credit_type dimension (#472).
func scopeEnv(t *testing.T) (*admission.Admitter, *admission.BudgetPolicyStore, *admission.TierPolicyStore, identity.CustomerID, context.Context) {
	t.Helper()
	ctx := context.Background()

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_policies WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.tier_policies WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_inflight_holds WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_window_state WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})

	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Terminate(ctx) })
	conn, err := rc.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(conn)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())

	cs := money.NewMoneyService(dbi)
	// Fund the payer generously so money never blocks (budgets are the gate here).
	_, err = cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1_000_000_000, Source: "seed"})
	require.NoError(t, err)

	tierStore := admission.NewTierPolicyStore(dbi)
	bpStore := admission.NewBudgetPolicyStore(dbi)
	bl := abuse.NewBlocklistService(dbi)
	bsvc := budgets.NewService(dbi)
	adm := admission.NewAdmitter(ratelimit.NewLimiter(rdb), cs, tierStore, bl, bsvc).WithBudgetScopes(bpStore)
	return adm, bpStore, tierStore, payer, ctx
}

func mustReq(payer identity.CustomerID, invoker, srcID string, roles []uuid.UUID, amount int64) admission.AdmitRequest {
	return admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: invoker, Tier: "free", Resource: "gpt-4o",
		Roles: roles, Amounts: map[string]int64{"request": 1},
		EstimatedAmount: amount, Source: "usage", SourceID: srcID, ExpiresAt: time.Now().Add(time.Hour),
	}
}

// budgetPolicyWindows builds a single 1h fixed-window scope policy capped at
// limit amount.
func budgetPolicyWindows(limit int64) []models.BudgetWindowPolicy {
	return []models.BudgetWindowPolicy{{Key: "1h", WindowSeconds: 3600, Limit: limit}}
}

// tierWith builds a tier policy with a generous throughput allowance and a
// (subject, invoker) budget window capped at limit amount (the pre-#473 path).
func tierWith(limit int64) models.ThroughputPolicy {
	return models.ThroughputPolicy{
		Windows:       []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 1_000_000}},
		BudgetWindows: budgetPolicyWindows(limit),
	}
}

// TestScope_PlatformCapDeniesEvenWhenRoleAndInvokerUnder: the platform (subject)
// cap denies even when the role pool and invoker cap have room.
// TestScope_InvokerCapIndependent: each invoker's (subject, invoker) cap is
// independent — one invoker hitting its cap does not block a different invoker.
func TestScope_InvokerCapIndependent(t *testing.T) {
	adm, _, tier, payer, ctx := scopeEnv(t)
	// (subject, invoker) cap via the tier policy budget windows = 400 internal units.
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", tierWith(400)))

	// user:a exhausts its own 400 cap.
	d, err := adm.Admit(ctx, mustReq(payer, "user:a", "i1", nil, 400))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	d, err = adm.Admit(ctx, mustReq(payer, "user:a", "i2", nil, 100))
	require.NoError(t, err)
	require.False(t, d.Allowed, "user:a is over its own cap")

	// user:b unaffected — independent (subject, invoker) bucket.
	d, err = adm.Admit(ctx, mustReq(payer, "user:b", "i3", nil, 400))
	require.NoError(t, err)
	require.True(t, d.Allowed, "user:b has its own independent cap")
}

// TestThroughput_PayerAggregatesAcrossInvokers (#472 G1): the throughput counter
// keys on (payer, release) and DROPS the invoker, so two different invokers of
// one payer share the SAME per-release limit.
func TestThroughput_PayerAggregatesAcrossInvokers(t *testing.T) {
	adm, _, tier, payer, ctx := scopeEnv(t)
	// 3 requests/min on release "rel-A"; no money axis.
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows: []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 3}},
	}))
	req := func(invoker, srcID string) admission.AdmitRequest {
		r := mustReq(payer, invoker, srcID, nil, 0)
		r.Resource = "rel-A"
		return r
	}
	// invoker user:a uses 2, invoker user:b uses 1 -> shared counter at 3.
	for _, a := range []string{"user:a", "user:a", "user:b"} {
		d, err := adm.Admit(ctx, req(a, "x"))
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}
	// 4th from a THIRD invoker is denied — the payer's shared (payer,release)
	// counter is exhausted regardless of invoker.
	d, err := adm.Admit(ctx, req("user:c", "x"))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "throughput", d.BlockedBy)
}

// TestThroughput_PerReleaseWindows (#472 G1): two releases under one payer carry
// DIFFERENT limits via per-release window VALUES; their counters are independent.
func TestThroughput_PerReleaseWindows(t *testing.T) {
	adm, _, tier, payer, ctx := scopeEnv(t)
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows: []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}, // default
		ReleaseWindows: map[string][]models.ThroughputWindow{
			"rel-small": {{Unit: "request", WindowSeconds: 60, Max: 1}},
		},
	}))
	small := func() admission.AdmitRequest {
		r := mustReq(payer, "user:a", "x", nil, 0)
		r.Resource = "rel-small"
		return r
	}
	big := func() admission.AdmitRequest {
		r := mustReq(payer, "user:a", "x", nil, 0)
		r.Resource = "rel-big"
		return r
	}

	// rel-small caps at 1.
	d, err := adm.Admit(ctx, small())
	require.NoError(t, err)
	require.True(t, d.Allowed)
	d, err = adm.Admit(ctx, small())
	require.NoError(t, err)
	require.False(t, d.Allowed, "rel-small over its per-release cap of 1")

	// rel-big (no override) uses the default 100 and is independent.
	d, err = adm.Admit(ctx, big())
	require.NoError(t, err)
	require.True(t, d.Allowed, "rel-big uses the default window, independent of rel-small")
}

// TestQueue_AdmitDeniesAndSettlementReleases (#472 G2): the queue/batch pool
// caps in-flight requests per (payer, release); admit denies BlockedBy="queue"
// on overflow, and a released hold frees the queued unit.
func TestQueue_AdmitDeniesOnPoolOverflow(t *testing.T) {
	adm, _, tier, payer, ctx := scopeEnv(t)
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:     []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 1000}},
		QueueLimits: []models.QueueLimitPolicy{{Unit: "request", Max: 2}},
	}))
	req := func(srcID string) admission.AdmitRequest {
		r := mustReq(payer, "user:a", srcID, nil, 100) // money axis funded
		r.Resource = "rel-q"
		return r
	}
	// 2 jobs enqueued -> both held.
	d, err := adm.Admit(ctx, req("q1"))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.True(t, d.QueueAcquired)
	d, err = adm.Admit(ctx, req("q2"))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	// 3rd exceeds the queue pool of 2 -> queue deny.
	d, err = adm.Admit(ctx, req("q3"))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "queue", d.BlockedBy)
	require.Equal(t, "request", d.BlockedUnit)
}
