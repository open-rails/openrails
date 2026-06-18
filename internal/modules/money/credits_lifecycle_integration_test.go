//go:build integration

package money_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func TestCreditsDepositOverflowGuard(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	userID := uuid.NewString()
	payerID := identity.CustomerIDFromString(userID).UUID()
	t.Cleanup(func() {
		// money_blocks + money_transactions were dropped (#512); reset the live
		// per-customer settings instead.
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
	})

	moneySvc := money.NewMoneyService(dbi)

	// Seed a positive balance, then attempt a MaxInt64 deposit which would wrap
	// the balance negative without the overflow guard.
	_, err := moneySvc.Deposit(ctx, money.DepositParams{
		Invoker: userID, Currency: money.DefaultCurrency, Amount: 1, Source: "seed",
	})
	require.NoError(t, err)

	_, err = moneySvc.Deposit(ctx, money.DepositParams{
		Invoker: userID, Currency: money.DefaultCurrency, Amount: math.MaxInt64, Source: "overflow",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "overflow")

	// Balance must be unchanged (still 1) after the rejected deposit.
	bal, err := moneySvc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(1), bal.Balance)
}
