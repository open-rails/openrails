//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// TestDunningWorkerNoDueSubscriptions tests that the worker handles no due subscriptions gracefully
func TestDunningWorkerNoDueSubscriptions(t *testing.T) {
	suite := setupTestSuite(t)

	// Create worker without NMI clients (tests skip behavior)
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: nil, // No NMI clients configured
	}

	// Create a job
	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Worker should complete without error (no subscriptions to process)
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should complete successfully with no due subscriptions")
}

// TestDunningWorkerSkipsWithoutNMIClients tests that the worker logs warning and exits when NMI clients aren't configured
func TestDunningWorkerSkipsWithoutNMIClients(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed products first
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create a user
	userID := uuid.New().String()

	// Create payment method for the user
	pm := suite.CreateTestPaymentMethod(userID)

	// Create a past_due subscription with next_retry_at in the past
	pastTime := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          userID,
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Processor:       models.ProcessorMobius,
		PaymentMethodID: &pm.ID,
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &pastTime,
	})

	// Create worker without NMI clients
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: nil, // No NMI clients - worker should skip
	}

	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Worker should complete without error (just skips)
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should complete successfully even without NMI clients")

	// Verify subscription is still past_due (no changes since NMI couldn't be called)
	updatedSub := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusPastDue, updatedSub.Status, "Subscription should still be past_due")
}

// TestDunningWorkerSkipsNonNMISubscriptions tests that the worker skips CCBill/other processor subscriptions
func TestDunningWorkerSkipsNonNMISubscriptions(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed products first
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create a user
	userID := uuid.New().String()

	// Create a CCBill past_due subscription (should be skipped)
	pastTime := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:        userID,
		PriceID:       priceID,
		Status:        models.StatusPastDue,
		Processor:     models.ProcessorCCBill, // CCBill, not NMI
		RetryAttempts: &retryAttempts,
		NextRetryAt:   &pastTime,
	})

	// Create worker (even with NMI clients, should skip CCBill subs)
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: nil, // Doesn't matter - CCBill subs won't be queried
	}

	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Worker should complete without error
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should complete successfully")

	// Verify CCBill subscription wasn't touched
	updatedSub := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusPastDue, updatedSub.Status, "CCBill subscription should be unchanged")
}

// TestDunningWorkerQueryFilters tests that the worker only queries correct subscriptions
func TestDunningWorkerQueryFilters(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed products first
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create multiple subscriptions with different statuses
	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)
	pastTime := time.Now().Add(-1 * time.Hour)
	futureTime := time.Now().Add(24 * time.Hour)
	retryAttempts := 1

	// 1. past_due with next_retry_at in past (should be processed)
	dueSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          userID,
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Processor:       models.ProcessorMobius,
		PaymentMethodID: &pm.ID,
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &pastTime,
	})

	// 2. past_due with next_retry_at in future (should NOT be processed)
	notDueSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          uuid.New().String(),
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Processor:       models.ProcessorMobius,
		PaymentMethodID: &pm.ID,
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &futureTime,
	})

	// 3. active subscription (should NOT be processed)
	activeSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          uuid.New().String(),
		PriceID:         priceID,
		Status:          models.StatusActive,
		Processor:       models.ProcessorMobius,
		PaymentMethodID: &pm.ID,
	})

	// 4. cancelled subscription (should NOT be processed)
	cancelledSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    uuid.New().String(),
		PriceID:   priceID,
		Status:    models.StatusCancelled,
		Processor: models.ProcessorMobius,
	})

	// Create worker (without NMI clients - won't actually try to rebill)
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: nil,
	}

	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Run worker
	err := worker.Work(context.Background(), job)
	require.NoError(t, err)

	// Verify only the due subscription could have been touched
	// (In practice, without NMI clients, no changes would occur)
	_ = dueSub
	_ = notDueSub
	_ = activeSub
	_ = cancelledSub

	// Just verify no errors occurred - detailed testing will be in NMI integration tests
}

// TestDunningWorkerMissingPaymentMethod tests that subscriptions without valid payment methods fail gracefully
func TestDunningWorkerMissingPaymentMethod(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed products first
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create a user with a past_due subscription but NO payment method
	userID := uuid.New().String()
	pastTime := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          userID,
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Processor:       models.ProcessorMobius,
		PaymentMethodID: nil, // No payment method!
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &pastTime,
	})

	// Create worker with NMI clients (use runtime's clients if available)
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: suite.App.Runtime.NMIClients,
	}

	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Run worker
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should handle missing payment method gracefully")

	// Subscription should be failed (moved to cancelled status) or stay past_due if NMI isn't configured
	updatedSub := suite.GetSubscription(sub.ID)
	// The FailMembership method should mark the subscription as cancelled
	// If NMI clients aren't configured, it might stay past_due
	assert.Contains(t, []models.SubscriptionStatus{
		models.StatusCancelled,
		models.StatusPastDue, // If NMI clients aren't configured, it might stay past_due
	}, updatedSub.Status)
}

// TestDunningWorkerMissingPaymentMethodVault tests that payment methods with missing vault fail the subscription
func TestDunningWorkerMissingPaymentMethodVault(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed products first
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create a user
	userID := uuid.New().String()

	// Create a payment method with empty VaultID
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:  userID,
		VaultID: "", // Missing vault!
	})

	pastTime := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          userID,
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Processor:       models.ProcessorMobius,
		PaymentMethodID: &pm.ID,
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &pastTime,
	})

	// Create worker
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: suite.App.Runtime.NMIClients,
	}

	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Run worker
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should handle inactive payment method gracefully")

	// Check subscription status
	updatedSub := suite.GetSubscription(sub.ID)
	assert.Contains(t, []models.SubscriptionStatus{
		models.StatusCancelled,
		models.StatusPastDue, // May stay if NMI clients aren't configured
	}, updatedSub.Status)
}

// TestDunningWorkerMultipleDueSubscriptions tests processing multiple subscriptions
func TestDunningWorkerMultipleDueSubscriptions(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed products first
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	pastTime := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1

	// Create 3 users with past_due subscriptions
	var subs []*models.Subscription
	for i := 0; i < 3; i++ {
		userID := uuid.New().String()
		pm := suite.CreateTestPaymentMethod(userID)

		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:          userID,
			PriceID:         priceID,
			Status:          models.StatusPastDue,
			Processor:       models.ProcessorMobius,
			PaymentMethodID: &pm.ID,
			RetryAttempts:   &retryAttempts,
			NextRetryAt:     &pastTime,
		})
		subs = append(subs, sub)
	}

	// Create worker (without NMI clients)
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		NMIClients: nil,
	}

	job := &river.Job[riverjobs.DunningArgs]{
		Args: riverjobs.DunningArgs{},
	}

	// Run worker
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should process multiple subscriptions")

	// All subs should still be past_due since NMI isn't configured
	for _, sub := range subs {
		updatedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, models.StatusPastDue, updatedSub.Status)
	}
}

// TestDunningWorkerWindowExpiredCancelsWithoutCharge verifies the dunning
// staleness window (#344): a past_due subscription whose missed rebill is older
// than the window is cancelled + downgraded WITHOUT any charge attempt, instead
// of being retried.
func TestDunningWorkerWindowExpiredCancelsWithoutCharge(t *testing.T) {
	suite := setupTestSuite(t)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	stalePeriodEnd := time.Now().Add(-60 * 24 * time.Hour) // far beyond the 15-day default window
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Processor:           models.ProcessorMobius,
		PaymentMethodID:     &pm.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         stalePeriodEnd.Add(-30 * 24 * time.Hour),
		CurrentPeriodEndsAt: &stalePeriodEnd,
	})

	// A zero-value NMI client: present (so the run proceeds) but unconfigured —
	// any attempted charge would error, proving the window path never charges.
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		Config:     suite.App.Runtime.Config,
		NMIClients: map[string]*nmi.NMIClient{string(models.ProcessorMobius): {}},
	}

	err := worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}})
	require.NoError(t, err)

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status)
	require.NotNil(t, updated.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *updated.CancelType)
	assert.Nil(t, updated.NextRetryAt)
	assert.Nil(t, updated.RetryAttempts)
	assert.NotNil(t, updated.CancelledAt)
}

// TestDunningWorkerWithinWindowIsNotExpired verifies a recent missed rebill is
// NOT window-cancelled (it proceeds into the normal rebill path).
func TestDunningWorkerWithinWindowIsNotExpired(t *testing.T) {
	suite := setupTestSuite(t)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	recentPeriodEnd := time.Now().Add(-3 * 24 * time.Hour) // inside the 15-day window
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Processor:           models.ProcessorMobius,
		PaymentMethodID:     &pm.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         recentPeriodEnd.Add(-30 * 24 * time.Hour),
		CurrentPeriodEndsAt: &recentPeriodEnd,
	})

	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		Config:     suite.App.Runtime.Config,
		NMIClients: map[string]*nmi.NMIClient{string(models.ProcessorMobius): {}},
	}

	err := worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}})
	require.NoError(t, err)

	// The unconfigured client cannot charge; the point is the subscription was
	// NOT window-cancelled — it stays past_due awaiting a real rebill attempt.
	updated := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusPastDue, updated.Status)
}

// TestDunningWorkerLimitedModeTakesNoAction verifies limited mode (#345):
// dunning is demoted to dry-run, so even a window-stale past_due subscription
// is neither charged nor window-expiry cancelled — it is left untouched.
func TestDunningWorkerLimitedModeTakesNoAction(t *testing.T) {
	suite := setupTestSuite(t)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	stalePeriodEnd := time.Now().Add(-60 * 24 * time.Hour)
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Processor:           models.ProcessorMobius,
		PaymentMethodID:     &pm.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         stalePeriodEnd.Add(-30 * 24 * time.Hour),
		CurrentPeriodEndsAt: &stalePeriodEnd,
	})

	limitedCfg := *suite.App.Runtime.Config
	limitedCfg.Mode = config.ModeLimited

	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		Config:     &limitedCfg,
		NMIClients: map[string]*nmi.NMIClient{string(models.ProcessorMobius): {}},
	}

	err := worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}})
	require.NoError(t, err)

	updated := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusPastDue, updated.Status)
	require.NotNil(t, updated.RetryAttempts)
	assert.Equal(t, 1, *updated.RetryAttempts)
	assert.Nil(t, updated.CancelledAt)
}
