//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// TestTierSchedule_AutoGraduationByCumulativeSpend is the #476 acceptance test:
// set a per-subject schedule ONCE, then deposits crossing thresholds auto-raise
// the tier with NO host GraduateTier call; the legacy host crank no-ops; an
// admin override sticks across deposits; and a second tenant is isolated.
func TestTierSchedule_AutoGraduationByCumulativeSpend(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM openrails.tier_schedules WHERE merchant_subject_id = $1", payer.UUID())
	})

	schedule := []money.TierThreshold{
		{Tier: "free", MinPaidMicros: 0},
		{Tier: "tier1", MinPaidMicros: 5_000},
		{Tier: "tier2", MinPaidMicros: 50_000},
	}
	require.NoError(t, svc.SetTierSchedule(ctx, payer, schedule))

	// Round-trips.
	got, err := svc.GetTierSchedule(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, schedule, got)

	dep := func(amt int64) {
		_, e := svc.Deposit(ctx, money.DepositParams{MerchantSubjectID: &payer, Actor: payer.UUID().String(), Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}
	tier := func() string {
		tr, e := svc.GetTier(ctx, payer)
		require.NoError(t, e)
		return tr
	}

	// First deposit crosses the tier1 threshold — auto-graduates with NO host call.
	dep(6_000) // cumulative 6,000
	require.Equal(t, "tier1", tier(), "deposit auto-raises tier to tier1")

	// Crossing the tier2 threshold auto-raises again.
	dep(50_000) // cumulative 56,000
	require.Equal(t, "tier2", tier(), "deposit auto-raises tier to tier2")

	// Host-cranked GraduateTier is a NO-OP when a schedule exists (returns the
	// auto-maintained tier, does not regress it with a bogus ladder).
	res, err := svc.GraduateTier(ctx, payer, []money.TierThreshold{{Tier: "free", MinPaidMicros: 0}})
	require.NoError(t, err)
	require.Equal(t, "tier2", res, "GraduateTier no-ops to the auto-maintained tier when a schedule exists")
	require.Equal(t, "tier2", tier())

	// Admin override sticks — and survives further deposits (auto-graduation must
	// not overwrite it).
	require.NoError(t, svc.SetTierOverride(ctx, payer, "vip"))
	require.Equal(t, "vip", tier())
	dep(1_000_000) // huge deposit; would otherwise re-derive tier2
	require.Equal(t, "vip", tier(), "admin override wins over the schedule")
}

// TestTierSchedule_MultiTenantIsolation confirms a tenant-wide default schedule
// in one tenant does not affect a payer in another tenant.
func TestTierSchedule_MultiTenantIsolation(t *testing.T) {
	svc, pool, _, _, _ := moneyInEnv(t)
	ctx := context.Background()

	// Two distinct tenants, each with its own payer.
	tA := dbtest.TestTenantID
	ctxA := dbtest.WithTestTenant(ctx)
	payerA := identity.MerchantSubjectIDFromString(uuid.NewString())

	// Tenant B (a second, isolated tenant row).
	tenantB := uuid.New()
	slugB := "tier-iso-" + tenantB.String()[:8]
	_, err := pool.Exec(ctx,
		"INSERT INTO openrails.merchants (id, slug, name, status) VALUES ($1,$2,$2,'active')",
		tenantB, slugB)
	require.NoError(t, err)
	ctxB := merchant.WithID(ctx, merchant.ID(tenantB))
	payerB := identity.MerchantSubjectIDFromString(uuid.NewString())

	t.Cleanup(func() {
		bg := context.Background()
		for _, p := range []uuid.UUID{payerA.UUID(), payerB.UUID()} {
			_, _ = pool.Exec(bg, "DELETE FROM openrails.tier_schedules WHERE merchant_subject_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_accounts WHERE merchant_subject_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_blocks WHERE merchant_subject_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_transactions WHERE merchant_subject_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_balances WHERE merchant_subject_id = $1", p)
		}
		_, _ = pool.Exec(bg, "DELETE FROM openrails.tier_schedules WHERE merchant_id = $1", tenantB)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.merchants WHERE id = $1", tenantB)
	})
	_ = tA

	// Tenant A: a tenant-wide default schedule with a low tier1 threshold.
	require.NoError(t, svc.SetTierSchedule(ctxA, identity.MerchantSubjectID{}, []money.TierThreshold{
		{Tier: "free", MinPaidMicros: 0}, {Tier: "tier1", MinPaidMicros: 1_000},
	}))

	depA := func(amt int64) {
		_, e := svc.Deposit(ctxA, money.DepositParams{MerchantSubjectID: &payerA, Actor: payerA.UUID().String(), Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}
	depB := func(amt int64) {
		_, e := svc.Deposit(ctxB, money.DepositParams{MerchantSubjectID: &payerB, Actor: payerB.UUID().String(), Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}

	depA(2_000)
	tierA, err := svc.GetTier(ctxA, payerA)
	require.NoError(t, err)
	require.Equal(t, "tier1", tierA, "tenant A's schedule graduates payer A")

	// Tenant B has NO schedule — a deposit of the same size does NOT graduate.
	depB(2_000)
	tierB, err := svc.GetTier(ctxB, payerB)
	require.NoError(t, err)
	require.Equal(t, "", tierB, "tenant B has no schedule — no auto-graduation")
}
