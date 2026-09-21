//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestReconcile_Clean(t *testing.T) {
	svc, payer, ctx := isolatedReconcileEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	rep, err := svc.Reconcile(ctx)
	require.NoError(t, err)
	require.Empty(t, rep.OrphanedHolds)
}

func TestReconcile_IgnoresLegacyPostgresHoldRows(t *testing.T) {
	svc, payer, ctx := isolatedReconcileEnv(t)
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

// Reconciliation owns a fresh merchant ledger; it never erases another test's
// immutable observations, authorizations, grants or transfers.
func isolatedReconcileEnv(t *testing.T) (*money.MoneyService, identity.CustomerID, context.Context) {
	t.Helper()
	mid := merchant.ID(uuid.New())
	database := dbtest.OpenMerchantDB(t, mid.UUID())
	ctx := merchant.WithID(t.Context(), mid)
	_, err := database.Qx(ctx).Exec(ctx, "INSERT INTO billing.merchants(id,slug) VALUES($1,$2)", mid.UUID(), "reconcile-"+mid.String())
	require.NoError(t, err)
	payer := identity.CustomerIDFromString(uuid.NewString())
	return money.NewMoneyService(database), payer, ctx
}
