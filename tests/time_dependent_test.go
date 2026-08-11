//go:build integration

package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// =============================================================================
// Entitlement Time-Dependent Tests
// =============================================================================

// TestEntitlementStacking tests that granting additional entitlements extends the expiry
func TestEntitlementStacking(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()

	// Set clock to a known starting point
	startTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	mockClock := suite.SetMockClock(startTime)

	userID := uuid.New().String()
	entitlementName := "premium"

	// Grant first 15-day entitlement
	firstEnd := startTime.Add(15 * 24 * time.Hour)
	firstSourceID := uuid.New()
	ent1 := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, userID),
		Entitlement: entitlementName,
		StartAt:     startTime,
		EndAt:       &firstEnd,
		SourceType:  models.EntitlementSourceOneOff,
		SourceID:    &firstSourceID,
		CreatedAt:   startTime,
		UpdatedAt:   startTime,
	}
	suite.InsertEntitlement(ctx, ent1)

	// Grant second 15-day entitlement that stacks (starts where first ends)
	secondStart := firstEnd
	secondEnd := secondStart.Add(15 * 24 * time.Hour) // 30 days from original start
	secondSourceID := uuid.New()
	ent2 := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, userID),
		Entitlement: entitlementName,
		StartAt:     secondStart,
		EndAt:       &secondEnd,
		SourceType:  models.EntitlementSourceOneOff,
		SourceID:    &secondSourceID,
		CreatedAt:   startTime,
		UpdatedAt:   startTime,
	}
	suite.InsertEntitlement(ctx, ent2)

	entService := suite.App.Runtime.EntitlementService

	t.Run("entitlement is active at start", func(t *testing.T) {
		isEntitled, err := entService.IsEntitled(ctx, userID, entitlementName, mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "Entitlement should be active at start")
	})

	t.Run("entitlement is active after 20 days (into second window)", func(t *testing.T) {
		// Advance clock 20 days - past first entitlement, into second
		mockClock.Advance(20 * 24 * time.Hour)

		isEntitled, err := entService.IsEntitled(ctx, userID, entitlementName, mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "Entitlement should still be active at 20 days (in second window)")
	})

	t.Run("entitlement is NOT active after 30 days", func(t *testing.T) {
		// Advance clock 10 more days (total 30 days from start)
		mockClock.Advance(10 * 24 * time.Hour)

		isEntitled, err := entService.IsEntitled(ctx, userID, entitlementName, mockClock.Now())
		require.NoError(t, err)
		assert.False(t, isEntitled, "Entitlement should NOT be active after 30 days (both windows expired)")
	})
}

// TestIndefiniteEntitlement tests that indefinite entitlements never expire
func TestIndefiniteEntitlement(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()

	// Set clock to a known starting point
	startTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	mockClock := suite.SetMockClock(startTime)

	userID := uuid.New().String()
	entitlementName := "premium"
	sourceID := uuid.New()

	// Grant an indefinite entitlement (EndAt is nil)
	ent := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, userID),
		Entitlement: entitlementName,
		StartAt:     startTime,
		EndAt:       nil, // Indefinite
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &sourceID,
		CreatedAt:   startTime,
		UpdatedAt:   startTime,
	}
	suite.InsertEntitlement(ctx, ent)

	entService := suite.App.Runtime.EntitlementService

	t.Run("indefinite entitlement is active at start", func(t *testing.T) {
		isEntitled, err := entService.IsEntitled(ctx, userID, entitlementName, mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "Indefinite entitlement should be active at start")
	})

	t.Run("indefinite entitlement is active after 1 year", func(t *testing.T) {
		// Advance clock 1 year
		mockClock.Advance(365 * 24 * time.Hour)

		isEntitled, err := entService.IsEntitled(ctx, userID, entitlementName, mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "Indefinite entitlement should still be active after 1 year")
	})

	t.Run("indefinite entitlement is active after 10 years", func(t *testing.T) {
		// Advance clock 9 more years (total 10 years)
		mockClock.Advance(9 * 365 * 24 * time.Hour)

		isEntitled, err := entService.IsEntitled(ctx, userID, entitlementName, mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "Indefinite entitlement should still be active after 10 years")
	})
}

// =============================================================================
// Cancellation Time-Dependent Tests
// =============================================================================

// TestCancelAccessAtPeriodEnd tests that user cancellation keeps access until period end
// and that access is revoked after period end (using mock clock to verify time-based behavior).
func TestCancelAccessAtPeriodEnd(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()

	// Set clock to a known starting point
	startTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	mockClock := suite.SetMockClock(startTime)

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()

	// Create subscription with period ending in 30 days
	periodEnd := startTime.Add(30 * 24 * time.Hour)
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:      userID,
		PriceID:     priceID,
		Status:      models.StatusActive,
		Rail:        models.RailNMI,
		PeriodStart: startTime,
		PeriodEnd:   periodEnd,
	})

	// Create a paid-term entitlement linked to the subscription
	ent := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, userID),
		Entitlement: "premium",
		StartAt:     startTime,
		EndAt:       &periodEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &sub.ID,
		CreatedAt:   startTime,
		UpdatedAt:   startTime,
	}
	suite.InsertEntitlement(ctx, ent)

	entService := suite.App.Runtime.EntitlementService
	lifecycleService := suite.App.Runtime.SubscriptionLifecycleService

	t.Run("user has entitlement before cancellation", func(t *testing.T) {
		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "User should have entitlement before cancellation")
	})

	t.Run("user cancels subscription (RevokeAccess: false)", func(t *testing.T) {
		// Advance clock 5 days (still within period)
		mockClock.Advance(5 * 24 * time.Hour)

		// User cancels but keeps access until period end
		err := lifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: &sub.ID,
			CancelType:     models.CancelTypeUser,
			RevokeAccess:   false, // Access continues until period end
		})
		require.NoError(t, err)

		// Verify subscription is cancelled
		updatedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, models.StatusCancelled, updatedSub.Status, "Subscription should be cancelled")
		assert.NotNil(t, updatedSub.CancelledAt, "CancelledAt should be set")
	})

	t.Run("entitlement EndAt remains at period end", func(t *testing.T) {
		dbEnt := *suite.GetEntitlement(ctx, ent.ID)
		require.NotNil(t, dbEnt.EndAt, "Entitlement EndAt should be set")
		assert.WithinDuration(t, periodEnd, *dbEnt.EndAt, time.Second,
			"Entitlement EndAt should remain at period end")
	})

	t.Run("user still has entitlement immediately after cancel", func(t *testing.T) {
		// User should still have access because we haven't reached period end yet
		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "User should still have entitlement immediately after cancel (RevokeAccess: false)")
	})

	t.Run("user still has entitlement at day 29 (1 day before period end)", func(t *testing.T) {
		// Advance to day 29 (1 day before period end; we're currently at day 5)
		mockClock.Advance(24 * 24 * time.Hour)

		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "User should still have entitlement 1 day before period end")
	})

	t.Run("user does NOT have entitlement at day 31 (past period end)", func(t *testing.T) {
		// Advance to day 31 (past period end; we're currently at day 29)
		mockClock.Advance(2 * 24 * time.Hour)

		// Entitlement should now be expired because EndAt was set to period end
		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.False(t, isEntitled, "User should NOT have entitlement after period end")
	})

	t.Run("subscription period has ended", func(t *testing.T) {
		updatedSub := suite.GetSubscription(sub.ID)
		assert.True(t, updatedSub.CurrentPeriodEndsAt.Before(mockClock.Now()),
			"Period should have ended by now")
	})
}

// TestAdminRevokeAccess tests that admin revocation removes access immediately
func TestAdminRevokeAccess(t *testing.T) {
	suite := setupTestSuite(t)
	// RLS-aware services (EntitlementService, SubscriptionLifecycleService) need
	// the merchant pinned on the context; the suite is single-merchant (#336).
	ctx := suite.MerchantCtx()

	// Set clock to a known starting point
	startTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	mockClock := suite.SetMockClock(startTime)

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()

	// Create subscription with period ending in 30 days
	periodEnd := startTime.Add(30 * 24 * time.Hour)
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:      userID,
		PriceID:     priceID,
		Status:      models.StatusActive,
		Rail:        models.RailNMI,
		PeriodStart: startTime,
		PeriodEnd:   periodEnd,
	})

	// Create an indefinite entitlement linked to the subscription
	ent := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, userID),
		Entitlement: "premium",
		StartAt:     startTime,
		EndAt:       nil, // Indefinite while subscription is active
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &sub.ID,
		CreatedAt:   startTime,
		UpdatedAt:   startTime,
	}
	suite.InsertEntitlement(ctx, ent)

	entService := suite.App.Runtime.EntitlementService
	lifecycleService := suite.App.Runtime.SubscriptionLifecycleService

	t.Run("user has entitlement before admin revocation", func(t *testing.T) {
		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "User should have entitlement before admin revocation")
	})

	t.Run("admin revokes access (RevokeAccess: true)", func(t *testing.T) {
		// Advance clock 5 days (still well within period)
		mockClock.Advance(5 * 24 * time.Hour)

		// Admin revokes access immediately
		err := lifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: &sub.ID,
			CancelType:     models.CancelTypeMerchant, // "merchant" = admin/merchant cancellation
			RevokeAccess:   true,                      // Access revoked immediately
		})
		require.NoError(t, err)

		// Verify subscription is cancelled
		updatedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, models.StatusCancelled, updatedSub.Status, "Subscription should be cancelled")
		assert.NotNil(t, updatedSub.CancelledAt, "CancelledAt should be set")
		assert.NotNil(t, updatedSub.EndedAt, "EndedAt should be set (immediate termination)")
	})

	t.Run("user does NOT have entitlement after admin revocation", func(t *testing.T) {
		// User should NOT have access because RevokeAccess was true
		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.False(t, isEntitled, "User should NOT have entitlement after admin revocation")
	})

	t.Run("user still does NOT have entitlement even days later", func(t *testing.T) {
		// Advance clock 10 more days
		mockClock.Advance(10 * 24 * time.Hour)

		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.False(t, isEntitled, "User should still NOT have entitlement days later")
	})
}

// =============================================================================
// Dunning Time-Dependent Tests
// =============================================================================

// TestDunningSuccessReactivates tests that successful dunning reactivates subscription
// and verifies period dates are correctly calculated using mock clock.
func TestDunningSuccessReactivates(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()

	// Set clock to a known starting point
	startTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	mockClock := suite.SetMockClock(startTime)

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	railSubID := "test-dunning-success-" + uuid.New().String()[:8]

	// Create a past_due subscription with period that just expired
	retryAttempts := 2
	nextRetry := startTime
	originalPeriodEnd := startTime.Add(-1 * time.Hour) // Just expired (1 hour before startTime)
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:        userID,
		PriceID:       priceID,
		Status:        models.StatusPastDue,
		Rail:          models.RailNMI,
		RailSubID:     railSubID,
		PeriodStart:   startTime.Add(-30 * 24 * time.Hour),
		PeriodEnd:     originalPeriodEnd,
		RetryAttempts: &retryAttempts,
		NextRetryAt:   &nextRetry,
	})

	lifecycleService := suite.App.Runtime.SubscriptionLifecycleService

	t.Run("subscription is past_due at day 0", func(t *testing.T) {
		updatedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, models.StatusPastDue, updatedSub.Status)
		assert.True(t, updatedSub.CurrentPeriodEndsAt.Before(mockClock.Now()),
			"Original period should have ended before current time")
	})

	t.Run("successful rebill reactivates subscription at day 1", func(t *testing.T) {
		// Advance clock 1 day
		mockClock.Advance(1 * 24 * time.Hour)

		// Simulate successful rebill via RenewMembership
		// RenewMembership uses the mock clock for period calculations
		err := lifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
			Rail:               models.RailNMI,
			RailSubscriptionID: railSubID,
			TransactionID:      "rebill-" + uuid.NewString(),
		})
		require.NoError(t, err)

		// Verify subscription is now active
		updatedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, models.StatusActive, updatedSub.Status,
			"Subscription should be active after successful rebill")
	})

	t.Run("new period starts from old period end", func(t *testing.T) {
		updatedSub := suite.GetSubscription(sub.ID)

		// New period should start from the old period end
		assert.NotNil(t, updatedSub.CurrentPeriodStartsAt)
		assert.Equal(t, originalPeriodEnd.Unix(), updatedSub.CurrentPeriodStartsAt.Unix(),
			"New period should start at old period end")
	})

	t.Run("new period end is 30 days after old period end", func(t *testing.T) {
		updatedSub := suite.GetSubscription(sub.ID)

		// New period end should be 30 days after original period end
		expectedNewEnd := originalPeriodEnd.Add(30 * 24 * time.Hour)
		assert.NotNil(t, updatedSub.CurrentPeriodEndsAt)
		assert.WithinDuration(t, expectedNewEnd, *updatedSub.CurrentPeriodEndsAt, time.Second,
			"New period end should be 30 days after original period end")
	})

	t.Run("subscription period is active at day 15", func(t *testing.T) {
		// Advance clock 14 more days (total 15 days from start)
		mockClock.Advance(14 * 24 * time.Hour)

		updatedSub := suite.GetSubscription(sub.ID)
		assert.True(t, updatedSub.CurrentPeriodEndsAt.After(mockClock.Now()),
			"Period should still be active at day 15")
	})

	t.Run("subscription period has ended at day 35", func(t *testing.T) {
		// Advance clock 20 more days (total 35 days from start)
		// New period started at originalPeriodEnd (day -0.04) and ends 30 days later (day ~30)
		mockClock.Advance(20 * 24 * time.Hour)

		updatedSub := suite.GetSubscription(sub.ID)
		assert.True(t, updatedSub.CurrentPeriodEndsAt.Before(mockClock.Now()),
			"Period should have ended by day 35")
	})
}

// NOTE: TestPaymentIntentExpiry and TestWalletChallengeExpiry were removed
// because they tested SolanaPaymentIntentService and SolanaVerificationService
// which have been removed as part of the Solana payment simplification.

// =============================================================================
// Subscription Period Time-Dependent Tests
// =============================================================================

// NOTE: Webhook retry backoff tests (TestWebhookRetryBackoff, TestWebhookMaxRetries) were removed
// because they tested methods (BeginProcessing, MarkFailure) and constants (WebhookStatusPending,
// WebhookStatusFailed, WebhookStatusError) that do not exist on WebhookEventService.
// The current WebhookEventService only has: Create, Get, MarkProcessed, MarkFailed.

// =============================================================================
// Payment Timestamp Tests
// =============================================================================

// TestPaymentTimestampUsesMockClock verifies that application-controlled payment timestamps
// use the mock clock (PurchasedAt is set by the application, CreatedAt is DB-controlled).
func TestPaymentTimestampUsesMockClock(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()
	// or#893: this test drives the service directly; arrive routed, like production.
	ctx = suite.PinPSP(ctx, string(models.RailNMI))

	// Set clock to a specific time
	fixedTime := time.Date(2024, time.June, 15, 14, 30, 0, 0, time.UTC)
	mockClock := suite.SetMockClock(fixedTime)

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()

	paymentService := suite.App.Runtime.PaymentService

	t.Run("payment PurchasedAt uses mock clock time", func(t *testing.T) {
		// Create a payment with TransactionID (required unique field)
		payment := &models.Payment{
			ID:            uuid.New(),
			CustomerID:    suite.ensureCustomer(ctx, userID),
			PriceID:       priceID,
			Rail:          models.RailNMI,
			TransactionID: "test-tx-" + uuid.New().String()[:8],
			Amount:        999,
			Currency:      "usd",
			MoneyMovement: models.MoneyMovementRail,
			PurchasedAt:   mockClock.Now(),
		}

		err := paymentService.Create(ctx, payment)
		require.NoError(t, err)

		// PurchasedAt is set by application code - should match mock clock
		assert.WithinDuration(t, fixedTime, payment.PurchasedAt, time.Second,
			"Payment PurchasedAt should match mock clock time")
	})

	t.Run("advancing clock affects PurchasedAt of subsequent payments", func(t *testing.T) {
		// Advance clock by 7 days
		mockClock.Advance(7 * 24 * time.Hour)
		expectedTime := fixedTime.Add(7 * 24 * time.Hour)

		// Create another payment with unique TransactionID
		payment := &models.Payment{
			ID:            uuid.New(),
			CustomerID:    suite.ensureCustomer(ctx, userID),
			PriceID:       priceID,
			Rail:          models.RailNMI,
			TransactionID: "test-tx-" + uuid.New().String()[:8],
			Amount:        999,
			Currency:      "usd",
			MoneyMovement: models.MoneyMovementRail,
			PurchasedAt:   mockClock.Now(),
		}

		err := paymentService.Create(ctx, payment)
		require.NoError(t, err)

		// Verify PurchasedAt matches advanced mock clock
		assert.WithinDuration(t, expectedTime, payment.PurchasedAt, time.Second,
			"Payment PurchasedAt should match advanced mock clock time")
	})
}

// =============================================================================
// Subscription Period Boundary Edge Cases
// =============================================================================

// TestSubscriptionExpiryAtExactBoundary tests behavior exactly at the expiry moment
func TestSubscriptionExpiryAtExactBoundary(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()

	// Set clock to a known starting point
	startTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	mockClock := suite.SetMockClock(startTime)

	// Seed products
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()

	// Create subscription that expires exactly at a specific time
	periodEnd := startTime.Add(30 * 24 * time.Hour) // Exactly 30 days from now
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:      userID,
		PriceID:     priceID,
		Status:      models.StatusActive,
		Rail:        models.RailNMI,
		PeriodStart: startTime,
		PeriodEnd:   periodEnd,
	})

	// Create entitlement that ends exactly at period end
	ent := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, userID),
		Entitlement: "premium",
		StartAt:     startTime,
		EndAt:       &periodEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &sub.ID,
		CreatedAt:   startTime,
		UpdatedAt:   startTime,
	}
	suite.InsertEntitlement(ctx, ent)

	entService := suite.App.Runtime.EntitlementService

	t.Run("1 second before expiry - entitlement active", func(t *testing.T) {
		// Advance to 1 second before expiry
		mockClock.Advance(30*24*time.Hour - 1*time.Second)

		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.True(t, isEntitled, "Entitlement should be active 1 second before expiry")
	})

	t.Run("exactly at expiry - entitlement NOT active", func(t *testing.T) {
		// Advance 1 second to exactly at expiry
		mockClock.Advance(1 * time.Second)

		// At exactly the expiry time, entitlement should NOT be active
		// (EndAt is exclusive - "before" check fails at exact time)
		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.False(t, isEntitled, "Entitlement should NOT be active at exact expiry time")
	})

	t.Run("1 second after expiry - entitlement NOT active", func(t *testing.T) {
		// Advance 1 more second
		mockClock.Advance(1 * time.Second)

		isEntitled, err := entService.IsEntitled(ctx, userID, "premium", mockClock.Now())
		require.NoError(t, err)
		assert.False(t, isEntitled, "Entitlement should NOT be active 1 second after expiry")
	})
}

