//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests so the container is closed after use even when the testcontainers Ryuk
// reaper is unavailable (offline/sandboxed runs).
func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestEnsureCustomerID_UUIDReusesExistingPayableID(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)

	dbtest.EnsureTestMerchant(ctx, t, pool)
	tenantID := dbtest.TestMerchantID.UUID()
	userID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Hour)
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, created_at, last_seen_at)
		 VALUES ($1, $2, $3, $4)`,
		userID, tenantID, createdAt, createdAt,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.customers WHERE id = $1`, userID)
	})

	resolved, err := db.EnsureCustomerID(ctx, pool, tenantID, userID.String())
	require.NoError(t, err)
	require.Equal(t, userID, resolved)

	// EnsureCustomerID reuses the existing payable row (id = subject UUID) and
	// refreshes last_seen_at (#491: customers is a pure balance keyed by id).
	var lastSeenAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen_at FROM openrails.customers WHERE id = $1`, userID,
	).Scan(&lastSeenAt))
	require.True(t, lastSeenAt.After(createdAt))
}

// Payable identities are UUID-only (#364): non-UUID subjects are rejected, the
// empty subject stays a documented no-op.
func TestEnsureCustomerID_RejectsNonUUIDSubject(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)

	_, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), "legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")

	id, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), "")
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, id)
}
