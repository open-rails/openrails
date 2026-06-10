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
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/open-rails/openrails/pkg/tenant"
)

func TestCreditExpiryWorker_HoldsDoNotReserveLots_CaptureSpillsToOwedAfterExpiry(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	creditTypeName := "test-credit-expiry-hold-" + uuid.New().String()[:8]
	creditType := suite.createTestCreditType(creditTypeName)

	userID := uuid.New().String()
	_ = suite.createTestCreditBalance(userID, creditType.ID, 100, 80)
	hold := suite.createTestCreditHold(userID, creditType.ID, 80, "active", time.Now().Add(1*time.Hour).UTC())

	expiredAt := time.Now().Add(-1 * time.Hour).UTC()
	batch := &models.CreditBlock{
		ID:              uuid.New(),
		TenantID:        tenant.DefaultID.UUID(),
		TenantSubjectID: suite.ensureTenantSubject(ctx, userID),
		CreditTypeID:    creditType.ID,
		OriginalAmount:  100,
		RemainingAmount: 100,
		ExpiresAt:       &expiredAt,
		CreatedAt:       expiredAt.Add(-1 * time.Minute),
	}
	_, err := suite.BunDB.NewInsert().Model(batch).Exec(ctx)
	require.NoError(t, err)

	fakeClock := clockwork.NewFakeClockAt(time.Now().UTC())
	worker := &riverjobs.CreditExpiryWorker{
		DB:    suite.App.Runtime.DB,
		Clock: fakeClock,
	}
	job := &river.Job[riverjobs.CreditExpiryArgs]{Args: riverjobs.CreditExpiryArgs{}}
	require.NoError(t, worker.Work(ctx, job))

	updatedBal, err := suite.App.Runtime.CreditsService.GetBalance(ctx, userID, creditTypeName)
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
	holdAfter := suite.getCreditHold(hold.ID)
	require.Equal(t, "captured", holdAfter.Status)

	balAfter, err := suite.App.Runtime.CreditsService.GetBalance(ctx, userID, creditTypeName)
	require.NoError(t, err)
	require.Equal(t, int64(0), balAfter.Balance)
	require.Equal(t, int64(0), balAfter.HeldBalance)

	owed, err := suite.App.Runtime.CreditsService.GetOutstandingOwed(ctx, identity.TenantSubjectID(personalOwnerID(userID)), creditTypeName)
	require.NoError(t, err)
	require.Equal(t, int64(50), owed)
}
