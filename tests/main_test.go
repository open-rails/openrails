//go:build integration

package tests

import (
	"os"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain runs the integration suite and then tears down the process-shared
// infra. CleanupSharedSuite handles the legacy full-stack suite's Redis and
// ClickHouse containers; dbtest.TerminateShared handles the shared Postgres
// container used by both harnesses.
func TestMain(m *testing.M) {
	// pgx decodes timestamptz into time.Local, whereas the bun/database-sql era
	// returned UTC. Pin the process zone to UTC so time.Time equality
	// assertions against UTC-constructed expectations keep working unchanged.
	time.Local = time.UTC

	code := m.Run()
	CleanupSharedSuite()
	dbtest.TerminateShared()
	os.Exit(code)
}
