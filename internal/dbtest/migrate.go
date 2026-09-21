//go:build integration

package dbtest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/stretchr/testify/require"
)

// ApplyPostgresMigrations initializes a fixture's billing namespace and runtime
// access through the same owner/runtime pool contract used by host applications.
func ApplyPostgresMigrations(t *testing.T, ownerDSN, runtimeDSN, schema string) {
	t.Helper()
	owner, err := pgxpool.New(t.Context(), ownerDSN)
	require.NoError(t, err)
	defer owner.Close()
	runtime, err := pgxpool.New(t.Context(), runtimeDSN)
	require.NoError(t, err)
	defer runtime.Close()
	require.NoError(t, migrate.ApplyPostgresMigrations(t.Context(), owner, migrate.Options{Schema: schema, RuntimePool: runtime}))
}
