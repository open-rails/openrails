//go:build integration

package converge

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// startReconcilePostgres boots the shared fully-migrated Postgres (incl. the
// reconciliation tables) and returns a *db.DB connected AS the unprivileged
// openrails_app role, so the Convergence Engine's reads and writes run under the
// same RLS chokepoint as production.
func startReconcilePostgres(t *testing.T) *db.DB {
	t.Helper()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err := db.NewWithPGXPool(pool, "") // default schema (shared harness)
	require.NoError(t, err)
	return appDB
}
