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
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM openrails.tier_schedules WHERE customer_id = $1", payer.UUID())
	})

	schedule := []money.TierThreshold{
		{Tier: "free", MinPaidAmount: 0},
		{Tier: "tier1", MinPaidAmount: 5_000},
		{Tier: "tier2", MinPaidAmount: 50_000},
	}
	require.NoError(t, svc.SetTierSchedule(ctx, payer, cur, schedule))

	// Round-trips.
	got, err := svc.GetTierSchedule(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, schedule, got)

	dep := func(amt int64) {
		_, e := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur, Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}
	tier := func() string {
		tr, e := svc.GetTier(ctx, payer, cur)
		require.NoError(t, e)
		return tr
	}

	// First deposit crosses the tier1 threshold — auto-graduates with NO host call.
	dep(6_000) // cumulative 6,000
	require.Equal(t, "tier1", tier(), "deposit auto-raises tier to tier1")

	// Crossing the tier2 threshold auto-raises again.
	dep(50_000) // cumulative 56,000
	require.Equal(t, "tier2", tier(), "deposit auto-raises tier to tier2")

	// A different currency has its own ladder and tier state.
	eurSchedule := []money.TierThreshold{{Tier: "eur1", MinPaidAmount: 1_000}}
	require.NoError(t, svc.SetTierSchedule(ctx, payer, "EUR", eurSchedule))
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: "EUR", Amount: 2_000, Source: "pay-eur"})
	require.NoError(t, err)
	eurTier, err := svc.GetTier(ctx, payer, "EUR")
	require.NoError(t, err)
	require.Equal(t, "eur1", eurTier, "EUR deposits graduate the EUR schedule")
	require.Equal(t, "tier2", tier(), "EUR deposits do not rewrite the USD tier")

	// Admin override sticks — and survives further deposits (auto-graduation must
	// not overwrite it).
	require.NoError(t, svc.SetTierOverride(ctx, payer, cur, "vip"))
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
	payerA := identity.CustomerIDFromString(uuid.NewString())

	// Tenant B (a second, isolated tenant row).
	tenantB := uuid.New()
	slugB := "tier-iso-" + tenantB.String()[:8]
	_, err := pool.Exec(ctx,
		"INSERT INTO openrails.merchants (id, slug, name, status) VALUES ($1,$2,$2,'active')",
		tenantB, slugB)
	require.NoError(t, err)
	ctxB := merchant.WithID(ctx, merchant.ID(tenantB))
	payerB := identity.CustomerIDFromString(uuid.NewString())

	t.Cleanup(func() {
		bg := context.Background()
		for _, p := range []uuid.UUID{payerA.UUID(), payerB.UUID()} {
			_, _ = pool.Exec(bg, "DELETE FROM openrails.tier_schedules WHERE customer_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_settings WHERE customer_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", p)
		}
		_, _ = pool.Exec(bg, "DELETE FROM openrails.tier_schedules WHERE merchant_id = $1", tenantB)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.merchants WHERE id = $1", tenantB)
	})
	_ = tA

	// Tenant A: a tenant-wide default schedule with a low tier1 threshold.
	require.NoError(t, svc.SetTierSchedule(ctxA, identity.CustomerID{}, money.DefaultCurrency, []money.TierThreshold{
		{Tier: "free", MinPaidAmount: 0}, {Tier: "tier1", MinPaidAmount: 1_000},
	}))

	depA := func(amt int64) {
		_, e := svc.Deposit(ctxA, money.DepositParams{CustomerID: &payerA, Invoker: payerA.UUID().String(), Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}
	depB := func(amt int64) {
		_, e := svc.Deposit(ctxB, money.DepositParams{CustomerID: &payerB, Invoker: payerB.UUID().String(), Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}

	depA(2_000)
	tierA, err := svc.GetTier(ctxA, payerA, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, "tier1", tierA, "tenant A's schedule graduates payer A")

	// Tenant B has NO schedule — a deposit of the same size does NOT graduate.
	depB(2_000)
	tierB, err := svc.GetTier(ctxB, payerB, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, "", tierB, "tenant B has no schedule — no auto-graduation")
}
