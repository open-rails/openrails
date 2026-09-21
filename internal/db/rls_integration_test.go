//go:build integration

package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
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
func newDBRetry(t *testing.T, dsn string) *DB {
	t.Helper()
	var result *DB
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		database, err := NewDB(t.Context(), &config.DBConfig{URL: dsn})
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
-- Unprivileged application role (migration-050 form) WITH LOGIN for the test.
	DO $$ BEGIN
	    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
	        CREATE ROLE openrails_app NOBYPASSRLS;
	    END IF;
	END $$;
	ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw';
	GRANT USAGE ON SCHEMA billing TO openrails_app;
	GRANT SELECT, INSERT, UPDATE, DELETE ON billing.rls_probe TO openrails_app;
	`

func startRLSContainer(t *testing.T) (superDSN string, appDSN string) {
	t.Helper()
	ctx := context.Background()

	// Escape hatch for flaky-testcontainers hosts: OPENRAILS_TEST_DB_DSN is the
	// SUPER/admin DSN; the openrails_app DSN is derived by swapping the userinfo
	// (the setup DDL creates that role with password 'app_pw').
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		superDSN = dsn
		u.User = url.UserPassword("openrails_app", "app_pw")
		appDSN = u.String()
		return superDSN, appDSN
	}

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithHostConfigModifier(postgresTestLimits),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)

	superDSN = fmt.Sprintf("postgresql://super:super@%s:%s/openrails?sslmode=disable", host, port.Port())
	appDSN = fmt.Sprintf("postgresql://openrails_app:app_pw@%s:%s/openrails?sslmode=disable", host, port.Port())
	return superDSN, appDSN
}

func postgresTestLimits(hc *container.HostConfig) {
	hc.Resources.Memory = 2 << 30
	hc.Resources.NanoCPUs = 2_000_000_000
}
