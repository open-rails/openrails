//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// queryNMIDeleteJobs returns (count, state, scheduledAt) for deferred NMI
// delete jobs matching the subscription, across all job states.
func queryNMIDeleteJobs(t *testing.T, suite *TestContainerSuite, subID uuid.UUID) (count int, state string, scheduledAt time.Time) {
	t.Helper()
	ctx := context.Background()
	err := suite.Pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COALESCE(MAX(state::text), ''),
		       COALESCE(MAX(scheduled_at), '0001-01-01'::timestamptz)
		FROM billing.river_job
		WHERE kind = $1 AND args->>'subscription_id' = $2`,
		riverjobs.KindSubscriptionNMIDelete, subID.String()).Scan(&count, &state, &scheduledAt)
	require.NoError(t, err)
	return count, state, scheduledAt
}

// countLiveNMIDeleteJobs counts not-yet-finalized deferred delete jobs for the
// subscription (the states the scheduler's UniqueOpts deduplicate across).
func countLiveNMIDeleteJobs(t *testing.T, suite *TestContainerSuite, subID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	var count int
	err := suite.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM billing.river_job
		WHERE kind = $1 AND args->>'subscription_id' = $2
		  AND state IN ('available', 'pending', 'running', 'retryable', 'scheduled')`,
		riverjobs.KindSubscriptionNMIDelete, subID.String()).Scan(&count)
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

// TestBootRescanReenqueuesPendingDeferredDeletes verifies the #344 follow-up
// boot rescan: cancelled subscriptions whose DeletionScheduledAt marker
// survived (e.g. the deletion kill switch skipped the remote delete) get their
// deferred NMI delete job re-enqueued on worker startup, at
// max(now, deletion_scheduled_at).
func TestBootRescanReenqueuesPendingDeferredDeletes(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	now := time.Now().UTC()
	futureDelete := now.Add(2 * time.Hour)
	pastDelete := now.Add(-48 * time.Hour)

	// Marker with a still-open undo window: must be re-enqueued AT the marker time.
	futureSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              uuid.New().String(),
		PriceID:             priceID,
		Status:              models.StatusCancelled,
		Processor:           models.ProcessorMobius,
		DeletionScheduledAt: &futureDelete,
	})

	// Overdue marker (e.g. kill switch was on for two days): re-enqueued at ~now.
	pastSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              uuid.New().String(),
		PriceID:             priceID,
		Status:              models.StatusCancelled,
		Processor:           models.ProcessorMobius,
		DeletionScheduledAt: &pastDelete,
	})

	reenqueued, err := suite.App.Runtime.RescanPendingDeferredDeletes(ctx)
	require.NoError(t, err)
	// Other tests in the shared suite may have left markers too.
	assert.GreaterOrEqual(t, reenqueued, 2, "both seeded markers should be re-enqueued")

	// Future marker: a scheduled job exists at the marker time (undo window honored).
	count, state, scheduledAt := queryNMIDeleteJobs(t, suite, futureSub.ID)
	require.Equal(t, 1, count, "exactly one delete job for the future-marker subscription")
	assert.Equal(t, "scheduled", state)
	assert.WithinDuration(t, futureDelete, scheduledAt, time.Minute)

	// Past marker: a job exists scheduled at ~now (clamped, never in the past).
	count, _, scheduledAt = queryNMIDeleteJobs(t, suite, pastSub.ID)
	require.GreaterOrEqual(t, count, 1, "a delete job exists for the overdue-marker subscription")
	assert.True(t, scheduledAt.After(pastDelete), "overdue delete is clamped to now, not the stale marker time")
	assert.WithinDuration(t, now, scheduledAt, time.Minute)

	// Idempotency: a second rescan must not stack a duplicate job for the
	// still-scheduled future delete (river.UniqueOpts ByArgs).
	_, err = suite.App.Runtime.RescanPendingDeferredDeletes(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, countLiveNMIDeleteJobs(t, suite, futureSub.ID), "rescan is idempotent for pending jobs")
}

// TestFailMembershipDunningExhaustionSchedulesNMIDelete verifies the #344
// follow-up webhook-path fix end-to-end via the runtime's shared lifecycle
// service (the instance the NMI webhook handlers use): a past_due NMI-backed
// subscription within the dunning window whose 5th failure exhausts dunning is
// cancelled AND a deferred remote-delete job is scheduled (so NMI stops
// retrying the dead subscription).
func TestFailMembershipDunningExhaustionSchedulesNMIDelete(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	recentPeriodEnd := time.Now().Add(-2 * 24 * time.Hour) // within the derived monthly dunning window (#359)
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

	// The deferred delete job was enqueued synchronously before FailMembership
	// returned (scheduled at ~now).
	count, state, _ := queryNMIDeleteJobs(t, suite, sub.ID)
	require.GreaterOrEqual(t, count, 1, "a deferred NMI delete job must be scheduled on dunning exhaustion")

	// The durable marker was persisted with the cancellation. The in-process
	// delete worker may already have picked the job up (it runs at "now"); the
	// marker is only legitimately cleared once the job finalized successfully.
	if updated.DeletionScheduledAt == nil {
		assert.Equal(t, "completed", state, "marker may only be cleared by a finalized delete job")
	} else {
		assert.WithinDuration(t, time.Now(), *updated.DeletionScheduledAt, time.Minute)
	}
}

// TestFailMembershipExhaustionSetsDurableMarkerViaScheduler pins down the
// deterministic pieces with an injected recording scheduler: the marker is
// persisted in the same transaction as the cancellation, and the scheduler is
// invoked exactly once after commit with the subscription's user and ~now.
func TestFailMembershipExhaustionSetsDurableMarkerViaScheduler(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()
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
	assert.Equal(t, updated.TenantSubjectID.String(), recorder.scheduled[0].UserID)
	assert.WithinDuration(t, time.Now(), recorder.scheduled[0].RunAt, time.Minute)
}

// TestFailMembershipLimitedModeLeavesRemoteSubscription verifies the #345 gate:
// in limited mode a terminal dunning failure still cancels locally, but takes
// NO proactive provider action — no deferred delete is scheduled and no marker
// is set (the remote subscription is left for reconciliation).
func TestFailMembershipLimitedModeLeavesRemoteSubscription(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()
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

	count, _, _ := queryNMIDeleteJobs(t, suite, sub.ID)
	assert.Zero(t, count, "no delete job may exist for the limited-mode cancellation")
}

// TestDunningWorkerWindowExpirySchedulesDeferredDelete closes the #344 tail:
// the dunning worker's terminal cancellations now route the processor-side
// delete through the shared deferred scheduler (no inline delete), so window
// expiry persists the durable marker and schedules exactly one delete job.
func TestDunningWorkerWindowExpirySchedulesDeferredDelete(t *testing.T) {
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

	require.Len(t, recorder.scheduled, 1, "exactly one deferred delete scheduled (no inline delete)")
	require.Equal(t, sub.ID, recorder.scheduled[0].SubscriptionID)
}
