//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/services"
)

// TestClockMockBasics tests the basic functionality of the mock clock
func TestClockMockBasics(t *testing.T) {
	t.Run("real clock returns current time", func(t *testing.T) {
		realClock := clockwork.NewRealClock()
		now := realClock.Now()

		// Should be within 1 second of actual time
		assert.WithinDuration(t, time.Now(), now, time.Second)
	})

	t.Run("mock clock returns fixed time", func(t *testing.T) {
		fixedTime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
		mockClock := clockwork.NewFakeClockAt(fixedTime)

		assert.Equal(t, fixedTime, mockClock.Now())
	})

	t.Run("mock clock can be advanced", func(t *testing.T) {
		startTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		mockClock := clockwork.NewFakeClockAt(startTime)

		// Advance by 1 hour
		mockClock.Advance(time.Hour)
		expected := startTime.Add(time.Hour)
		assert.Equal(t, expected, mockClock.Now())

		// Advance by 1 day
		mockClock.Advance(24 * time.Hour)
		expected = expected.Add(24 * time.Hour)
		assert.Equal(t, expected, mockClock.Now())
	})

	t.Run("mock clock Since calculation", func(t *testing.T) {
		mockClock := clockwork.NewFakeClockAt(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
		pastTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

		since := mockClock.Since(pastTime)
		assert.Equal(t, 24*time.Hour, since)
	})

	t.Run("mock clock Until calculation", func(t *testing.T) {
		mockClock := clockwork.NewFakeClockAt(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
		futureTime := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

		until := mockClock.Until(futureTime)
		assert.Equal(t, 24*time.Hour, until)
	})
}

// TestClockInTestSuite tests the clock integration with TestContainerSuite
func TestClockInTestSuite(t *testing.T) {
	suite := setupTestSuite(t)

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
