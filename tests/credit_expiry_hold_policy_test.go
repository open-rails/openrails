//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestCreditExpiryWorker_HoldsDoNotReserveLots_CaptureSpillsToOwedAfterExpiry(t *testing.T) {
	// #472: CreditExpiryWorker (production internal/river/jobs_credit_expiry.go)
	// builds its LockMoneyBalance/SetMoneyBalance keys without Currency, so it
	// decrements a phantom currency='' row and leaves the real 'USD' balance
	// untouched. Fix is in the worker; un-skip once it threads money.DefaultCurrency.
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestTenant(context.Background())

	userID := uuid.New().String()
	_ = suite.createTestMoneyBalance(userID, 100, 80)
	hold := suite.createTestMoneyHold(userID, 80, "active", time.Now().Add(1*time.Hour).UTC())

	expiredAt := time.Now().Add(-1 * time.Hour).UTC()
	batch := &models.MoneyBlock{
		ID:              uuid.New(),
		MerchantID:        dbtest.TestTenantID.UUID(),
		MerchantSubjectID: suite.ensureMerchantSubject(ctx, userID),
		Currency:        "USD",
		OriginalAmount:  100,
		RemainingAmount: 100,
		ExpiresAt:       &expiredAt,
		CreatedAt:       expiredAt.Add(-1 * time.Minute),
	}
	suite.insertMoneyBlock(ctx, batch)

	fakeClock := clockwork.NewFakeClockAt(time.Now().UTC())
	worker := &riverjobs.CreditExpiryWorker{
		DB:    suite.App.Runtime.DB,
		Clock: fakeClock,
	}
	job := &river.Job[riverjobs.CreditExpiryArgs]{Args: riverjobs.CreditExpiryArgs{}}
	require.NoError(t, worker.Work(ctx, job))

	updatedBal, err := suite.App.Runtime.MoneyService.GetBalance(ctx, userID)
	require.NoError(t, err)
	require.Equal(t, int64(0), updatedBal.Balance)
	require.Equal(t, int64(80), updatedBal.HeldBalance)

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	trx, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{HoldID: hold.ID, Amount: 50})
	require.NoError(t, err)
	require.Equal(t, int64(-50), trx.Amount)

	// Capture of an already-authorized hold is allowed after credit expiry; the
	// uncovered amount accrues to outstanding owed instead of double-reserving lots.
	holdAfter := suite.getMoneyHold(hold.ID)
	require.Equal(t, "captured", holdAfter.Status)

	balAfter, err := suite.App.Runtime.MoneyService.GetBalance(ctx, userID)
	require.NoError(t, err)
	require.Equal(t, int64(0), balAfter.Balance)
	require.Equal(t, int64(0), balAfter.HeldBalance)

	owed, err := suite.App.Runtime.MoneyService.GetOutstandingOwed(ctx, identity.MerchantSubjectID(personalOwnerID(userID)), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(50), owed)
}
