//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
)

// queryNMIDeleteIntents returns (count, status, nextAttemptAt) for
// nmi_delete_subscription intents on the provider intent ledger (#358) for
// the subscription, across all statuses.
func queryNMIDeleteIntents(t *testing.T, suite *TestContainerSuite, subID uuid.UUID) (count int, status string, nextAttemptAt time.Time) {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	err := suite.Pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COALESCE(MAX(status), ''),
		       COALESCE(MAX(next_attempt_at), '0001-01-01'::timestamptz)
		FROM openrails.provider_intents
		WHERE intent_type = $1 AND subscription_id = $2`,
		intents.TypeNMIDeleteSubscription, subID).Scan(&count, &status, &nextAttemptAt)
	require.NoError(t, err)
	return count, status, nextAttemptAt
}

// countLiveNMIDeleteIntents counts not-yet-resolved delete intents for the
// subscription (the statuses an idempotent re-enqueue deduplicates into one
// row).
func countLiveNMIDeleteIntents(t *testing.T, suite *TestContainerSuite, subID uuid.UUID) int {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	var count int
	err := suite.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM openrails.provider_intents
		WHERE intent_type = $1 AND subscription_id = $2
		  AND status IN ('pending', 'in_flight', 'failed_retryable', 'unknown_needs_verify')`,
		intents.TypeNMIDeleteSubscription, subID).Scan(&count)
	require.NoError(t, err)
	return count
}

// recordingDeferredDeleteScheduler is a test double for
// subscriptions.DeferredDeleteScheduler that records ScheduleNMIDelete calls.
type recordingDeferredDeleteScheduler struct {
	scheduled []scheduledDelete
}

type scheduledDelete struct {
	UserID         string
	SubscriptionID uuid.UUID
	RunAt          time.Time
}

func (r *recordingDeferredDeleteScheduler) ScheduleNMIDelete(_ context.Context, userID string, subscriptionID uuid.UUID, runAt time.Time) error {
	r.scheduled = append(r.scheduled, scheduledDelete{UserID: userID, SubscriptionID: subscriptionID, RunAt: runAt})
	return nil
}

func (r *recordingDeferredDeleteScheduler) CancelNMIDelete(context.Context, string, uuid.UUID) error {
	return nil
}

// WithTx satisfies subscriptions.DeferredDeleteScheduler; the recorder has no
// transactional state, so the same instance keeps recording.
func (r *recordingDeferredDeleteScheduler) WithTx(pgx.Tx) subscriptions.DeferredDeleteScheduler {
	return r
}

// TestMarkerConversionSweepEnqueuesIntents verifies the #358 startup sweep
// that replaced the #344 boot rescan: cancelled subscriptions whose
// DeletionScheduledAt marker survived (kill switch skipped the delete, or a
// pre-ledger job was lost) get a durable nmi_delete_subscription intent
// enqueued, due at max(now, deletion_scheduled_at). Idempotent via the intent
// idempotency_key.
func TestMarkerConversionSweepEnqueuesIntents(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	now := time.Now().UTC()
	futureDelete := now.Add(2 * time.Hour)
	pastDelete := now.Add(-48 * time.Hour)

	// Marker with a still-open undo window: the intent must be due AT the
	// marker time (undo window honored).
	futureSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              uuid.New().String(),
		PriceID:             priceID,
		Status:              models.StatusCancelled,
		Processor:           models.ProcessorMobius,
		DeletionScheduledAt: &futureDelete,
	})

	// Overdue marker (e.g. kill switch was on for two days): due at ~now.
	pastSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              uuid.New().String(),
		PriceID:             priceID,
		Status:              models.StatusCancelled,
		Processor:           models.ProcessorMobius,
		DeletionScheduledAt: &pastDelete,
	})

	converted, err := suite.App.Runtime.ConvertDeferredDeleteMarkersToIntents(ctx)
	require.NoError(t, err)
	// Other tests in the shared suite may have left markers too.
	assert.GreaterOrEqual(t, converted, 2, "both seeded markers should be converted")

	// Future marker: one intent due at the marker time.
	count, status, nextAt := queryNMIDeleteIntents(t, suite, futureSub.ID)
	require.Equal(t, 1, count, "exactly one delete intent for the future-marker subscription")
	assert.Equal(t, intents.StatusPending, status)
	assert.WithinDuration(t, futureDelete, nextAt, time.Minute)

	// Past marker: one intent due at ~now (clamped, never in the past).
	count, _, nextAt = queryNMIDeleteIntents(t, suite, pastSub.ID)
	require.GreaterOrEqual(t, count, 1, "a delete intent exists for the overdue-marker subscription")
	assert.True(t, nextAt.After(pastDelete), "overdue delete is clamped to now, not the stale marker time")
	assert.WithinDuration(t, now, nextAt, time.Minute)

	// Idempotency: a second sweep must not stack a duplicate intent
	// (idempotency_key conflict refreshes the pending row).
	_, err = suite.App.Runtime.ConvertDeferredDeleteMarkersToIntents(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, countLiveNMIDeleteIntents(t, suite, futureSub.ID), "sweep is idempotent for pending intents")
	assert.Equal(t, 1, countLiveNMIDeleteIntents(t, suite, pastSub.ID))
}

// TestFailMembershipDunningExhaustionSchedulesNMIDelete verifies the #344
// follow-up webhook-path fix end-to-end via the runtime's shared lifecycle
// service (the instance the NMI webhook handlers use): a past_due NMI-backed
// subscription within the dunning window whose 5th failure exhausts dunning is
// cancelled AND a deferred remote-delete intent is enqueued on the ledger
// (#358), so NMI stops retrying the dead subscription.
func TestFailMembershipDunningExhaustionSchedulesNMIDelete(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	recentPeriodEnd := time.Now().Add(-2 * 24 * time.Hour)    // within the derived monthly dunning window (#359)
	retryAttempts := subscriptions.DunningMaxFailures(30) - 1 // the next failure is the 5th = terminal

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

	reason := "rebill declined"
	err := suite.App.Runtime.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Processor:      models.ProcessorMobius,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	})
	require.NoError(t, err)

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status, "5th dunning failure must cancel")
	require.NotNil(t, updated.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *updated.CancelType)

	// The delete intent was enqueued synchronously before FailMembership
	// returned, due ~now, system-origin (dunning exhaustion is proactive).
	count, status, _ := queryNMIDeleteIntents(t, suite, sub.ID)
	require.GreaterOrEqual(t, count, 1, "a deferred NMI delete intent must be enqueued on dunning exhaustion")

	var origin string
	require.NoError(t, suite.Pool.QueryRow(ctx, `
		SELECT origin FROM openrails.provider_intents
		WHERE intent_type = $1 AND subscription_id = $2`,
		intents.TypeNMIDeleteSubscription, sub.ID).Scan(&origin))
	assert.Equal(t, string(intents.OriginSystem), origin, "dunning-exhaustion deletes are system-origin")

	// The durable marker was persisted with the cancellation. The scheduled
	// intent executor may already have picked the intent up; the marker is
	// only legitimately cleared once the intent succeeded.
	if updated.DeletionScheduledAt == nil {
		assert.Equal(t, intents.StatusSucceeded, status, "marker may only be cleared by a succeeded delete intent")
	} else {
		assert.WithinDuration(t, time.Now(), *updated.DeletionScheduledAt, time.Minute)
	}
}

// TestFailMembershipExhaustionSetsDurableMarkerViaScheduler pins down the
// deterministic pieces with an injected recording scheduler: the marker is
// persisted in the same transaction as the cancellation, and the scheduler is
// invoked exactly once after commit with the subscription's user and ~now.
func TestFailMembershipExhaustionSetsDurableMarkerViaScheduler(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	rt := suite.App.Runtime

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pastRetry := time.Now().Add(-1 * time.Hour)
	retryAttempts := subscriptions.DunningMaxFailures(30) - 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:        userID,
		PriceID:       priceID,
		Status:        models.StatusPastDue,
		Processor:     models.ProcessorMobius,
		RetryAttempts: &retryAttempts,
		NextRetryAt:   &pastRetry,
	})

	recorder := &recordingDeferredDeleteScheduler{}
	lifecycle := subscriptions.NewSubscriptionLifecycleService(
		rt.DB, rt.ProductService, rt.PriceService, rt.EntitlementService, nil, rt.PaymentService, nil)
	lifecycle.SetConfig(rt.Config)
	lifecycle.SetDeferredDeleteScheduler(recorder)

	reason := "rebill declined"
	require.NoError(t, lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Processor:      models.ProcessorMobius,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}))

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status)
	require.NotNil(t, updated.DeletionScheduledAt, "durable deletion marker must persist with the cancellation")
	assert.WithinDuration(t, time.Now(), *updated.DeletionScheduledAt, time.Minute)

	require.Len(t, recorder.scheduled, 1, "exactly one deferred delete scheduled")
	assert.Equal(t, sub.ID, recorder.scheduled[0].SubscriptionID)
	assert.Equal(t, updated.CustomerID.String(), recorder.scheduled[0].UserID)
	assert.WithinDuration(t, time.Now(), recorder.scheduled[0].RunAt, time.Minute)
}

// TestFailMembershipLimitedModeLeavesRemoteSubscription verifies the #345 gate:
// in limited mode a terminal dunning failure still cancels locally, but takes
// NO proactive provider action — no deferred delete is scheduled and no marker
// is set (the remote subscription is left for reconciliation).
func TestFailMembershipLimitedModeLeavesRemoteSubscription(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	rt := suite.App.Runtime

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pastRetry := time.Now().Add(-1 * time.Hour)
	retryAttempts := subscriptions.DunningMaxFailures(30) - 1

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:        userID,
		PriceID:       priceID,
		Status:        models.StatusPastDue,
		Processor:     models.ProcessorMobius,
		RetryAttempts: &retryAttempts,
		NextRetryAt:   &pastRetry,
	})

	limitedCfg := *rt.Config
	limitedCfg.Mode = config.ModeLimited

	recorder := &recordingDeferredDeleteScheduler{}
	lifecycle := subscriptions.NewSubscriptionLifecycleService(
		rt.DB, rt.ProductService, rt.PriceService, rt.EntitlementService, nil, rt.PaymentService, nil)
	lifecycle.SetConfig(&limitedCfg)
	lifecycle.SetDeferredDeleteScheduler(recorder)

	reason := "rebill declined"
	require.NoError(t, lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Processor:      models.ProcessorMobius,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}))

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status, "local lifecycle proceeds in limited mode")
	assert.Nil(t, updated.DeletionScheduledAt, "limited mode must not stamp a proactive deletion marker")
	assert.Empty(t, recorder.scheduled, "limited mode must not schedule a remote delete")

	count, _, _ := queryNMIDeleteIntents(t, suite, sub.ID)
	assert.Zero(t, count, "no delete intent may exist for the limited-mode cancellation")
}

// TestDunningWorkerWindowExpirySchedulesDeferredDelete closes the #344 tail:
// the dunning worker's terminal cancellations route the processor-side
// delete through the shared deferred scheduler (no inline delete), so window
// expiry persists the durable marker and schedules exactly one delete.
func TestDunningWorkerWindowExpirySchedulesDeferredDelete(t *testing.T) {
	suite := getSharedTestSuite(t)

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

	recorder := &recordingDeferredDeleteScheduler{}
	worker := &riverjobs.DunningWorker{
		DB:          suite.App.Runtime.DB,
		Config:      suite.App.Runtime.Config,
		NMIClients:  map[string]*nmi.NMIClient{string(models.ProcessorMobius): {}},
		DeferDelete: recorder,
	}

	err := worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{}})
	require.NoError(t, err)

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status)
	require.NotNil(t, updated.DeletionScheduledAt, "terminal window-expiry cancel must persist the deferred-delete marker")

	var scheduledForSub []scheduledDelete
	for _, scheduled := range recorder.scheduled {
		if scheduled.SubscriptionID == sub.ID {
			scheduledForSub = append(scheduledForSub, scheduled)
		}
	}
	require.Len(t, scheduledForSub, 1, "exactly one deferred delete scheduled for this subscription (no inline delete)")
}

// TestUserCancelEnqueuesUserOriginIntentAndResumeSupersedes covers the
// public-behavior contract end-to-end on the ledger: a user cancel with an
// open undo window enqueues a USER-origin intent due at deleteAt (period end
// minus the safety margin), and the resume worker supersedes it.
func TestUserCancelEnqueuesUserOriginIntentAndResumeSupersedes(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	rt := suite.App.Runtime

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	periodEnd := time.Now().Add(20 * 24 * time.Hour).UTC()

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusActive,
		Processor:           models.ProcessorMobius,
		CurrentPeriodEndsAt: &periodEnd,
	})

	require.NoError(t, rt.UserSubscriptionService.CancelUserSubscription(ctx, userID, "changed my mind"))

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status)
	require.NotNil(t, updated.DeletionScheduledAt, "undo-window cancel defers the delete")
	deleteAt, deferred := subscriptions.NMIDeferredDeleteAt(updated, time.Now().UTC())
	_ = deferred // marker already proves deferral; deleteAt pins the schedule

	count, status, nextAt := queryNMIDeleteIntents(t, suite, sub.ID)
	require.Equal(t, 1, count, "user cancel enqueues exactly one delete intent")
	assert.Equal(t, intents.StatusPending, status)
	assert.WithinDuration(t, *updated.DeletionScheduledAt, nextAt, time.Minute, "intent due at deleteAt (undo window honored)")
	if deferred {
		assert.WithinDuration(t, deleteAt, nextAt, time.Minute)
	}

	var origin string
	require.NoError(t, suite.Pool.QueryRow(ctx, `
		SELECT origin FROM openrails.provider_intents
		WHERE intent_type = $1 AND subscription_id = $2`,
		intents.TypeNMIDeleteSubscription, sub.ID).Scan(&origin))
	assert.Equal(t, string(intents.OriginUser), origin, "user cancels are user-origin (execute under limited)")

	// Resume within the window: the worker supersedes the intent and
	// reactivates the subscription.
	resumeWorker := &riverjobs.ResumeSubscriptionWorker{
		DB:                           rt.DB,
		Config:                       rt.Config,
		EntitlementService:           rt.EntitlementService,
		SubscriptionService:          rt.SubscriptionService,
		SubscriptionLifecycleService: rt.SubscriptionLifecycleService,
		NMIClients:                   rt.NMIClients,
	}
	require.NoError(t, resumeWorker.Work(ctx, &river.Job[riverjobs.ResumeSubscriptionArgs]{
		Args: riverjobs.ResumeSubscriptionArgs{UserID: userID, SubscriptionID: sub.ID},
	}))

	resumed := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusActive, resumed.Status, "resume reactivates")
	assert.Nil(t, resumed.DeletionScheduledAt, "resume clears the marker")

	count, status, _ = queryNMIDeleteIntents(t, suite, sub.ID)
	require.Equal(t, 1, count)
	assert.Equal(t, intents.StatusSuperseded, status, "resume supersedes the pending delete intent")
	assert.Zero(t, countLiveNMIDeleteIntents(t, suite, sub.ID))
}
