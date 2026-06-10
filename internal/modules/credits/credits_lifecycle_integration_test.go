//go:build integration

package credits_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestCreditsLifecycle_HoldIdempotentAndCaptureReleaseExpire(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	// Require the lifecycle migration (authorized_amount) + credit_blocks to exist.
	var hasLifecycle bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='billing' AND table_name='credit_transactions' AND column_name='authorized_amount')").
		Scan(&hasLifecycle))
	if !hasLifecycle {
		t.Skip("billing.credit_transactions missing authorized_amount; run migrations before integration tests")
	}
	var hasBlocks bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_blocks')").
		Scan(&hasBlocks))
	if !hasBlocks {
		t.Skip("billing.credit_blocks not found; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)
	creditTypeName := "test_credits_lifecycle_" + uuid.NewString()
	creditTypeID := uuid.New()
	userID := uuid.NewString()

	_, err := gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            creditTypeID,
		Name:          creditTypeName,
		DisplayName:   "Test Credits Lifecycle",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_blocks WHERE credit_type_id = $1", creditTypeID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_transactions WHERE credit_type_id = $1", creditTypeID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_balances WHERE credit_type_id = $1", creditTypeID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_types WHERE id = $1", creditTypeID)
	})

	creditsSvc := credits.NewCreditsService(dbi)

	// Seed spendable credits (creates a credit block).
	_, err = creditsSvc.Deposit(ctx, credits.CreditDepositParams{
		Actor:      userID,
		CreditType: creditTypeName,
		Amount:     1000,
		Source:     "test_deposit",
	})
	require.NoError(t, err)

	// 1) Hold idempotency.
	expiresAt := now.Add(10 * time.Minute).UTC()
	hold1, err := creditsSvc.Hold(ctx, nil, userID, creditTypeName, 200, "api_call", "req_1", expiresAt)
	require.NoError(t, err)
	hold2, err := creditsSvc.Hold(ctx, nil, userID, creditTypeName, 200, "api_call", "req_1", expiresAt)
	require.NoError(t, err)
	require.Equal(t, hold1.ID, hold2.ID)

	bal, err := creditsSvc.GetBalance(ctx, userID, creditTypeName)
	require.NoError(t, err)
	require.Equal(t, int64(1000), bal.Balance)
	require.Equal(t, int64(200), bal.HeldBalance)

	// 2) Release hold.
	require.NoError(t, creditsSvc.ReleaseHold(ctx, hold1.ID))
	bal, err = creditsSvc.GetBalance(ctx, userID, creditTypeName)
	require.NoError(t, err)
	require.Equal(t, int64(1000), bal.Balance)
	require.Equal(t, int64(0), bal.HeldBalance)

	holdAfterRelease := new(models.CreditTransaction)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT transaction_type, status FROM billing.credit_transactions WHERE id = $1", hold1.ID).
		Scan(&holdAfterRelease.TransactionType, &holdAfterRelease.Status))
	require.Equal(t, "hold", holdAfterRelease.TransactionType)
	require.Equal(t, "released", holdAfterRelease.Status)

	// 3) Hold -> partial capture.
	hold3, err := creditsSvc.Hold(ctx, nil, userID, creditTypeName, 300, "api_call", "req_2", now.Add(10*time.Minute).UTC())
	require.NoError(t, err)
	trx, err := creditsSvc.CaptureHold(ctx, hold3.ID, 120)
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

	bal, err = creditsSvc.GetBalance(ctx, userID, creditTypeName)
	require.NoError(t, err)
	require.Equal(t, int64(880), bal.Balance)
	require.Equal(t, int64(0), bal.HeldBalance)

	// 4) Hold -> expire (via worker).
	hold4, err := creditsSvc.Hold(ctx, nil, userID, creditTypeName, 50, "api_call", "req_3", now.Add(-1*time.Minute).UTC())
	require.NoError(t, err)
	bal, err = creditsSvc.GetBalance(ctx, userID, creditTypeName)
	require.NoError(t, err)
	require.Equal(t, int64(50), bal.HeldBalance)

	w := &riverjobs.HoldExpiryWorker{
		DB:    dbi,
		Clock: nil, // uses real clock; the hold is already expired
	}
	job := &river.Job[riverjobs.HoldExpiryArgs]{Args: riverjobs.HoldExpiryArgs{}}
	require.NoError(t, w.Work(ctx, job))

	holdAfterExpire := new(models.CreditTransaction)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT status FROM billing.credit_transactions WHERE id = $1", hold4.ID).
		Scan(&holdAfterExpire.Status))
	require.Equal(t, "expired", holdAfterExpire.Status)

	bal, err = creditsSvc.GetBalance(ctx, userID, creditTypeName)
	require.NoError(t, err)
	require.Equal(t, int64(0), bal.HeldBalance)
	require.Equal(t, int64(880), bal.Balance)
}
