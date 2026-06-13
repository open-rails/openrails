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
	_, err := svc.Deposit(ctx, money.DepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.Hold(ctx, &payer, "user:a", cur, 200, "usage", "h1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	rep, err := svc.Reconcile(ctx)
	require.NoError(t, err)
	require.Empty(t, rep.OrphanedHolds)
	require.Empty(t, rep.HeldBalanceDrift)
	require.Empty(t, rep.BalanceAnomalies)
}

func TestReconcile_OrphanedExpiredHold(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	resetMoneyLedger(t, pool, ctx)
	_, err := svc.Deposit(ctx, money.DepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 1000, Source: "seed"})
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

func TestReconcile_HeldBalanceDriftAndRepair(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	resetMoneyLedger(t, pool, ctx)
	_, err := svc.Deposit(ctx, money.DepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.Hold(ctx, &payer, "user:a", cur, 200, "usage", "h1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	// Corrupt the stored held_balance.
	_, err = pool.Exec(ctx,
		"UPDATE openrails.money_balances SET held_balance = $1 WHERE tenant_subject_id = $2",
		999, payer.UUID())
	require.NoError(t, err)

	drift, err := svc.FindHeldBalanceDrift(ctx)
	require.NoError(t, err)
	require.Len(t, drift, 1)
	require.Equal(t, int64(999), drift[0].Stored)
	require.Equal(t, int64(200), drift[0].Computed)

	corrected, err := svc.RepairHeldBalance(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(200), corrected)

	drift2, err := svc.FindHeldBalanceDrift(ctx)
	require.NoError(t, err)
	require.Empty(t, drift2, "repair clears the drift")
}

func TestReconcile_BalanceAnomaly(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	resetMoneyLedger(t, pool, ctx)
	_, err := svc.Deposit(ctx, money.DepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 100, Source: "seed"})
	require.NoError(t, err)
	// held > balance.
	_, err = pool.Exec(ctx,
		"UPDATE openrails.money_balances SET held_balance = $1 WHERE tenant_subject_id = $2",
		500, payer.UUID())
	require.NoError(t, err)

	anomalies, err := svc.FindBalanceAnomalies(ctx)
	require.NoError(t, err)
	require.Len(t, anomalies, 1)
	require.Equal(t, "held_exceeds_balance", anomalies[0].Reason)
}

func resetMoneyLedger(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	for _, table := range []string{
		"openrails.money_spend_limits",
		"openrails.money_accounts",
		"openrails.money_blocks",
		"openrails.money_transactions",
		"openrails.money_balances",
	} {
		_, err := pool.Exec(ctx, "DELETE FROM "+table)
		require.NoError(t, err, "reset %s", table)
	}
}
