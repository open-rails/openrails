//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func startOwnerTenantPostgres(t *testing.T) (*db.DB, string, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(ctx, t, dbi.Pool())
	return dbi, dsn, dbtest.WithTestMerchant(ctx)
}

// money_balances schema constraint tests removed (#491): the cache table is
// gone — balance + held are derived from money_blocks + active holds/windows, so
// there is no money_balances row to enforce NOT NULL / uniqueness on.

// seedSpendable deposits `amount` in the default currency for a user via the
// service, creating a spendable lot.
func seedSpendable(t *testing.T, ctx context.Context, svc *money.MoneyService, userID string, amount int64) {
	t.Helper()
	src := uuid.New().String()
	_, err := svc.Deposit(ctx, money.DepositParams{
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   amount,
		Source:   "test_seed",
		SourceID: &src,
	})
	require.NoError(t, err)
}

func TestPostedSpend_ConservesTotal(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	svc := money.NewMoneyService(dbi)

	userID := uuid.NewString()
	const initial = int64(1000)
	seedSpendable(t, ctx, svc, userID, initial)

	bal0, err := svc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, initial, bal0.Balance)
	require.Equal(t, int64(0), bal0.HeldBalance)

	payer := identity.CustomerIDFromString(userID)
	err = svc.SpendCredits(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   150,
		Source:   "api",
		SourceID: "req-spend-1",
	})
	require.NoError(t, err)

	balFinal, err := svc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, initial-150, balFinal.Balance, "only the posted spend leaves the balance")
	require.Equal(t, int64(0), balFinal.HeldBalance, "request holds are Redis state, not durable money held")
	require.Equal(t, initial-150, balFinal.Balance-balFinal.HeldBalance, "available == initial - captured")
}

func TestSpendIdempotency_RestoresSameBalanceOnReplay(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	svc := money.NewMoneyService(dbi)

	userID := uuid.NewString()
	const initial = int64(500)
	seedSpendable(t, ctx, svc, userID, initial)

	payer := identity.CustomerIDFromString(userID)
	err := svc.SpendCredits(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   300,
		Source:   "api",
		SourceID: "req-replay-1",
	})
	require.NoError(t, err)
	err = svc.SpendCredits(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   300,
		Source:   "api",
		SourceID: "req-replay-1",
	})
	require.NoError(t, err)

	bal, err := svc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, initial-300, bal.Balance, "replaying the same source/source_id must not spend twice")
	require.Equal(t, int64(0), bal.HeldBalance)
}
