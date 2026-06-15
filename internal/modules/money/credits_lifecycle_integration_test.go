//go:build integration

package money_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestCreditsDepositOverflowGuard(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	userID := uuid.NewString()
	payerID := identity.CustomerIDFromString(userID).UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})

	moneySvc := money.NewMoneyService(dbi)

	// Seed a positive balance, then attempt a MaxInt64 deposit which would wrap
	// the balance negative without the overflow guard.
	_, err := moneySvc.Deposit(ctx, money.DepositParams{
		Invoker: userID, Amount: 1, Source: "seed",
	})
	require.NoError(t, err)

	_, err = moneySvc.Deposit(ctx, money.DepositParams{
		Invoker: userID, Amount: math.MaxInt64, Source: "overflow",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "overflow")

	// Balance must be unchanged (still 1) after the rejected deposit.
	bal, err := moneySvc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(1), bal.Balance)
}

func TestCreditsLifecycle_HoldIdempotentAndCaptureReleaseExpire(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.NewString()
	payerID := identity.CustomerIDFromString(userID).UUID()

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})

	moneySvc := money.NewMoneyService(dbi)
	cur := money.DefaultCurrency

	// Seed spendable money (creates a money block).
	_, err := moneySvc.Deposit(ctx, money.DepositParams{
		Invoker: userID,
		Amount:  1000,
		Source:  "test_deposit",
	})
	require.NoError(t, err)

	// 1) Hold idempotency.
	expiresAt := now.Add(10 * time.Minute).UTC()
	hold1, err := moneySvc.Hold(ctx, nil, userID, cur, 200, "api_call", "req_1", expiresAt)
	require.NoError(t, err)
	hold2, err := moneySvc.Hold(ctx, nil, userID, cur, 200, "api_call", "req_1", expiresAt)
	require.NoError(t, err)
	require.Equal(t, hold1.ID, hold2.ID)

	bal, err := moneySvc.GetBalance(ctx, userID, cur)
	require.NoError(t, err)
	require.Equal(t, int64(1000), bal.Balance)
	require.Equal(t, int64(200), bal.HeldBalance)

	// 2) Release hold.
	_, relErr := moneySvc.ReleaseHold(ctx, hold1.ID)
	require.NoError(t, relErr)
	bal, err = moneySvc.GetBalance(ctx, userID, cur)
	require.NoError(t, err)
	require.Equal(t, int64(1000), bal.Balance)
	require.Equal(t, int64(0), bal.HeldBalance)

	holdAfterRelease := new(models.MoneyTransaction)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT transaction_type, status FROM openrails.money_transactions WHERE id = $1", hold1.ID).
		Scan(&holdAfterRelease.TransactionType, &holdAfterRelease.Status))
	require.Equal(t, "hold", holdAfterRelease.TransactionType)
	require.Equal(t, "released", holdAfterRelease.Status)

	// 3) Hold -> partial capture.
	hold3, err := moneySvc.Hold(ctx, nil, userID, cur, 300, "api_call", "req_2", now.Add(10*time.Minute).UTC())
	require.NoError(t, err)
	trx, err := moneySvc.CaptureHold(ctx, hold3.ID, 120)
	require.NoError(t, err)
	require.Equal(t, "hold", trx.TransactionType)
	require.Equal(t, "captured", trx.Status)
	require.NotNil(t, trx.Authorized)
	require.Equal(t, int64(300), *trx.Authorized)
	require.NotNil(t, trx.Captured)
	require.Equal(t, int64(120), *trx.Captured)
	require.Equal(t, int64(-120), trx.Amount)
	require.NotNil(t, trx.BalanceAfter)
	require.Equal(t, int64(880), *trx.BalanceAfter)

	bal, err = moneySvc.GetBalance(ctx, userID, cur)
	require.NoError(t, err)
	require.Equal(t, int64(880), bal.Balance)
	require.Equal(t, int64(0), bal.HeldBalance)
}

// TestCreditsLifecycle_HoldExpiryReleasesHeld covers the hold-expiry sweep
// releasing an expired hold's held. Under the derived model (#491) the worker
// just flips the hold to 'expired'; held is re-derived from active holds, so the
// old currency-keyed-cache bug is gone and this exercises the real release.
func TestCreditsLifecycle_HoldExpiryReleasesHeld(t *testing.T) {

	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.NewString()
	cur := money.DefaultCurrency
	moneySvc := money.NewMoneyService(dbi)

	_, err := moneySvc.Deposit(ctx, money.DepositParams{Invoker: userID, Amount: 880, Source: "seed"})
	require.NoError(t, err)

	hold, err := moneySvc.Hold(ctx, nil, userID, cur, 50, "api_call", "req_exp", now.Add(-1*time.Minute).UTC())
	require.NoError(t, err)
	bal, err := moneySvc.GetBalance(ctx, userID, cur)
	require.NoError(t, err)
	require.Equal(t, int64(50), bal.HeldBalance)

	w := &riverjobs.HoldExpiryWorker{DB: dbi, Clock: nil}
	job := &river.Job[riverjobs.HoldExpiryArgs]{Args: riverjobs.HoldExpiryArgs{}}
	require.NoError(t, w.Work(ctx, job))

	holdAfterExpire := new(models.MoneyTransaction)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT status FROM openrails.money_transactions WHERE id = $1", hold.ID).
		Scan(&holdAfterExpire.Status))
	require.Equal(t, "expired", holdAfterExpire.Status)

	bal, err = moneySvc.GetBalance(ctx, userID, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), bal.HeldBalance)
	require.Equal(t, int64(880), bal.Balance)
}
