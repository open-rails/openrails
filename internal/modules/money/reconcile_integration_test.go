//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestReconcile_Clean(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	resetMoneyLedger(t, pool, ctx)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	rep, err := svc.Reconcile(ctx)
	require.NoError(t, err)
	require.Empty(t, rep.OrphanedHolds)
}

func TestReconcile_IgnoresLegacyPostgresHoldRows(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	resetMoneyLedger(t, pool, ctx)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	orphans, err := svc.FindOrphanedExpiredHolds(ctx)
	require.NoError(t, err)
	require.Empty(t, orphans)

	rep, err := svc.Reconcile(ctx)
	require.NoError(t, err)
	require.Empty(t, rep.OrphanedHolds)
}

// Held-balance drift / anomaly reconciliation is gone (#491): balance + held are
// DERIVED from money_blocks + durable windows, while request holds are Redis TTL
// state (#505), so there is no cache or Postgres request-hold state to reconcile.

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
