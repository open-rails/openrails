//go:build integration

package tests

import (
	"os"
	"testing"
	"time"
)

// TestMain runs the integration suite and then tears down the process-shared
// TestContainerSuite (Postgres + Redis + ClickHouse). Without this, the
// sync.Once shared suite created by getSharedTestSuite was never terminated and
// leaked its containers whenever the testcontainers Ryuk reaper was unavailable
// (offline/sandboxed runs). CleanupSharedSuite was defined for exactly this
// purpose but had no caller until now.
func TestMain(m *testing.M) {
	// pgx decodes timestamptz into time.Local, whereas the bun/database-sql era
	// returned UTC. Pin the process zone to UTC so time.Time equality
	// assertions against UTC-constructed expectations keep working unchanged.
	time.Local = time.UTC

	code := m.Run()
	CleanupSharedSuite()
	os.Exit(code)
}
