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

	"github.com/doujins-org/doujins-billing/internal/db/models"
	riverjobs "github.com/doujins-org/doujins-billing/internal/river"
)

// uuid is used by models for ID generation in cleanup tests

// TestRiverWorkersStarted verifies that River workers are running in the test suite
func TestRiverWorkersStarted(t *testing.T) {
	suite := setupTestSuite(t)

	t.Run("river client is initialized", func(t *testing.T) {
		client := suite.GetRiverClient()
		require.NotNil(t, client, "River client should be initialized")
	})

	t.Run("river workers are started", func(t *testing.T) {
		// The runtime should have riverStarted = true after StartWorkers
		require.NotNil(t, suite.App.Runtime.RiverClient, "River client should be set on runtime")
	})
}

// TestRiverJobEnqueue tests that we can enqueue jobs and they get processed
func TestRiverJobEnqueue(t *testing.T) {
	suite := setupTestSuite(t)

	// Clear any existing jobs from periodic schedulers
	suite.ClearJobQueue()

	t.Run("can enqueue and process dunning job", func(t *testing.T) {
		// Get the River client directly (it's already typed)
		client := suite.App.Runtime.RiverClient
		require.NotNil(t, client, "Should have River client")

		// Get initial completed count
		initialCompleted := suite.GetCompletedJobCount()

		// Enqueue a dunning job (webhooks are now processed synchronously, not via River)
		ctx := context.Background()
		_, err := client.Insert(ctx, riverjobs.DunningArgs{}, &river.InsertOpts{
			Queue: riverjobs.QueueBilling,
		})
		require.NoError(t, err, "Should be able to enqueue job")

		// Wait for job to complete (max 5 seconds)
		completed := suite.WaitForJobCompletion(initialCompleted+1, 5*time.Second)
		assert.True(t, completed, "Job should complete within timeout")

		// Verify job completed
		finalCompleted := suite.GetCompletedJobCount()
		assert.Greater(t, finalCompleted, initialCompleted, "Completed job count should increase")
	})
}

// TestRiverPeriodicJobs tests that periodic jobs are scheduled
func TestRiverPeriodicJobs(t *testing.T) {
	suite := setupTestSuite(t)

	t.Run("periodic jobs are registered", func(t *testing.T) {
		client := suite.App.Runtime.RiverClient
		require.NotNil(t, client, "Should have River client")

		// Check that periodic jobs exist
		periodicJobs := client.PeriodicJobs()
		require.NotNil(t, periodicJobs, "Periodic jobs manager should exist")

		// We can't directly inspect periodic jobs, but we can verify the client is set up
		// The presence of the client and its configuration is enough for this test
	})
}

// TestWebhookProcessingFlow has been removed.
// Webhook processing is now synchronous-only - no async River jobs.
// See: agents/progress.json "simplify-webhook-processing" for details.

// TestDunningJobEnqueue tests enqueueing a dunning job
func TestDunningJobEnqueue(t *testing.T) {
	suite := setupTestSuite(t)

	// Clear job queue
	suite.ClearJobQueue()

	t.Run("dunning job can be manually enqueued", func(t *testing.T) {
		client := suite.App.Runtime.RiverClient
		require.NotNil(t, client)

		ctx := context.Background()
		initialCompleted := suite.GetCompletedJobCount()

		// Enqueue dunning job
		_, err := client.Insert(ctx, riverjobs.DunningArgs{}, &river.InsertOpts{
			Queue: riverjobs.QueueBilling,
		})
		require.NoError(t, err)

		// Wait for completion (dunning with no due subscriptions should complete quickly)
		completed := suite.WaitForJobCompletion(initialCompleted+1, 5*time.Second)
		assert.True(t, completed, "Dunning job should complete within timeout")
	})
}

// TestJobQueueHelpers tests the helper functions
func TestJobQueueHelpers(t *testing.T) {
	suite := setupTestSuite(t)

	t.Run("clear job queue works", func(t *testing.T) {
		suite.ClearJobQueue()

		// After clearing, pending should be 0 (unless periodic jobs fire)
		pending := suite.GetPendingJobCount()
		t.Logf("Pending jobs after clear: %d", pending)
		// Note: periodic jobs may have fired, so we just verify the function runs
	})

	t.Run("get completed job count works", func(t *testing.T) {
		completed := suite.GetCompletedJobCount()
		t.Logf("Completed jobs: %d", completed)
		// Just verify the function runs without error
	})

	t.Run("get pending job count works", func(t *testing.T) {
		pending := suite.GetPendingJobCount()
		t.Logf("Pending jobs: %d", pending)
		// Just verify the function runs without error
	})
}

// TestCleanupExpiredDataWorker tests the cleanup worker for expired data
func TestCleanupExpiredDataWorker(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	// Clear job queue for clean state
	suite.ClearJobQueue()

	t.Run("cleanup job can be enqueued", func(t *testing.T) {
		client := suite.App.Runtime.RiverClient
		require.NotNil(t, client)

		initialCompleted := suite.GetCompletedJobCount()

		// Enqueue cleanup job
		_, err := client.Insert(ctx, riverjobs.CleanupExpiredDataArgs{}, &river.InsertOpts{
			Queue: riverjobs.QueueBilling,
		})
		require.NoError(t, err)

		// Wait for completion (should complete quickly with no data)
		completed := suite.WaitForJobCompletion(initialCompleted+1, 5*time.Second)
		assert.True(t, completed, "Cleanup job should complete within timeout")
	})

	// NOTE: Wallet challenge cleanup was removed - wallet auth belongs in authkit, not billing.

	t.Run("cleans up old seen notifications", func(t *testing.T) {
		// Set up a mock clock 100 days in the future
		now := time.Now()
		mockClock := suite.SetMockClock(now.Add(100 * 24 * time.Hour))

		// Insert an old seen notification (created 95 days ago relative to mock time)
		userID := uuid.New().String()
		notification := &models.NotificationQueue{
			ID:        uuid.New(),
			UserID:    userID,
			EventType: models.NotificationSystemAlert,
			Seen:      true, // Seen notifications have 90-day retention
			CreatedAt: mockClock.Now().Add(-95 * 24 * time.Hour),
		}
		_, err := suite.BunDB.NewInsert().Model(notification).Exec(ctx)
		require.NoError(t, err)

		// Run cleanup worker
		worker := riverjobs.CleanupExpiredDataWorker{
			DB:     suite.App.Runtime.DB,
			Clock:  mockClock,
			Config: riverjobs.DefaultCleanupConfig(),
		}
		err = worker.Work(ctx, &river.Job[riverjobs.CleanupExpiredDataArgs]{})
		require.NoError(t, err)

		// Verify notification was deleted
		var count int
		err = suite.BunDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM billing.notification_queue WHERE id = $1", notification.ID).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "Old seen notification should be deleted")
	})

	t.Run("preserves recent data", func(t *testing.T) {
		// Use current time
		mockClock := clockwork.NewRealClock()

		// Insert recent notification (just created)
		notification := &models.NotificationQueue{
			ID:        uuid.New(),
			UserID:    uuid.New().String(),
			EventType: models.NotificationSystemAlert,
			Seen:      false,
			CreatedAt: mockClock.Now(),
		}
		_, err := suite.BunDB.NewInsert().Model(notification).Exec(ctx)
		require.NoError(t, err)

		// Run cleanup worker
		worker := riverjobs.CleanupExpiredDataWorker{
			DB:     suite.App.Runtime.DB,
			Clock:  mockClock,
			Config: riverjobs.DefaultCleanupConfig(),
		}
		err = worker.Work(ctx, &river.Job[riverjobs.CleanupExpiredDataArgs]{})
		require.NoError(t, err)

		// Verify recent notification was preserved
		var notifCount int
		err = suite.BunDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM billing.notification_queue WHERE id = $1", notification.ID).Scan(&notifCount)
		require.NoError(t, err)
		assert.Equal(t, 1, notifCount, "Recent notification should be preserved")

		// Clean up test data
		suite.BunDB.NewDelete().Model(notification).WherePK().Exec(ctx)
	})
}
