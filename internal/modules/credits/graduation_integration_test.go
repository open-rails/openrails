//go:build integration

package credits_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
)

func TestGraduateTier_ByCumulativePaid(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	ladder := []credits.TierThreshold{
		{Tier: "free", MinPaidCents: 0},
		{Tier: "tier1", MinPaidCents: 5_000},
		{Tier: "tier2", MinPaidCents: 50_000},
	}
	dep := func(amt int64) {
		_, e := svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, InvokerID: payer.UUID().String(), CreditType: ct, Amount: amt, Source: "pay"})
		require.NoError(t, e)
	}

	// No paid spend -> free.
	tier, err := svc.GraduateTier(ctx, payer, ct, ladder)
	require.NoError(t, err)
	require.Equal(t, "free", tier)

	dep(6_000) // cumulative 6,000 -> tier1
	tier, err = svc.GraduateTier(ctx, payer, ct, ladder)
	require.NoError(t, err)
	require.Equal(t, "tier1", tier)

	dep(50_000) // cumulative 56,000 -> tier2
	tier, err = svc.GraduateTier(ctx, payer, ct, ladder)
	require.NoError(t, err)
	require.Equal(t, "tier2", tier)

	// Persisted + readable.
	got, err := svc.GetTier(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, "tier2", got)
}
