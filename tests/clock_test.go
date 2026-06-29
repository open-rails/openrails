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
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

func TestRuntimeClockInjectedBeforeConstruction(t *testing.T) {
	fixedTime := time.Date(2024, 8, 1, 10, 0, 0, 0, time.UTC)
	mockClock := clockwork.NewFakeClockAt(fixedTime)
	suite := setupTestSuite(t, WithSuiteClock(mockClock))
	ctx := dbtest.WithTestMerchant(context.Background())

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
	require.Equal(t, mockClock, rt.MoneyService.Clock())
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
	retryCustomerID := suite.ensureCustomer(ctx, retryUserID)
	pm := suite.CreateTestPaymentMethod(retryUserID)
	nextRetry := mockClock.Now().Add(time.Hour)
	retryAttempts := 1
	railSubID := "clock-dunning-" + uuid.New().String()[:8]
	suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          retryUserID,
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Rail:            models.RailNMI,
		RailSubID:       railSubID,
		PeriodStart:     mockClock.Now().Add(-30 * 24 * time.Hour),
		PeriodEnd:       mockClock.Now().Add(-time.Hour),
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &nextRetry,
		PaymentMethodID: &pm.ID,
	})
	countDueRetries := func() int {
		return suite.Count(ctx, `
			SELECT COUNT(*) FROM openrails.subscriptions sub
			WHERE sub.rail = $1
			  AND sub.customer_id = $2
			  AND sub.status = $3
			  AND sub.next_retry_at IS NOT NULL AND sub.next_retry_at <= $4`,
			string(models.RailNMI), retryCustomerID,
			string(models.StatusPastDue), mockClock.Now())
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
		Rail:        models.RailNMI,
		PeriodStart: cancelStart,
		PeriodEnd:   cancelPeriodEnd,
	})
	ent := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, cancelUserID),
		Entitlement: "premium",
		StartAt:     cancelStart,
		EndAt:       &cancelPeriodEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &cancelSub.ID,
		CreatedAt:   cancelStart,
		UpdatedAt:   cancelStart,
	}
	suite.InsertEntitlement(ctx, ent)

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

	// Balance derives from #514 credit lots (grants) → the #512 ledger. Seed a 25
	// non-expiring lot (the surviving balance) + a 75 lot that expires in 1h.
	// (Request holds are Redis-only now (#505), so there is no durable money hold
	// to expire here — only the credit-lot lapse is swept in Postgres.)
	creditUserID := uuid.New().String()
	merchantID := dbtest.TestMerchantID.UUID()
	customerID := suite.ensureCustomer(ctx, creditUserID)
	_ = suite.insertMoneyCreditLot(ctx, merchantID, customerID, "USD", 25, nil, mockClock.Now()) // non-expiring
	blockExpiry := mockClock.Now().Add(time.Hour)
	lotID := suite.insertMoneyCreditLot(ctx, merchantID, customerID, "USD", 75, &blockExpiry, mockClock.Now())

	creditWorker := &riverjobs.CreditExpiryWorker{DB: rt.DB, Clock: rt.Clock}

	require.NoError(t, creditWorker.Work(ctx, &river.Job[riverjobs.CreditExpiryArgs]{Args: riverjobs.CreditExpiryArgs{}}))
	assert.Equal(t, int64(75), suite.lotRemaining(ctx, merchantID, lotID), "lot intact before expiry")

	mockClock.Advance(2 * time.Hour)
	require.NoError(t, creditWorker.Work(ctx, &river.Job[riverjobs.CreditExpiryArgs]{Args: riverjobs.CreditExpiryArgs{}}))
	assert.Equal(t, int64(0), suite.lotRemaining(ctx, merchantID, lotID), "lapsed lot remainder clawed to expired_credits")
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
			Rail:        "nmi",
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
	ctx := dbtest.WithTestMerchant(context.Background())

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
			UserID:  userID,
			PriceID: priceID,
			Rail:    models.RailNMI,
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
		railSubID := "test-cancel-" + uuid.New().String()

		// Create subscription
		sub, err := suite.App.Runtime.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
			UserID:             userID,
			PriceID:            priceID,
			Rail:               models.RailNMI,
			RailSubscriptionID: &railSubID,
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

		// Verify cancellation timestamp uses the mock clock.
		// WithinDuration instead of Equal: the DB round-trip changes the
		// time.Time internal representation (location pointer), which fails
		// require.Equal/reflect.DeepEqual even when the instants are identical.
		require.NotNil(t, updatedSub.CancelledAt)
		assert.WithinDuration(t, cancelTime, *updatedSub.CancelledAt, 0,
			"CancelMembership should use mock clock for CancelledAt timestamp")
	})
}
