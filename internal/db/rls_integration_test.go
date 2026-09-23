//go:build integration

package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Shared PostgreSQL fixtures for connection/session scope tests. The probe
// table has no RLS; tenant selection is explicit in each query.

var (
	rlsTenantA = mustID("00000000-0000-0000-0000-0000000000a1")
	rlsTenantB = mustID("00000000-0000-0000-0000-0000000000b2")
)

func mustID(s string) merchant.ID {
	id, err := merchant.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// newDBRetry opens a *db.DB, retrying transient connect failures. Busy/CI Docker
// hosts intermittently drop bridge packets (i/o timeout) on connect; the test
// logic is unaffected, so a few retries keep the suite from flaking.
func newDBRetry(t *testing.T, dsn string) *db.DB {
	t.Helper()
	var result *db.DB
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		database, err := db.NewDB(t.Context(), &config.DBConfig{URL: dsn})
		assert.NoError(collect, err)
		if err == nil {
			result = database
		}
	}, 16*time.Second, 250*time.Millisecond, "connect to the RLS test database")
	return result
}

const rlsSetupDDL = `
CREATE SCHEMA IF NOT EXISTS billing;
CREATE TABLE IF NOT EXISTS billing.rls_probe (
    id        UUID PRIMARY KEY,
    merchant_id UUID NOT NULL,
    val       TEXT NOT NULL
);
	GRANT USAGE ON SCHEMA billing TO openrails_app;
	GRANT SELECT, INSERT, UPDATE, DELETE ON billing.rls_probe TO openrails_app;
	`

// The package-owned database and cluster-role lock are shared with the other
// integration fixtures. The external DSN is only an allocator, never a test target.
func startRLSContainer(t *testing.T) (superDSN string, appDSN string) {
	t.Helper()
	return dbtest.SharedRLSPostgres(t)
}
