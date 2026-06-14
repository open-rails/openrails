//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests so the container is closed after use even when the testcontainers Ryuk
// reaper is unavailable (offline/sandboxed runs).
func TestMain(m *testing.M) { dbtest.RunMain(m) }

// seedCustomer materializes the openrails.customers row a direct money-row
// insert needs to satisfy the customer_id FK. customers is UUID-only (#491): the
// id IS the payable customer id; tenant_id is the canonical test tenant (#336:
// no default tenant).
func seedCustomer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tsID uuid.UUID) {
	t.Helper()
	dbtest.EnsureTestTenant(ctx, t, pool)
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id)
		 VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, tsID, dbtest.TestTenantID.UUID())
	require.NoError(t, err)
}
