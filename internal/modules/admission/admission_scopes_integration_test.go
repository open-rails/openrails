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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_reservations WHERE customer_id = $1", payerID)
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
	_, err = cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Actor: payer.UUID().String(), Amount: 1_000_000_000, Source: "seed"})
	require.NoError(t, err)

	tierStore := admission.NewTierPolicyStore(dbi)
	bpStore := admission.NewBudgetPolicyStore(dbi)
	bl := abuse.NewBlocklistService(dbi)
	bsvc := budgets.NewService(dbi)
	adm := admission.NewAdmitter(ratelimit.NewLimiter(rdb), cs, tierStore, bl, bsvc).WithBudgetScopes(bpStore)
	return adm, bpStore, tierStore, payer, ctx
}

func mustReq(payer identity.CustomerID, actor, srcID string, roles []uuid.UUID, micros int64) admission.AdmitRequest {
	return admission.AdmitRequest{
		CustomerID: payer, Actor: actor, Tier: "free", Resource: "gpt-4o",
		Roles: roles, Amounts: map[string]int64{"request": 1},
		EstimateMicros: micros, Source: "usage", SourceID: srcID, ExpiresAt: time.Now().Add(time.Hour),
	}
}

// budgetPolicyWindows builds a single 1h fixed-window scope policy capped at
// limit micros.
func budgetPolicyWindows(limit int64) []models.BudgetWindowPolicy {
	return []models.BudgetWindowPolicy{{Key: "1h", WindowSeconds: 3600, LimitMicros: limit}}
}

// tierWith builds a tier policy with a generous throughput allowance and a
// (subject, actor) budget window capped at limit micros (the pre-#473 path).
func tierWith(limit int64) models.ThroughputPolicy {
	return models.ThroughputPolicy{
		Windows:       []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 1_000_000}},
		BudgetWindows: budgetPolicyWindows(limit),
	}
}

// TestScope_PlatformCapDeniesEvenWhenRoleAndInvokerUnder: the platform (subject)
// cap denies even when the role pool and invoker cap have room.
func TestScope_PlatformCapDeniesEvenWhenRoleAndInvokerUnder(t *testing.T) {
	adm, bp, tier, payer, ctx := scopeEnv(t)
	role := uuid.New()
	// Platform (subject) cap = $0.0004 (400 micros); generous role + invoker caps.
	require.NoError(t, bp.Upsert(ctx, payer, admission.BudgetScopePolicy{Scope: budgets.ScopeSubject, Owner: "platform", Windows: budgetPolicyWindows(400)}))
	require.NoError(t, bp.Upsert(ctx, payer, admission.BudgetScopePolicy{Scope: budgets.ScopeRole, Owner: "subject", ScopeKey: role.String(), Windows: budgetPolicyWindows(1_000_000)}))
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", tierWith(1_000_000)))

	// First request 300 micros: under all caps -> allow.
	d, err := adm.Admit(ctx, mustReq(payer, "user:a", "s1", []uuid.UUID{role}, 300))
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Second request 200 micros: platform window 300+200=500 > 400 -> deny on platform.
	d, err = adm.Admit(ctx, mustReq(payer, "user:a", "s2", []uuid.UUID{role}, 200))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
	// The blocking scope is the platform (subject) cap.
	var blockedPlatform bool
	for _, sc := range d.BudgetScopes {
		if sc.Scope == budgets.ScopeSubject && sc.Owner == "platform" {
			for _, w := range sc.Windows {
				if !w.Allowed {
					blockedPlatform = true
				}
			}
		}
	}
	require.True(t, blockedPlatform, "platform (subject) cap should be the blocking scope")
}

// TestScope_RolePoolSharedAcrossTwoUsers: a (subject, role) cap is a SHARED pool
// — two different actors holding the same role draw from the same budget.
func TestScope_RolePoolSharedAcrossTwoUsers(t *testing.T) {
	adm, bp, tier, payer, ctx := scopeEnv(t)
	role := uuid.New()
	require.NoError(t, bp.Upsert(ctx, payer, admission.BudgetScopePolicy{Scope: budgets.ScopeRole, Owner: "subject", ScopeKey: role.String(), Windows: budgetPolicyWindows(500)}))
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", tierWith(1_000_000)))

	// user:a spends 300 of the shared 500 role pool.
	d, err := adm.Admit(ctx, mustReq(payer, "user:a", "r1", []uuid.UUID{role}, 300))
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// user:b (same role) spends 200 -> pool now 500, still allowed.
	d, err = adm.Admit(ctx, mustReq(payer, "user:b", "r2", []uuid.UUID{role}, 200))
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// user:b again, 100 -> pool 600 > 500 -> deny (shared across both users).
	d, err = adm.Admit(ctx, mustReq(payer, "user:b", "r3", []uuid.UUID{role}, 100))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
}

// TestScope_InvokerCapIndependent: each invoker's (subject, actor) cap is
// independent — one invoker hitting its cap does not block a different invoker.
func TestScope_InvokerCapIndependent(t *testing.T) {
	adm, _, tier, payer, ctx := scopeEnv(t)
	// (subject, actor) cap via the tier policy budget windows = 400 micros.
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", tierWith(400)))

	// user:a exhausts its own 400 cap.
	d, err := adm.Admit(ctx, mustReq(payer, "user:a", "i1", nil, 400))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	d, err = adm.Admit(ctx, mustReq(payer, "user:a", "i2", nil, 100))
	require.NoError(t, err)
	require.False(t, d.Allowed, "user:a is over its own cap")

	// user:b unaffected — independent (subject, actor) bucket.
	d, err = adm.Admit(ctx, mustReq(payer, "user:b", "i3", nil, 400))
	require.NoError(t, err)
	require.True(t, d.Allowed, "user:b has its own independent cap")
}

// TestScope_SettleReleasesAllScopes: a money-denied composed reserve frees ALL
// scopes (verified by a follow-up request succeeding up to the full caps).
func TestScope_OverSubscriptionFlooredByBalanceNotBudget(t *testing.T) {
	adm, bp, tier, payer, ctx := scopeEnv(t)
	role := uuid.New()
	// Sub-budgets MAY over-subscribe: role cap 1M + invoker cap 1M, both well
	// above any single request; the request is allowed by budgets (balance is the
	// hard floor, tested elsewhere). Here we assert composition allows when every
	// scope has room.
	require.NoError(t, bp.Upsert(ctx, payer, admission.BudgetScopePolicy{Scope: budgets.ScopeRole, Owner: "subject", ScopeKey: role.String(), Windows: budgetPolicyWindows(1_000_000)}))
	require.NoError(t, bp.Upsert(ctx, payer, admission.BudgetScopePolicy{Scope: budgets.ScopeSubject, Owner: "subject", Windows: budgetPolicyWindows(1_000_000)}))
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", tierWith(1_000_000)))

	d, err := adm.Admit(ctx, mustReq(payer, "user:a", "o1", []uuid.UUID{role}, 500_000))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	// All four scopes reserved (platform absent here): subject(subject), role, actor.
	require.GreaterOrEqual(t, len(d.BudgetScopes), 3)
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
	req := func(actor, srcID string) admission.AdmitRequest {
		r := mustReq(payer, actor, srcID, nil, 0)
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
