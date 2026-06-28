//go:build integration

package tests

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestSharedPostgresHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping container tests in short mode")
	}

	if !usesExternalPostgres() && !isDockerAvailable() {
		t.Skip("Docker is not available, skipping container tests")
	}

	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)

	var one int
	err := pool.QueryRow(ctx, "SELECT 1").Scan(&one)
	require.NoError(t, err, "shared Postgres should accept queries")
	assert.Equal(t, 1, one)

	dbtest.EnsureTestMerchant(ctx, t, pool)
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

func usesExternalPostgres() bool {
	return strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_URL")) != "" ||
		strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")) != ""
}
