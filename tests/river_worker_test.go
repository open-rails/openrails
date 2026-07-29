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
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// uuid is used by models for ID generation in cleanup tests

// TestWebhookProcessingFlow has been removed.
// Webhook processing is now synchronous-only - no async River jobs.
// See: agents/progress.json "simplify-webhook-processing" for details.

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

	// NOTE: Wallet challenge cleanup was removed - wallet auth belongs in authkit, not openrails.

	t.Run("cleans up old seen notifications", func(t *testing.T) {
		// Set up a mock clock 100 days in the future
		now := time.Now()
		mockClock := suite.SetMockClock(now.Add(100 * 24 * time.Hour))

		// Insert an old seen notification (created 95 days ago relative to mock time)
		userID := uuid.New().String()
		notification := &models.NotificationQueue{
			ID:         uuid.New(),
			CustomerID: suite.ensureCustomer(ctx, userID),
			EventType:  models.NotificationSystemAlert,
			Seen:       true, // Seen notifications have 90-day retention
			CreatedAt:  mockClock.Now().Add(-95 * 24 * time.Hour),
		}
		suite.InsertNotification(ctx, notification)

		// Run cleanup worker
		worker := riverjobs.CleanupExpiredDataWorker{
			DB:     suite.App.Runtime.DB,
			Clock:  mockClock,
			Config: riverjobs.DefaultCleanupConfig(),
		}
		err := worker.Work(suite.WorkerCtx(), &river.Job[riverjobs.CleanupExpiredDataArgs]{})
		require.NoError(t, err)

		// Verify notification was deleted
		count := suite.Count(ctx, "SELECT COUNT(*) FROM openrails.notification_queue WHERE id = $1", notification.ID)
		assert.Equal(t, 0, count, "Old seen notification should be deleted")
	})

	t.Run("preserves recent data", func(t *testing.T) {
		// Use current time
		mockClock := clockwork.NewRealClock()

		// Insert recent notification (just created)
		userID := uuid.New().String()
		notification := &models.NotificationQueue{
			ID:         uuid.New(),
			CustomerID: suite.ensureCustomer(ctx, userID),
			EventType:  models.NotificationSystemAlert,
			Seen:       false,
			CreatedAt:  mockClock.Now(),
		}
		suite.InsertNotification(ctx, notification)

		// Run cleanup worker
		worker := riverjobs.CleanupExpiredDataWorker{
			DB:     suite.App.Runtime.DB,
			Clock:  mockClock,
			Config: riverjobs.DefaultCleanupConfig(),
		}
		err := worker.Work(suite.WorkerCtx(), &river.Job[riverjobs.CleanupExpiredDataArgs]{})
		require.NoError(t, err)

		// Verify recent notification was preserved
		notifCount := suite.Count(ctx, "SELECT COUNT(*) FROM openrails.notification_queue WHERE id = $1", notification.ID)
		assert.Equal(t, 1, notifCount, "Recent notification should be preserved")

		// Clean up test data
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE id = $1", notification.ID)
	})
}
