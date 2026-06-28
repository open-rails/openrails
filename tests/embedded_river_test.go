//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// TestBillingWorkersProcessJobs tests that billing workers can process jobs
func TestBillingWorkersProcessJobs(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	suite.ClearJobQueue()

	t.Run("cleanup job completes successfully", func(t *testing.T) {
		client := suite.GetRiverClient()
		require.NotNil(t, client)

		initialCompleted := suite.GetCompletedJobCount()

		// Enqueue a cleanup job (one of billing's workers)
		_, err := client.Insert(ctx, riverjobs.CleanupExpiredDataArgs{}, &river.InsertOpts{
			Queue: riverjobs.QueueBilling,
		})
		require.NoError(t, err)

		// Wait for job to complete
		completed := suite.WaitForJobCompletion(initialCompleted+1, 5*time.Second)
		assert.True(t, completed, "Cleanup job should complete within timeout")
	})

	t.Run("credit expiry job completes successfully", func(t *testing.T) {
		client := suite.GetRiverClient()
		require.NotNil(t, client)

		initialCompleted := suite.GetCompletedJobCount()

		// Enqueue a credit expiry job
		_, err := client.Insert(ctx, riverjobs.CreditExpiryArgs{}, &river.InsertOpts{
			Queue: riverjobs.QueueBilling,
		})
		require.NoError(t, err)

		// Wait for job to complete
		completed := suite.WaitForJobCompletion(initialCompleted+1, 5*time.Second)
		assert.True(t, completed, "Credit expiry job should complete within timeout")
	})
}
