//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TestClockInTestSuite tests the clock integration with TestContainerSuite
func TestClockInTestSuite(t *testing.T) {
	suite := setupTestSuite(t, WithSuiteClock(clockwork.NewRealClock()))

	t.Run("default clock is real clock", func(t *testing.T) {
		clk := suite.GetClock()
		require.NotNil(t, clk)

		// Default should be real clock, so Now() should be close to time.Now()
		now := clk.Now()
		assert.WithinDuration(t, time.Now(), now, time.Second)
	})

	t.Run("can set mock clock via suite", func(t *testing.T) {
		fixedTime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
		mockClock := suite.SetMockClock(fixedTime)

		// Runtime should now use the mock clock
		assert.Equal(t, fixedTime, suite.App.Runtime.Clock.Now())

		// Advance time
		mockClock.Advance(30 * 24 * time.Hour) // 30 days
		expected := fixedTime.Add(30 * 24 * time.Hour)
		assert.Equal(t, expected, suite.App.Runtime.Clock.Now())
	})
}

func TestRuntimeClockInjectedBeforeConstruction(t *testing.T) {
	fixedTime := time.Date(2024, 8, 1, 10, 0, 0, 0, time.UTC)
	mockClock := clockwork.NewFakeClockAt(fixedTime)
	suite := setupTestSuite(t, WithSuiteClock(mockClock))
	ctx := context.Background()

	rt := suite.App.Runtime
	require.Equal(t, mockClock, rt.Clock)
	require.Equal(t, mockClock, rt.SubscriptionLifecycleService.Clock())
	require.Equal(t, mockClock, rt.SubscriptionService.Clock())
	require.Equal(t, mockClock, rt.EntitlementService.Clock())
	require.Equal(t, mockClock, rt.PaymentService.Clock())
	require.Equal(t, mockClock, rt.VaultService.Clock())
	require.Equal(t, mockClock, rt.WebhookDispatcher.Clock)
	require.Equal(t, mockClock, rt.CheckoutService.Clock())
	require.Equal(t, mockClock, rt.CheckoutSessionService.Clock())
	require.Equal(t, mockClock, rt.CreditsService.Clock())
	require.Equal(t, mockClock, rt.SolanaPayService.Clock())
	require.Equal(t, mockClock, rt.SolanaTransactionService.Clock())
	if rt.EventLogService != nil {
		require.Equal(t, mockClock, rt.EventLogService.Clock())
	}

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	userID := uuid.New().String()
	periodEnd := fixedTime.Add(24 * time.Hour)
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   priceID,
		Status:    models.StatusActive,
		PeriodEnd: periodEnd,
	})

	assert.Equal(t, fixedTime, sub.CreatedAt)
	assert.Equal(t, fixedTime, sub.UpdatedAt)
	assert.Equal(t, fixedTime, sub.StartedAt)

	activeSub, err := rt.SubscriptionService.GetActiveSubscription(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, sub.ID, activeSub.ID)

	mockClock.Advance(25 * time.Hour)
	_, err = rt.SubscriptionService.GetActiveSubscription(ctx, userID)
	require.Error(t, err, "subscription should not be active after fake time passes the period end")

	cancelledAt := mockClock.Now()
	cancelType := models.CancelTypeUser
	sub.Status = models.StatusCancelled
	sub.CancelledAt = &cancelledAt
	sub.CancelType = &cancelType
	require.NoError(t, rt.SubscriptionService.Update(ctx, sub))
	updated := suite.GetSubscription(sub.ID)
	assert.WithinDuration(t, mockClock.Now(), updated.UpdatedAt, time.Millisecond)

	retryUserID := uuid.New().String()
	retryTenantSubjectID := suite.ensureTenantSubject(ctx, retryUserID)
	pm := suite.CreateTestPaymentMethod(retryUserID)
	nextRetry := mockClock.Now().Add(time.Hour)
	retryAttempts := 1
	processorSubID := "clock-dunning-" + uuid.New().String()[:8]
	suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          retryUserID,
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Processor:       models.ProcessorMobius,
		ProcessorSubID:  processorSubID,
		PeriodStart:     mockClock.Now().Add(-30 * 24 * time.Hour),
		PeriodEnd:       mockClock.Now().Add(-time.Hour),
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &nextRetry,
		PaymentMethodID: &pm.ID,
	})
	countDueRetries := func() int {
		var count int
		err := suite.BunDB.NewSelect().
			Model((*models.Subscription)(nil)).
			Where("sub.processor = ?", models.ProcessorMobius).
			Where("sub.tenant_subject_id = ?", retryTenantSubjectID).
			Where("sub.status = ?", models.StatusPastDue).
			Where("sub.next_retry_at IS NOT NULL AND sub.next_retry_at <= ?", mockClock.Now()).
			ColumnExpr("COUNT(*)").
			Scan(ctx, &count)
		require.NoError(t, err)
		return count
	}
	assert.Equal(t, 0, countDueRetries())
	mockClock.Advance(time.Hour)
	assert.Equal(t, 1, countDueRetries())

	cancelUserID := uuid.New().String()
	cancelStart := mockClock.Now()
	cancelPeriodEnd := cancelStart.Add(30 * 24 * time.Hour)
	cancelSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:      cancelUserID,
		PriceID:     priceID,
		Status:      models.StatusActive,
		Processor:   models.ProcessorMobius,
		PeriodStart: cancelStart,
		PeriodEnd:   cancelPeriodEnd,
	})
	ent := &models.Entitlement{
		ID:              uuid.New(),
		TenantSubjectID: suite.ensureTenantSubject(ctx, cancelUserID),
		Entitlement:     "premium",
		StartAt:         cancelStart,
		EndAt:           &cancelPeriodEnd,
		SourceType:      models.EntitlementSourceSubscription,
		SourceID:        &cancelSub.ID,
		CreatedAt:       cancelStart,
		UpdatedAt:       cancelStart,
	}
	_, err = suite.BunDB.NewInsert().Model(ent).Exec(ctx)
	require.NoError(t, err)

	mockClock.Advance(5 * 24 * time.Hour)
	require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
		SubscriptionID: &cancelSub.ID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   false,
	}))
	isEntitled, err := rt.EntitlementService.IsEntitled(ctx, cancelUserID, "premium", mockClock.Now())
	require.NoError(t, err)
	assert.True(t, isEntitled)

	mockClock.Advance(cancelPeriodEnd.Add(-time.Hour).Sub(mockClock.Now()))
	isEntitled, err = rt.EntitlementService.IsEntitled(ctx, cancelUserID, "premium", mockClock.Now())
	require.NoError(t, err)
	assert.True(t, isEntitled)

	mockClock.Advance(2 * time.Hour)
	isEntitled, err = rt.EntitlementService.IsEntitled(ctx, cancelUserID, "premium", mockClock.Now())
	require.NoError(t, err)
	assert.False(t, isEntitled)

	creditType := suite.createTestCreditType("clock-expiry-" + uuid.New().String()[:8])
	creditUserID := uuid.New().String()
	balance := suite.createTestCreditBalance(creditUserID, creditType.ID, 100, 25)
	holdExpiry := mockClock.Now().Add(time.Hour)
	hold := suite.createTestCreditHold(creditUserID, creditType.ID, 25, "active", holdExpiry)
	blockExpiry := mockClock.Now().Add(time.Hour)
	block := &models.CreditBlock{
		ID:              uuid.New(),
		TenantID:        tenant.DefaultID.UUID(),
		TenantSubjectID: suite.ensureTenantSubject(ctx, creditUserID),
		InvokerID:       creditUserID,
		CreditTypeID:    creditType.ID,
		OriginalAmount:  75,
		RemainingAmount: 75,
		ExpiresAt:       &blockExpiry,
		CreatedAt:       mockClock.Now(),
	}
	_, err = suite.BunDB.NewInsert().Model(block).Exec(ctx)
	require.NoError(t, err)

	holdWorker := &riverjobs.HoldExpiryWorker{DB: rt.DB, Config: rt.Config, Clock: rt.Clock}
	creditWorker := &riverjobs.CreditExpiryWorker{DB: rt.DB, Config: rt.Config, Clock: rt.Clock}

	require.NoError(t, holdWorker.Work(ctx, &river.Job[riverjobs.HoldExpiryArgs]{Args: riverjobs.HoldExpiryArgs{}}))
	require.NoError(t, creditWorker.Work(ctx, &river.Job[riverjobs.CreditExpiryArgs]{Args: riverjobs.CreditExpiryArgs{}}))
	assert.Equal(t, "active", suite.getCreditHold(hold.ID).Status)
	var remaining int64
	require.NoError(t, suite.BunDB.NewSelect().Model((*models.CreditBlock)(nil)).
		ColumnExpr("remaining_amount").
		Where("id = ?", block.ID).
		Scan(ctx, &remaining))
	assert.Equal(t, int64(75), remaining)

	mockClock.Advance(2 * time.Hour)
	require.NoError(t, holdWorker.Work(ctx, &river.Job[riverjobs.HoldExpiryArgs]{Args: riverjobs.HoldExpiryArgs{}}))
	require.NoError(t, creditWorker.Work(ctx, &river.Job[riverjobs.CreditExpiryArgs]{Args: riverjobs.CreditExpiryArgs{}}))
	assert.Equal(t, "expired", suite.getCreditHold(hold.ID).Status)
	updatedBalance := suite.getCreditBalance(balance.ID)
	assert.Equal(t, int64(0), updatedBalance.HeldBalance)
	assert.Equal(t, int64(25), updatedBalance.Balance)
	require.NoError(t, suite.BunDB.NewSelect().Model((*models.CreditBlock)(nil)).
		ColumnExpr("remaining_amount").
		Where("id = ?", block.ID).
		Scan(ctx, &remaining))
	assert.Equal(t, int64(0), remaining)
}

// TestSubscriptionExpiryWithMockClock demonstrates testing subscription expiry logic
func TestSubscriptionExpiryWithMockClock(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	t.Run("subscription period tracking with mock clock", func(t *testing.T) {
		// Set clock to a known date
		startDate := time.Date(2024, time.November, 1, 12, 0, 0, 0, time.UTC)
		mockClock := suite.SetMockClock(startDate)

		// Create a subscription that ends on Nov 30
		endDate := time.Date(2024, time.November, 30, 12, 0, 0, 0, time.UTC)
		userID := uuid.New().String()

		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:      userID,
			PriceID:     priceID,
			Status:      "active",
			Processor:   "mobius",
			PeriodStart: startDate,
			PeriodEnd:   endDate,
		})

		// Verify subscription is created with correct dates
		assert.Equal(t, startDate.Truncate(time.Second), sub.CurrentPeriodStartsAt.Truncate(time.Second))
		assert.Equal(t, endDate.Truncate(time.Second), sub.CurrentPeriodEndsAt.Truncate(time.Second))

		// At Nov 1, subscription is not expired
		assert.True(t, sub.CurrentPeriodEndsAt.After(mockClock.Now()))

		// Advance to Nov 29 - still active (28 days from Nov 1)
		mockClock.Advance(28 * 24 * time.Hour)
		assert.True(t, sub.CurrentPeriodEndsAt.After(mockClock.Now()))

		// Advance to Dec 1 - now expired (2 more days = 30 days total from Nov 1)
		mockClock.Advance(2 * 24 * time.Hour)
		assert.True(t, sub.CurrentPeriodEndsAt.Before(mockClock.Now()))
	})
}

// TestLifecycleServiceUsesMockClock verifies that SubscriptionLifecycleService uses the mock clock
func TestLifecycleServiceUsesMockClock(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	t.Run("CreateMembership uses mock clock for period dates", func(t *testing.T) {
		// Set clock to a specific date
		mockedTime := time.Date(2024, time.March, 15, 10, 0, 0, 0, time.UTC)
		suite.SetMockClock(mockedTime)

		userID := uuid.New().String()

		// Create membership through the lifecycle service
		sub, err := suite.App.Runtime.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
			UserID:    userID,
			PriceID:   priceID,
			Processor: models.ProcessorMobius,
		})
		require.NoError(t, err)
		require.NotNil(t, sub)

		// Verify the subscription period starts at the mocked time
		require.NotNil(t, sub.CurrentPeriodStartsAt)
		assert.Equal(t, mockedTime.Truncate(time.Second), sub.CurrentPeriodStartsAt.Truncate(time.Second),
			"CreateMembership should use the mock clock for period start")

		// Period end should be 30 days from the mocked start (default billing cycle)
		expectedEnd := mockedTime.Add(30 * 24 * time.Hour)
		require.NotNil(t, sub.CurrentPeriodEndsAt)
		assert.Equal(t, expectedEnd.Truncate(time.Second), sub.CurrentPeriodEndsAt.Truncate(time.Second),
			"CreateMembership should calculate period end from mock clock")
	})

	t.Run("CancelMembership uses mock clock for cancellation timestamp", func(t *testing.T) {
		// Set initial clock
		initialTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
		mockClock := suite.SetMockClock(initialTime)

		userID := uuid.New().String()
		processorSubID := "test-cancel-" + uuid.New().String()

		// Create subscription
		sub, err := suite.App.Runtime.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
			UserID:                  userID,
			PriceID:                 priceID,
			Processor:               models.ProcessorMobius,
			ProcessorSubscriptionID: &processorSubID,
		})
		require.NoError(t, err)
		require.NotNil(t, sub)

		// Advance clock to 15 days later
		mockClock.Advance(15 * 24 * time.Hour)
		cancelTime := initialTime.Add(15 * 24 * time.Hour)

		// Cancel the subscription
		err = suite.App.Runtime.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: &sub.ID,
			CancelType:     models.CancelTypeUser,
			RevokeAccess:   true,
		})
		require.NoError(t, err)

		// Fetch updated subscription
		updatedSub, err := suite.App.Runtime.SubscriptionService.GetByID(ctx, sub.ID)
		require.NoError(t, err)

		// Verify cancellation timestamp uses the mock clock
		require.NotNil(t, updatedSub.CancelledAt)
		assert.Equal(t, cancelTime.Truncate(time.Second), updatedSub.CancelledAt.Truncate(time.Second),
			"CancelMembership should use mock clock for CancelledAt timestamp")
	})
}
