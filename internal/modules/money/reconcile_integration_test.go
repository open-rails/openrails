//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestReconcile_Clean(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	resetMoneyLedger(t, pool, ctx)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.Hold(ctx, &payer, "user:a", cur, 200, "usage", "h1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	rep, err := svc.Reconcile(ctx)
	require.NoError(t, err)
	require.Empty(t, rep.OrphanedHolds)
}

func TestReconcile_OrphanedExpiredHold(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	resetMoneyLedger(t, pool, ctx)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	// Active hold already past expiry (HoldExpiryWorker hasn't run).
	_, err = svc.Hold(ctx, &payer, "user:a", cur, 50, "usage", "h-old", time.Now().Add(-time.Minute).UTC())
	require.NoError(t, err)

	orphans, err := svc.FindOrphanedExpiredHolds(ctx)
	require.NoError(t, err)
	require.Len(t, orphans, 1)
	require.Equal(t, int64(50), orphans[0].Amount)

	rep, err := svc.Reconcile(ctx)
	require.NoError(t, err)
	require.Len(t, rep.OrphanedHolds, 1)
}

// Held-balance drift / anomaly reconciliation is gone (#491): balance + held are
// DERIVED from money_blocks + active holds/windows, so there is no cache to drift
// against. The only remaining check is orphaned holds (above).

func resetMoneyLedger(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	for _, table := range []string{
		"openrails.money_spend_limits",
		"openrails.money_settings",
		"openrails.money_blocks",
		"openrails.money_transactions",
	} {
		_, err := pool.Exec(ctx, "DELETE FROM "+table)
		require.NoError(t, err, "reset %s", table)
	}
}
