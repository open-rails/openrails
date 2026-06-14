//go:build integration

package service_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// TestServiceUsageRollup_NoDoubleDebit_GroupsByDimension proves #311:
// InsertCaptureUsageEvent appends a usage_event WITHOUT a second ledger debit
// (distinct from RecordUsage, which debits), and ServiceUsageRollup returns
// per-dimension-VALUE spend grouped by endpoint / tier. This is the data behind
// the tensorhub platform /budget-usage surface (#410).
func TestServiceUsageRollup_NoDoubleDebit_GroupsByDimension(t *testing.T) {
	_, ms, payer, ctx := authzEnv(t)

	pool := testPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE tenant_subject_id = $1", payer.UUID())
	})

	_, err := ms.Deposit(ctx, money.DepositParams{
		TenantSubjectID: &payer, Actor: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000, Source: "seed",
	})
	require.NoError(t, err)

	before, err := ms.GetBalanceForTenantSubject(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)

	// Two captures on endpoint "alpha" (standard tier), one on "beta" (fast tier).
	events := []struct {
		endpoint, tier, src string
		amount              int64
	}{
		{"alpha", "standard", "r1", 5},
		{"alpha", "standard", "r2", 3},
		{"beta", "fast", "r3", 4},
	}
	for _, e := range events {
		require.NoError(t, ms.InsertCaptureUsageEvent(ctx, money.CaptureUsageEventParams{
			TenantSubjectID: payer.UUID(),
			Actor:           "user:a",
			EventType:       "owner/" + e.endpoint,
			Amount:          e.amount,
			Resource:        e.endpoint,
			Metadata: map[string]any{
				"function_name":     "gen",
				"availability_tier": e.tier,
			},
			Source:   "invoke",
			SourceID: e.src,
		}))
	}

	// No second debit: usage-event inserts must NOT move the ledger balance.
	after, err := ms.GetBalanceForTenantSubject(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, before.Balance, after.Balance, "InsertCaptureUsageEvent must not debit the ledger")

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	// Rollup by resource: alpha=8 over 2 events, beta=4 over 1.
	byEndpoint, err := ms.ServiceUsageRollup(ctx, payer, from, to, "resource")
	require.NoError(t, err)
	endpoints := map[string]money.ServiceUsageRollupRow{}
	for _, r := range byEndpoint {
		endpoints[r.Key] = r
	}
	require.Equal(t, int64(8), endpoints["alpha"].TotalAmount)
	require.Equal(t, int64(2), endpoints["alpha"].EventCount)
	require.Equal(t, int64(4), endpoints["beta"].TotalAmount)

	// Rollup by tier: standard=8, fast=4.
	byTier, err := ms.ServiceUsageRollup(ctx, payer, from, to, "tier")
	require.NoError(t, err)
	tiers := map[string]int64{}
	for _, r := range byTier {
		tiers[r.Key] = r.TotalAmount
	}
	require.Equal(t, int64(8), tiers["standard"])
	require.Equal(t, int64(4), tiers["fast"])

	// Invalid group_by is rejected.
	_, err = ms.ServiceUsageRollup(ctx, payer, from, to, "bogus")
	require.Error(t, err)
}
