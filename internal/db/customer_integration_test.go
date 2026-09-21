//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests so the container is closed after use even when the testcontainers Ryuk
// reaper is unavailable (offline/sandboxed runs).
func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestEnsureCustomerID_UUIDReusesExistingPayableID(t *testing.T) {
	ctx := context.Background()
	pool := db.WrapPool(dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()), "")

	dbtest.EnsureTestMerchant(ctx, t, pool)
	tenantID := dbtest.TestMerchantID.UUID()
	userID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Hour)
	_, err := pool.Exec(ctx,
		`INSERT INTO billing.customers (id, merchant_id, created_at, last_seen_at)
		 VALUES ($1, $2, $3, $4)`,
		userID, tenantID, createdAt, createdAt,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM billing.customers WHERE id = $1`, userID)
	})

	resolved, err := db.EnsureCustomerID(ctx, pool, tenantID, userID.String())
	require.NoError(t, err)
	require.Equal(t, userID, resolved)

	// EnsureCustomerID reuses the existing payable row (id = subject UUID) and
	// refreshes last_seen_at (#491: customers is a pure balance keyed by id).
	var lastSeenAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen_at FROM billing.customers WHERE id = $1`, userID,
	).Scan(&lastSeenAt))
	require.True(t, lastSeenAt.After(createdAt))
}

// seedForeignCustomer creates a second merchant plus a customer row owned by it,
// returning that merchant's id and the customer id.
// It requires a handle that spans merchants (the privileged pool): the foreign
// customer row is by definition outside the test merchant's RLS scope.
func seedForeignCustomer(ctx context.Context, t *testing.T, pool gen.DBTX) (uuid.UUID, uuid.UUID) {
	t.Helper()
	otherMerchantID := uuid.New()
	customerID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		otherMerchantID, "cross-merchant-"+otherMerchantID.String()[:8])
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO billing.customers (id, merchant_id) VALUES ($1, $2)`,
		customerID, otherMerchantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM billing.customers WHERE id = $1`, customerID)
		_, _ = pool.Exec(ctx, `DELETE FROM billing.merchants WHERE id = $1`, otherMerchantID)
	})
	return otherMerchantID, customerID
}

// A privileged import can materialize the same subject in another merchant
// without transferring or modifying the original customer identity.
func TestEnsureCustomerID_SameSubjectHasIndependentMerchantRows(t *testing.T) {
	ctx := context.Background()
	// Import/bootstrap can span merchants; its explicit ownership tuple must
	// preserve both rows even on a privileged handle.
	pool := db.WrapPool(dbtest.SharedSuperuserPGXPool(t), "")

	dbtest.EnsureTestMerchant(ctx, t, pool)
	otherMerchantID, customerID := seedForeignCustomer(ctx, t, pool)

	_, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), customerID.String())
	require.NoError(t, err)

	var owners []uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT array_agg(merchant_id) FROM billing.customers WHERE id = $1`, customerID,
	).Scan(&owners))
	require.ElementsMatch(t, []uuid.UUID{otherMerchantID, dbtest.TestMerchantID.UUID()}, owners)
}

// Commerce materialization creates an independent FK target for each merchant.
func TestEnsureCustomerRow_SameSubjectHasIndependentMerchantRows(t *testing.T) {
	ctx := context.Background()
	// Privileged to exercise explicit merchant identity independent of RLS.
	pool := db.WrapPool(dbtest.SharedSuperuserPGXPool(t), "")

	dbtest.EnsureTestMerchant(ctx, t, pool)
	otherMerchantID, customerID := seedForeignCustomer(ctx, t, pool)

	err := db.EnsureCustomerRow(ctx, pool, dbtest.TestMerchantID.UUID(), customerID)
	require.NoError(t, err)

	var owners []uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT array_agg(merchant_id) FROM billing.customers WHERE id = $1`, customerID,
	).Scan(&owners))
	require.ElementsMatch(t, []uuid.UUID{otherMerchantID, dbtest.TestMerchantID.UUID()}, owners)
}

// The same id under the SAME merchant stays a plain idempotent no-op.
func TestEnsureCustomerRow_RepeatIsIdempotent(t *testing.T) {
	ctx := context.Background()
	// One merchant's own rows: the RLS-enforcing pinned pool, so the policies
	// stay live and the fixture proves the merchant can write its own customer.
	pool := db.WrapPool(dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()), "")

	dbtest.EnsureTestMerchant(ctx, t, pool)
	tenantID := dbtest.TestMerchantID.UUID()
	customerID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM billing.customers WHERE id = $1`, customerID)
	})

	require.NoError(t, db.EnsureCustomerRow(ctx, pool, tenantID, customerID))
	require.NoError(t, db.EnsureCustomerRow(ctx, pool, tenantID, customerID))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM billing.customers WHERE id = $1 AND merchant_id = $2`,
		customerID, tenantID,
	).Scan(&count))
	require.Equal(t, 1, count)
}

// The scoped unique key resolves concurrent first touches of one customer.
// A waiting insert completes after the other transaction commits.
func TestEnsureCustomerRow_ConcurrentFirstTouchConverges(t *testing.T) {
	ctx := context.Background()
	pool := db.WrapPool(dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()), "")

	dbtest.EnsureTestMerchant(ctx, t, pool)
	tenantID := dbtest.TestMerchantID.UUID()
	customerID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM billing.customers WHERE id = $1`, customerID)
	})

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	// The committing goroutine below is only reached if every assertion between
	// here and it passes. Without this, ANY failure in between abandons a
	// checked-out connection and the pool's Close cleanup blocks forever —
	// turning a one-line assertion failure into a whole-package hang.
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx,
		`INSERT INTO billing.customers (id, merchant_id) VALUES ($1, $2)`,
		customerID, tenantID)
	require.NoError(t, err)

	committed := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		committed <- tx.Commit(ctx)
	}()

	require.NoError(t, db.EnsureCustomerRow(ctx, pool, tenantID, customerID))
	require.NoError(t, <-committed)
}

// Payable identities are UUID-only (#364): non-UUID subjects are rejected, the
// empty subject stays a documented no-op.
func TestEnsureCustomerID_RejectsNonUUIDSubject(t *testing.T) {
	ctx := context.Background()
	pool := db.WrapPool(dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()), "")

	_, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), "legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")

	id, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), "")
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, id)
}
