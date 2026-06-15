//go:build integration

package admission_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// #487: per-charge ceiling. An Admit whose estimate exceeds the tier's
// max_single_charge_amount is rejected up front with the generic deny code.
func TestAdmit_SingleChargeCapDeny(t *testing.T) {
	adm, cs, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:               []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}},
		MaxSingleChargeAmount: 1_000,
	}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1_000_000, Source: "seed"})
	require.NoError(t, err)

	// Over the per-charge ceiling -> deny (even though the balance covers it).
	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 1_001,
		Source: "usage", SourceID: "big1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "money", d.BlockedBy)
	require.Equal(t, admission.DenySingleChargeCap, d.DenyCode)
	require.Equal(t, int64(1_000), d.MaxSingleChargeAmount)
	require.Nil(t, d.Hold, "a rejected single-charge places no hold")

	// At the ceiling -> allowed; the resolved cap is surfaced.
	d, err = adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 1_000,
		Source: "usage", SourceID: "ok1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.Equal(t, int64(1_000), d.MaxSingleChargeAmount)
}

// #487: concurrent-held $ cap. The sum of a payer's ACTIVE (un-settled) hold $
// plus the new estimate may not exceed the tier's max_concurrent_held_amount.
func TestAdmit_ConcurrentHeldCapDeny(t *testing.T) {
	adm, cs, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:                 []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}},
		MaxConcurrentHeldAmount: 1_500,
	}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1_000_000, Source: "seed"})
	require.NoError(t, err)

	// First hold (1000) is well under the cap; surfaced held == the placed hold.
	d1, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 1_000,
		Source: "usage", SourceID: "h1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d1.Allowed)
	require.Equal(t, int64(1_500), d1.MaxConcurrentHeldAmount)
	require.Equal(t, int64(1_000), d1.HeldAmount, "allowed verdict includes the hold just placed")

	// Second hold (1000) would push active-held to 2000 > 1500 -> deny.
	d2, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 1_000,
		Source: "usage", SourceID: "h2", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, d2.Allowed)
	require.Equal(t, "money", d2.BlockedBy)
	require.Equal(t, admission.DenyConcurrentHeldCap, d2.DenyCode)
	require.Equal(t, int64(1_500), d2.MaxConcurrentHeldAmount)
	require.Equal(t, int64(1_000), d2.HeldAmount, "deny surfaces the pre-existing held sum")
	require.Nil(t, d2.Hold)
}

// #487: a tier with no $-limits keeps the pre-#487 behavior (0 = uncapped).
func TestAdmit_NoAdmitLimits_Unchanged(t *testing.T) {
	adm, cs, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1_000_000, Source: "seed"})
	require.NoError(t, err)

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 900_000,
		Source: "usage", SourceID: "big", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.Equal(t, int64(0), d.MaxSingleChargeAmount)
	require.Equal(t, int64(0), d.MaxConcurrentHeldAmount)
}
