//go:build integration

package service_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/stretchr/testify/require"
)

// Real admission and capture append one usage event and debit once, including
// exact retries, while resource and tier rollups retain the original attribution.
func TestServiceUsageRollup_NoDoubleDebit_GroupsByDimension(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)

	pool := testPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
	})

	_, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000, Source: "seed",
	})
	require.NoError(t, err)

	before, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
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
		e.src = uuid.NewString()
		admit, err := svc.Admit(ctx, billingservice.AdmitInput{CustomerID: payer, Invoker: "user:a", InvokerType: "payer",
			Currency: money.DefaultCurrency, EstimatedAmount: e.amount, SourceID: e.src, ExpiresAtUnix: time.Now().Add(time.Hour).Unix()})
		require.NoError(t, err)
		require.True(t, admit.Allowed)
		req := billingservice.CaptureHoldRequest{RequestID: e.src, Amount: e.amount, EventType: "owner/" + e.endpoint, Resource: e.endpoint,
			Metadata: map[string]any{"function_name": "gen", "availability_tier": e.tier}, Source: "invoke", SourceID: e.src}
		first, err := svc.CaptureHold(ctx, req)
		require.NoError(t, err)
		replay, err := svc.CaptureHold(ctx, req)
		require.NoError(t, err)
		first.Replayed = true
		require.Equal(t, first, replay)
	}
	after, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, before.Balance-12, after.Balance, "three captures debit once each, including usage and retries")

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	// Rollup by resource: alpha=8 over 2 events, beta=4 over 1.
	byEndpoint, err := ms.ServiceUsageRollup(ctx, payer, money.DefaultCurrency, from, to, "resource")
	require.NoError(t, err)
	endpoints := map[string]money.ServiceUsageRollupRow{}
	for _, r := range byEndpoint {
		endpoints[r.Key] = r
	}
	require.Equal(t, int64(8), endpoints["alpha"].TotalAmount)
	require.Equal(t, int64(2), endpoints["alpha"].EventCount)
	require.Equal(t, int64(4), endpoints["beta"].TotalAmount)

	// Rollup by tier: standard=8, fast=4.
	byTier, err := ms.ServiceUsageRollup(ctx, payer, money.DefaultCurrency, from, to, "tier")
	require.NoError(t, err)
	tiers := map[string]int64{}
	for _, r := range byTier {
		tiers[r.Key] = r.TotalAmount
	}
	require.Equal(t, int64(8), tiers["standard"])
	require.Equal(t, int64(4), tiers["fast"])

	// Invalid group_by is rejected.
	_, err = ms.ServiceUsageRollup(ctx, payer, money.DefaultCurrency, from, to, "bogus")
	require.Error(t, err)
}
