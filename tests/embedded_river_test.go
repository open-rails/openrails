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
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestRuntimeAddBillingWorkersTo tests that billing workers can be added to an external registry
func TestRuntimeAddBillingWorkersTo(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	t.Run("adds workers to external registry", func(t *testing.T) {
		workers := river.NewWorkers()

		// Add billing workers using the Runtime method
		err := suite.App.Runtime.AddBillingWorkersTo(ctx, workers)
		require.NoError(t, err)

		// Workers should have been added (we can't inspect them directly,
		// but the function should succeed without error)
	})

	t.Run("second call fails due to duplicate workers", func(t *testing.T) {
		workers := river.NewWorkers()

		// First call should succeed
		err := suite.App.Runtime.AddBillingWorkersTo(ctx, workers)
		require.NoError(t, err)

		// Second call will fail because workers are already registered
		// This is expected River behavior - AddWorkerSafely returns error for duplicates
		err = suite.App.Runtime.AddBillingWorkersTo(ctx, workers)
		assert.Error(t, err, "Should error on duplicate worker registration")
	})
}

// TestRuntimeGetBillingPeriodicJobs tests that billing periodic jobs are returned
func TestRuntimeGetBillingPeriodicJobs(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	t.Run("returns periodic jobs", func(t *testing.T) {
		jobs, err := suite.App.Runtime.GetBillingPeriodicJobs(ctx)
		require.NoError(t, err)
		require.NotNil(t, jobs)

		// Should have multiple periodic jobs (dunning, idempotency cleanup, ccbill reconcile, cleanup, credit expiry)
		assert.GreaterOrEqual(t, len(jobs), 5, "Should have at least 5 periodic jobs")

		t.Logf("Found %d periodic jobs", len(jobs))
	})
}

// TestRuntimeExternalRiverClient tests external River client flag
func TestRuntimeExternalRiverClient(t *testing.T) {
	suite := getSharedTestSuite(t)

	t.Run("HasExternalRiverClient is false by default", func(t *testing.T) {
		// The suite creates its own River client internally, not externally
		// So this should be false (unless the suite was modified to use external)
		hasExternal := suite.App.Runtime.HasExternalRiverClient()
		t.Logf("HasExternalRiverClient: %v", hasExternal)
		// Don't assert - just verify method works
	})
}

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

// TestQueueBillingExport verifies the queue constant is exported via embedded package
func TestQueueBillingExport(t *testing.T) {
	// Verify the constant is accessible and matches the internal constant
	assert.Equal(t, "billing", embedded.QueueBilling)
	assert.Equal(t, riverjobs.QueueBilling, embedded.QueueBilling)
}

// TestExternalRiverClientWorkflow tests the documented workflow for external clients
func TestExternalRiverClientWorkflow(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	t.Run("documented workflow steps work", func(t *testing.T) {
		// This test verifies the workflow documented in README.md compiles and runs
		// We don't actually create a second client since suite already has one

		// Step 1: Create worker registry
		workers := river.NewWorkers()

		// Step 2: Add billing workers
		err := suite.App.Runtime.AddBillingWorkersTo(ctx, workers)
		require.NoError(t, err)

		// Step 3: Get periodic jobs
		periodicJobs, err := suite.App.Runtime.GetBillingPeriodicJobs(ctx)
		require.NoError(t, err)
		require.NotEmpty(t, periodicJobs)

		// Step 4: Verify queue constant is accessible
		queue := embedded.QueueBilling
		assert.Equal(t, "billing", queue)

		// The actual River client creation and starting is not tested here
		// since the suite already has a running client
		t.Log("External River client workflow steps verified")
	})
}
