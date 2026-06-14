//go:build integration

package money_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestGraduateTier_ByCumulativePaid(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	ladder := []money.TierThreshold{
		{Tier: "free", MinPaidMicros: 0},
		{Tier: "tier1", MinPaidMicros: 5_000},
		{Tier: "tier2", MinPaidMicros: 50_000},
	}
	dep := func(amt int64) {
		_, e := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Actor: payer.UUID().String(), Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}

	// No paid spend -> free.
	tier, err := svc.GraduateTier(ctx, payer, ladder)
	require.NoError(t, err)
	require.Equal(t, "free", tier)

	dep(6_000) // cumulative 6,000 -> tier1
	tier, err = svc.GraduateTier(ctx, payer, ladder)
	require.NoError(t, err)
	require.Equal(t, "tier1", tier)

	dep(50_000) // cumulative 56,000 -> tier2
	tier, err = svc.GraduateTier(ctx, payer, ladder)
	require.NoError(t, err)
	require.Equal(t, "tier2", tier)

	// Persisted + readable.
	got, err := svc.GetTier(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, "tier2", got)
}
