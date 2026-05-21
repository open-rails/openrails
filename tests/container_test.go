//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestPostgresContainer tests PostgreSQL container setup
func TestPostgresContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping container tests in short mode")
	}

	// Check if Docker is available
	if !isDockerAvailable() {
		t.Skip("Docker is not available, skipping container tests")
	}

	ctx := context.Background()

	t.Run("Start PostgreSQL Container", func(t *testing.T) {
		container, err := postgres.Run(ctx,
			"postgres:18-alpine",
			postgres.WithDatabase("test_db"),
			postgres.WithUsername("test_user"),
			postgres.WithPassword("test_password"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(30*time.Second),
			),
		)
		require.NoError(t, err, "Should start PostgreSQL container")
		defer func() {
			if err := container.Terminate(ctx); err != nil {
				t.Logf("Failed to terminate container: %v", err)
			}
		}()

		// Test container is running
		state, err := container.State(ctx)
		require.NoError(t, err, "Should get container state")
		assert.True(t, state.Running, "Container should be running")

		// Test connection string
		connStr, err := container.ConnectionString(ctx, "sslmode=disable")
		require.NoError(t, err, "Should get connection string")
		assert.Contains(t, connStr, "postgres://", "Should have postgres connection string")
		assert.Contains(t, connStr, "test_user", "Should contain username")
		assert.Contains(t, connStr, "test_db", "Should contain database name")

		t.Logf("PostgreSQL container started successfully with connection: %s", connStr)
	})
}

// isDockerAvailable checks if Docker daemon is accessible
func isDockerAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try to create a minimal container provider - this checks Docker connectivity
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return false
	}

	// Check if we can ping the Docker daemon
	return provider.Health(ctx) == nil
}
