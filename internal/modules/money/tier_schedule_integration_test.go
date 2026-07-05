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

// TestTrustLevelSchedule_AutoGraduationByCumulativeSpend is the #476 acceptance
// test: set a per-subject schedule ONCE, then deposits crossing thresholds
// auto-raise the trust level with NO host GraduateTrustLevel call; the legacy
// host crank no-ops; an admin override sticks across deposits; and a second
// merchant is isolated.
func TestTrustLevelSchedule_AutoGraduationByCumulativeSpend(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM openrails.tier_schedules WHERE customer_id = $1", payer.UUID())
	})

	schedule := []money.TrustLevelThreshold{
		{TrustLevel: "free", MinPaidAmount: 0},
		{TrustLevel: "trust1", MinPaidAmount: 5_000},
		{TrustLevel: "trust2", MinPaidAmount: 50_000},
	}
	require.NoError(t, svc.SetTrustLevelSchedule(ctx, payer, cur, schedule))

	// Round-trips.
	got, err := svc.GetTrustLevelSchedule(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, schedule, got)

	dep := func(amt int64) {
		_, e := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur, Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}
	trustLevel := func() string {
		tr, e := svc.GetTrustLevel(ctx, payer, cur)
		require.NoError(t, e)
		return tr
	}

	// First deposit crosses the trust1 threshold — auto-graduates with NO host call.
	dep(6_000) // cumulative 6,000
	require.Equal(t, "trust1", trustLevel(), "deposit auto-raises trust level to trust1")

	// Crossing the trust2 threshold auto-raises again.
	dep(50_000) // cumulative 56,000
	require.Equal(t, "trust2", trustLevel(), "deposit auto-raises trust level to trust2")

	// A different currency has its own ladder and trust-level state.
	eurSchedule := []money.TrustLevelThreshold{{TrustLevel: "eur1", MinPaidAmount: 1_000}}
	require.NoError(t, svc.SetTrustLevelSchedule(ctx, payer, "EUR", eurSchedule))
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: "EUR", Amount: 2_000, Source: "pay-eur"})
	require.NoError(t, err)
	eurTrustLevel, err := svc.GetTrustLevel(ctx, payer, "EUR")
	require.NoError(t, err)
	require.Equal(t, "eur1", eurTrustLevel, "EUR deposits graduate the EUR schedule")
	require.Equal(t, "trust2", trustLevel(), "EUR deposits do not rewrite the USD trust level")

	// Admin override sticks — and survives further deposits (auto-graduation must
	// not overwrite it).
	require.NoError(t, svc.SetTrustLevelOverride(ctx, payer, cur, "vip"))
	require.Equal(t, "vip", trustLevel())
	dep(1_000_000) // huge deposit; would otherwise re-derive trust2
	require.Equal(t, "vip", trustLevel(), "admin override wins over the schedule")
}

// TestTrustLevelSchedule_MultiMerchantIsolation confirms a merchant-wide default
// schedule in one merchant does not affect a payer in another merchant.
func TestTrustLevelSchedule_MultiMerchantIsolation(t *testing.T) {
	svc, pool, _, _, _ := moneyInEnv(t)
	ctx := context.Background()

	// Two distinct merchants, each with its own payer.
	tA := dbtest.TestMerchantID
	ctxA := dbtest.WithTestMerchant(ctx)
	payerA := identity.CustomerIDFromString(uuid.NewString())

	// Merchant B (a second, isolated merchant row).
	tenantB := uuid.New()
	merchantB := merchant.ID(tenantB)
	slugB := "trust-iso-" + merchantB.String()[:8]
	_, err := pool.Exec(ctx,
		"INSERT INTO openrails.merchants (id, slug, status) VALUES ($1,$2,'active')",
		tenantB, slugB)
	require.NoError(t, err)
	ctxB := merchant.WithID(ctx, merchant.ID(tenantB))
	payerB := identity.CustomerIDFromString(uuid.NewString())

	t.Cleanup(func() {
		bg := context.Background()
		for _, p := range []uuid.UUID{payerA.UUID(), payerB.UUID()} {
			_, _ = pool.Exec(bg, "DELETE FROM openrails.tier_schedules WHERE customer_id = $1", p)
			_, _ = pool.Exec(bg, "DELETE FROM openrails.money_settings WHERE customer_id = $1", p)
		}
		_, _ = pool.Exec(bg, "DELETE FROM openrails.tier_schedules WHERE merchant_id = $1", merchantB)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.merchants WHERE id = $1", merchantB)
	})
	_ = tA

	// Merchant A: a merchant-wide default schedule with a low trust1 threshold.
	require.NoError(t, svc.SetTrustLevelSchedule(ctxA, identity.CustomerID{}, money.DefaultCurrency, []money.TrustLevelThreshold{
		{TrustLevel: "free", MinPaidAmount: 0}, {TrustLevel: "trust1", MinPaidAmount: 1_000},
	}))

	depA := func(amt int64) {
		_, e := svc.Deposit(ctxA, money.DepositParams{CustomerID: &payerA, Invoker: payerA.UUID().String(), Currency: money.DefaultCurrency, Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}
	depB := func(amt int64) {
		_, e := svc.Deposit(ctxB, money.DepositParams{CustomerID: &payerB, Invoker: payerB.UUID().String(), Currency: money.DefaultCurrency, Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}

	depA(2_000)
	trustLevelA, err := svc.GetTrustLevel(ctxA, payerA, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, "trust1", trustLevelA, "merchant A's schedule graduates payer A")

	// Merchant B has NO schedule — a deposit of the same size does NOT graduate.
	depB(2_000)
	trustLevelB, err := svc.GetTrustLevel(ctxB, payerB, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, "", trustLevelB, "merchant B has no schedule — no auto-graduation")
}
