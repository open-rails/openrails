//go:build integration

package service_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// TestServiceUsageRollup_NoDoubleDebit_GroupsByDimension proves #311:
// InsertCaptureUsageEvent appends a usage_event WITHOUT a second ledger debit
// (distinct from RecordUsage, which debits), and ServiceUsageRollup returns
// per-dimension-VALUE spend grouped by endpoint / tier. This is the data behind
// the tensorhub platform /budget-usage surface (#410).
func TestServiceUsageRollup_NoDoubleDebit_GroupsByDimension(t *testing.T) {
	_, cs, payer, ct, ctx := authzEnv(t)

	cleanupDB := bun.NewDB(
		sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.SharedPostgresDSN(t)))),
		pgdialect.New(),
	)
	models.RegisterModels(cleanupDB)
	t.Cleanup(func() {
		_, _ = cleanupDB.NewDelete().Model((*models.UsageEvent)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
		_ = cleanupDB.Close()
	})

	ctType, err := cs.GetCreditTypeByName(ctx, ct)
	require.NoError(t, err)

	_, err = cs.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, InvokerID: payer.UUID().String(), CreditType: ct, Amount: 100_000, Source: "seed",
	})
	require.NoError(t, err)

	before, err := cs.GetBalanceForTenantSubject(ctx, payer, ct)
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
		require.NoError(t, cs.InsertCaptureUsageEvent(ctx, credits.CaptureUsageEventParams{
			TenantSubjectID: payer.UUID(),
			InvokerID:       "user:a",
			CreditTypeID:    ctType.ID,
			EventType:       "owner/" + e.endpoint,
			Amount:          e.amount,
			Metadata: map[string]any{
				"endpoint_name":     e.endpoint,
				"function_name":     "gen",
				"availability_tier": e.tier,
				"delegated_user_id": "user:a",
			},
			Source:   "invoke",
			SourceID: e.src,
		}))
	}

	// No second debit: usage-event inserts must NOT move the ledger balance.
	after, err := cs.GetBalanceForTenantSubject(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, before.Balance, after.Balance, "InsertCaptureUsageEvent must not debit the ledger")

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	// Rollup by endpoint: alpha=8 over 2 events, beta=4 over 1.
	byEndpoint, err := cs.ServiceUsageRollup(ctx, payer, from, to, "endpoint")
	require.NoError(t, err)
	endpoints := map[string]credits.ServiceUsageRollupRow{}
	for _, r := range byEndpoint {
		endpoints[r.Key] = r
	}
	require.Equal(t, int64(8), endpoints["alpha"].TotalAmount)
	require.Equal(t, int64(2), endpoints["alpha"].EventCount)
	require.Equal(t, int64(4), endpoints["beta"].TotalAmount)

	// Rollup by tier: standard=8, fast=4.
	byTier, err := cs.ServiceUsageRollup(ctx, payer, from, to, "tier")
	require.NoError(t, err)
	tiers := map[string]int64{}
	for _, r := range byTier {
		tiers[r.Key] = r.TotalAmount
	}
	require.Equal(t, int64(8), tiers["standard"])
	require.Equal(t, int64(4), tiers["fast"])

	// Invalid group_by is rejected.
	_, err = cs.ServiceUsageRollup(ctx, payer, from, to, "bogus")
	require.Error(t, err)
}
