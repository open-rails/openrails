//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests so the container is closed after use even when the testcontainers Ryuk
// reaper is unavailable (offline/sandboxed runs).
func TestMain(m *testing.M) { dbtest.RunMain(m) }

// fullModeConfig is the operating mode every intent Runner in this package must
// state. or#865 made the origin x mode gate fail CLOSED: a Runner with no
// ModeView cannot tell which mode it is in, so it parks every intent instead of
// executing it. A top-up fixture that expects the charge to happen has to say
// "full" out loud — silence is a wiring bug, not a default.
func fullModeConfig() *config.Config {
	return &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
}

// strptr returns a pointer to s — a shared helper for the money integration suite.
func strptr(s string) *string { return &s }

// seedCustomer materializes the openrails.customers row a direct money-row
// insert needs to satisfy the customer_id FK. customers is UUID-only (#491): the
// id IS the payable customer id; merchant_id is the canonical test merchant (#336:
// no default merchant).
func seedCustomer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tsID uuid.UUID) {
	t.Helper()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id)
		 VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, tsID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
}
