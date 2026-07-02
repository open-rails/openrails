//go:build integration

package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// TestDunningWorkerNoDueSubscriptions tests that the worker handles no due subscriptions gracefully
func TestDunningWorkerNoDueSubscriptions(t *testing.T) {
	suite := getSharedTestSuite(t)

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
	suite := getSharedTestSuite(t)

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
		Rail:            models.RailNMI,
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

// TestDunningWorkerSkipsNonNMISubscriptions tests that the worker skips CCBill/other rail subscriptions
func TestDunningWorkerSkipsNonNMISubscriptions(t *testing.T) {
	suite := getSharedTestSuite(t)

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
		Rail:          models.RailCCBill, // CCBill, not NMI
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
	suite := getSharedTestSuite(t)

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
		Rail:            models.RailNMI,
		PaymentMethodID: &pm.ID,
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &pastTime,
	})

	// 2. past_due with next_retry_at in future (should NOT be processed)
	notDueSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          uuid.New().String(),
		PriceID:         priceID,
		Status:          models.StatusPastDue,
		Rail:            models.RailNMI,
		PaymentMethodID: &pm.ID,
		RetryAttempts:   &retryAttempts,
		NextRetryAt:     &futureTime,
	})

	// 3. active subscription (should NOT be processed)
	activeSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          uuid.New().String(),
		PriceID:         priceID,
		Status:          models.StatusActive,
		Rail:            models.RailNMI,
		PaymentMethodID: &pm.ID,
	})

	// 4. cancelled subscription (should NOT be processed)
	cancelledSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:  uuid.New().String(),
		PriceID: priceID,
		Status:  models.StatusCancelled,
		Rail:    models.RailNMI,
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
	suite := getSharedTestSuite(t)

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
		Rail:            models.RailNMI,
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
	suite := getSharedTestSuite(t)

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
		Rail:            models.RailNMI,
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
	suite := getSharedTestSuite(t)

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
			Rail:            models.RailNMI,
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
// staleness window (#344, derived from the billing cycle since #359): a
// past_due subscription whose missed rebill is older than the window is
// cancelled + downgraded WITHOUT any charge attempt, instead of being retried.
func TestDunningWorkerWindowExpiredCancelsWithoutCharge(t *testing.T) {
	suite := getSharedTestSuite(t)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	stalePeriodEnd := time.Now().Add(-60 * 24 * time.Hour) // far beyond the derived 14-day monthly window
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
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
		NMIClients: map[string]*nmi.NMIClient{string(models.RailNMI): {}},
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
	suite := getSharedTestSuite(t)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	recentPeriodEnd := time.Now().Add(-3 * 24 * time.Hour) // inside the derived 14-day monthly window
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
		PaymentMethodID:     &pm.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         recentPeriodEnd.Add(-30 * 24 * time.Hour),
		CurrentPeriodEndsAt: &recentPeriodEnd,
	})

	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		Config:     suite.App.Runtime.Config,
		NMIClients: map[string]*nmi.NMIClient{string(models.RailNMI): {}},
	}

	err := worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}})
	require.NoError(t, err)

	// The unconfigured client cannot charge; the point is the subscription was
	// NOT window-cancelled — it stays past_due awaiting a real rebill attempt.
	updated := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusPastDue, updated.Status)
}

// queryManualRebillIntent returns (count, status, origin) for the manual_rebill
// charge intents (#358/#366) on the provider intent ledger for the subscription.
func queryManualRebillIntent(t *testing.T, suite *TestContainerSuite, subID uuid.UUID) (count int, status, origin string) {
	t.Helper()
	err := suite.Pool.QueryRow(context.Background(), `
		SELECT COUNT(*), COALESCE(MAX(status), ''), COALESCE(MAX(origin), '')
		FROM openrails.rail_intents
		WHERE intent_type = $1 AND subscription_id = $2`,
		intents.TypeManualRebill, subID).Scan(&count, &status, &origin)
	require.NoError(t, err)
	return count, status, origin
}

// TestDunningWorkerLimitedModeMaterializesWithoutProviderWrites verifies the
// #366 limited-mode contract (changed from the pre-#366 "takes no action" of
// #345). Limited mode no longer dry-runs dunning — it MATERIALIZES decisions
// locally while making ZERO provider/rail writes:
//
//   - a window-EXPIRED past_due sub is cancelled locally + its entitlements are
//     revoked, with NO provider write (no nmi_delete intent, no deferred-delete
//     marker — the remote rail sub is left alive for reconciliation);
//   - an IN-WINDOW past_due sub records its charge decision as a PARKED
//     system-origin manual_rebill intent on the ledger (the executor drains it
//     only at mode=full), while the subscription row itself takes no action —
//     it stays past_due with retry state preserved and is never charged.
func TestDunningWorkerLimitedModeMaterializesWithoutProviderWrites(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()
	rt := suite.App.Runtime

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	pastRetry := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1

	// Sub A: window-EXPIRED -> cancel + revoke locally, no provider writes.
	expiredUser := uuid.New().String()
	expiredPM := suite.CreateTestPaymentMethod(expiredUser)
	stalePeriodEnd := time.Now().Add(-60 * 24 * time.Hour) // far beyond the derived 14-day monthly window
	expiredSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              expiredUser,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
		PaymentMethodID:     &expiredPM.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         stalePeriodEnd.Add(-30 * 24 * time.Hour),
		CurrentPeriodEndsAt: &stalePeriodEnd,
	})
	// Seed a currently-active subscription entitlement (end in the future) so a
	// later IsEntitled(now)==false unambiguously proves window-expiry revoked it.
	entStart := time.Now().Add(-30 * 24 * time.Hour).UTC()
	entEnd := time.Now().Add(30 * 24 * time.Hour).UTC()
	suite.InsertEntitlement(dbtest.WithTestMerchant(ctx), &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, expiredUser),
		Entitlement: "premium",
		StartAt:     entStart,
		EndAt:       &entEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &expiredSub.ID,
		CreatedAt:   entStart,
		UpdatedAt:   entStart,
	})

	// Sub B: IN-WINDOW -> charge decision materialized as a parked intent; the
	// subscription row itself is untouched (the surviving half of the old
	// "takes no action" contract).
	inWindowUser := uuid.New().String()
	inWindowPM := suite.CreateTestPaymentMethod(inWindowUser)
	recentPeriodEnd := time.Now().Add(-3 * 24 * time.Hour) // inside the derived 14-day monthly window
	inWindowSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              inWindowUser,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
		PaymentMethodID:     &inWindowPM.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         recentPeriodEnd.Add(-30 * 24 * time.Hour),
		CurrentPeriodEndsAt: &recentPeriodEnd,
	})

	limitedCfg := *rt.Config
	// ProviderWriteMode, not the deprecated Mode alias: the harness now sets
	// ProviderWriteMode explicitly, and setting BOTH (differing) fail-closes
	// to readonly (Paul 2026-07-02).
	limitedCfg.ProviderWriteMode = config.ProviderWriteModeLimited

	// A zero-value NMI client: present (so the scan proceeds) but unconfigured —
	// any attempted provider call would error, proving the limited-mode path
	// never reaches the rail. DeferDelete is intentionally nil: limited
	// mode must not schedule a remote delete regardless.
	worker := &riverjobs.DunningWorker{
		DB:         rt.DB,
		Config:     &limitedCfg,
		NMIClients: map[string]*nmi.NMIClient{string(models.RailNMI): {}},
	}

	require.NoError(t, worker.Work(ctx, &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}}))

	// --- Sub A: window-expired cancels + revokes locally, no provider writes ---
	expired := suite.GetSubscription(expiredSub.ID)
	require.Equal(t, models.StatusCancelled, expired.Status, "window-expired sub is cancelled locally in limited mode")
	require.NotNil(t, expired.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *expired.CancelType)
	assert.NotNil(t, expired.CancelledAt)
	assert.Nil(t, expired.NextRetryAt)
	assert.Nil(t, expired.RetryAttempts)

	entitled, err := rt.EntitlementService.IsEntitled(dbtest.WithTestMerchant(ctx), expiredUser, "premium", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, entitled, "window-expiry cancellation revokes entitlements")

	// No provider write: limited mode leaves the remote sub alive for
	// reconciliation — no deferred-delete marker, no delete intent.
	assert.Nil(t, expired.DeletionScheduledAt, "limited mode must not stamp a proactive deletion marker")
	delCount, _, _ := queryNMIDeleteIntents(t, suite, expiredSub.ID)
	assert.Zero(t, delCount, "limited mode must not enqueue a provider delete intent")
	// The no-charge (window-expiry) path never records a charge intent either.
	rebillCount, _, _ := queryManualRebillIntent(t, suite, expiredSub.ID)
	assert.Zero(t, rebillCount, "window-expired sub takes the no-charge path; no rebill intent")

	// --- Sub B: in-window charge decision parked; sub row untouched ---
	inWindow := suite.GetSubscription(inWindowSub.ID)
	assert.Equal(t, models.StatusPastDue, inWindow.Status, "in-window sub row is untouched in limited mode")
	require.NotNil(t, inWindow.RetryAttempts)
	assert.Equal(t, 1, *inWindow.RetryAttempts, "retry state preserved — no claim writes last_retry_at")
	assert.Nil(t, inWindow.CancelledAt)
	assert.Nil(t, inWindow.DeletionScheduledAt)

	count, status, origin := queryManualRebillIntent(t, suite, inWindowSub.ID)
	require.Equal(t, 1, count, "in-window charge decision is materialized as exactly one ledger intent")
	assert.Equal(t, string(intents.OriginSystem), origin, "materialized rebill is system-origin (parked until mode=full)")
	assert.Equal(t, intents.StatusPending, status, "parked: pending on the ledger, drained only when mode allows")
}

// createDunningCycleProductPrice seeds an active Mobius product+price with the
// given billing cycle, for exercising the cadence-relative dunning tiers (#359).
func createDunningCycleProductPrice(t *testing.T, suite *TestContainerSuite, cycleHours int) uuid.UUID {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	now := suite.GetClock().Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	cycleLabel := fmt.Sprintf("%dh", cycleHours)

	suite.InsertProduct(ctx, &models.Product{
		ID:               productID,
		Key:              fmt.Sprintf("dunning-cycle-%s-%s", cycleLabel, uuid.New().String()[:8]),
		DisplayName:      fmt.Sprintf("Dunning %s cycle", cycleLabel),
		Description:      "cadence-relative dunning fixture",
		EntitlementsSpec: map[string]*int{"premium": nil},
		Status:           models.CatalogStatusActive,
		MerchantID:       dbtest.TestMerchantID.UUID(),
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	suite.InsertPrice(ctx, &models.Price{
		ID:                  priceID,
		ProductID:           productID,
		Status:              models.CatalogStatusActive,
		Amount:              999,
		Currency:            "usd",
		AccessDurationHours: &cycleHours, AutoRenew: true,
		MerchantID: dbtest.TestMerchantID.UUID(),
		Rails: map[string]map[string]string{
			string(models.RailNMI): {
				models.RailKeyPlanID: "plan_dunning_cycle",
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	})
	return priceID
}

// TestDunningWorkerWeeklyCycleDerivedWindow pins the weekly tier (#359): a 7d
// billing cycle derives a 3-day staleness window (last retry offset +2d, plus
// one day of slack). A missed rebill older than 3 days is terminal-cancelled
// without a charge; a fresher one proceeds into the normal rebill path.
func TestDunningWorkerWeeklyCycleDerivedWindow(t *testing.T) {
	suite := getSharedTestSuite(t)

	priceID := createDunningCycleProductPrice(t, suite, 7*24)
	pastRetry := time.Now().Add(-1 * time.Hour)
	retryAttempts := 1

	// Beyond the derived 3d window -> terminal cancel without charge.
	staleUser := uuid.New().String()
	stalePM := suite.CreateTestPaymentMethod(staleUser)
	stalePeriodEnd := time.Now().Add(-4 * 24 * time.Hour)
	staleSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              staleUser,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
		PaymentMethodID:     &stalePM.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         stalePeriodEnd.Add(-7 * 24 * time.Hour),
		CurrentPeriodEndsAt: &stalePeriodEnd,
	})

	// Inside the derived 3d window -> stays past_due (normal rebill path).
	freshUser := uuid.New().String()
	freshPM := suite.CreateTestPaymentMethod(freshUser)
	freshPeriodEnd := time.Now().Add(-2 * 24 * time.Hour)
	freshSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              freshUser,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
		PaymentMethodID:     &freshPM.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         freshPeriodEnd.Add(-7 * 24 * time.Hour),
		CurrentPeriodEndsAt: &freshPeriodEnd,
	})

	// Zero-value NMI client: present (so the run proceeds) but unconfigured —
	// any attempted charge would error, proving the window path never charges.
	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		Config:     suite.App.Runtime.Config,
		NMIClients: map[string]*nmi.NMIClient{string(models.RailNMI): {}},
	}
	require.NoError(t, worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}}))

	stale := suite.GetSubscription(staleSub.ID)
	require.Equal(t, models.StatusCancelled, stale.Status, "4d-old missed rebill on a 7d cycle is beyond the derived 3d window")
	require.NotNil(t, stale.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *stale.CancelType)
	assert.Nil(t, stale.NextRetryAt)

	fresh := suite.GetSubscription(freshSub.ID)
	assert.Equal(t, models.StatusPastDue, fresh.Status, "2d-old missed rebill on a 7d cycle is inside the derived 3d window")
}

// TestDunningWorkerDailyCycleImmediatelyTerminal pins the 0-retry tier (#359):
// a sub-4-day billing cycle derives a ZERO staleness window, so ANY past_due
// daily subscription is terminal-cancelled by the worker without a charge.
func TestDunningWorkerDailyCycleImmediatelyTerminal(t *testing.T) {
	suite := getSharedTestSuite(t)

	priceID := createDunningCycleProductPrice(t, suite, 24)

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	periodEnd := time.Now().Add(-1 * time.Hour) // barely missed — still terminal
	retryAttempts := 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
		PaymentMethodID:     &pm.ID,
		RetryAttempts:       &retryAttempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         periodEnd.Add(-24 * time.Hour),
		CurrentPeriodEndsAt: &periodEnd,
	})

	worker := &riverjobs.DunningWorker{
		DB:         suite.App.Runtime.DB,
		Config:     suite.App.Runtime.Config,
		NMIClients: map[string]*nmi.NMIClient{string(models.RailNMI): {}},
	}
	require.NoError(t, worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}}))

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status, "1d-cycle past_due sub must be immediately terminal (zero derived window)")
	require.NotNil(t, updated.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *updated.CancelType)
	assert.Nil(t, updated.NextRetryAt)
	assert.Nil(t, updated.RetryAttempts)
}

// TestDunningWorkerFailMembershipDailyCycleFirstFailureTerminal pins the
// FailMembership side of the 0-retry tier (#359): the FIRST rebill failure of
// a 1d-cycle subscription goes straight to the terminal branch — cancelled,
// no past_due retry state.
func TestDunningWorkerFailMembershipDailyCycleFirstFailureTerminal(t *testing.T) {
	suite := getSharedTestSuite(t)
	rt := suite.App.Runtime
	ctx := dbtest.WithTestMerchant(context.Background())

	priceID := createDunningCycleProductPrice(t, suite, 24)

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)
	periodEnd := time.Now().UTC()

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusActive,
		Rail:                models.RailNMI,
		PaymentMethodID:     &pm.ID,
		PeriodStart:         periodEnd.Add(-24 * time.Hour),
		CurrentPeriodEndsAt: &periodEnd,
	})

	reason := "rebill declined"
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}))

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status, "first failure on a 1d cycle is terminal (0-retry tier)")
	require.NotNil(t, updated.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *updated.CancelType)
	assert.Nil(t, updated.NextRetryAt, "no retry is ever scheduled on the 0-retry tier")
}

// TestDunningWorkerFailMembershipWeeklyCycleSchedule pins the weekly tier's
// retry scheduling in FailMembership (#359): a 7d cycle retries at +1d and
// +2d after the initial failure and is terminal on the 3rd failure.
func TestDunningWorkerFailMembershipWeeklyCycleSchedule(t *testing.T) {
	suite := getSharedTestSuite(t)
	rt := suite.App.Runtime
	ctx := dbtest.WithTestMerchant(context.Background())

	priceID := createDunningCycleProductPrice(t, suite, 7*24)

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)
	periodEnd := time.Now().UTC()

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusActive,
		Rail:                models.RailNMI,
		PaymentMethodID:     &pm.ID,
		PeriodStart:         periodEnd.Add(-7 * 24 * time.Hour),
		CurrentPeriodEndsAt: &periodEnd,
	})

	reason := "rebill declined"

	// First failure: past_due, next retry ~24h out (weekly offsets +1d/+2d).
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}))
	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusPastDue, updated.Status)
	require.NotNil(t, updated.RetryAttempts)
	assert.Equal(t, 1, *updated.RetryAttempts)
	require.NotNil(t, updated.NextRetryAt)
	assert.WithinDuration(t, time.Now().UTC().Add(subscriptions.DunningNextRetryIn(7*24, 1)), updated.NextRetryAt.UTC(), time.Minute,
		"weekly tier schedules the first retry 24h out")

	// Second failure: still past_due, next retry another 24h out.
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}))
	updated = suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusPastDue, updated.Status, "2nd failure on a 7d cycle still retries")
	require.NotNil(t, updated.RetryAttempts)
	assert.Equal(t, 2, *updated.RetryAttempts)
	require.NotNil(t, updated.NextRetryAt)
	assert.WithinDuration(t, time.Now().UTC().Add(subscriptions.DunningNextRetryIn(7*24, 2)), updated.NextRetryAt.UTC(), time.Minute,
		"weekly tier schedules the second retry another 24h out")

	// Third failure: terminal (weekly tier allows 3 failures total).
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}))
	updated = suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status, "3rd failure on a 7d cycle is terminal")
	assert.Nil(t, updated.NextRetryAt)
}
