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
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())

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

// seedForeignCustomer creates a second merchant plus a customer row owned by it,
// returning that merchant's id and the customer id.
// It requires a handle that spans merchants (the privileged pool): the foreign
// customer row is by definition outside the test merchant's RLS scope.
func seedForeignCustomer(ctx context.Context, t *testing.T, pool gen.DBTX) (uuid.UUID, uuid.UUID) {
	t.Helper()
	otherMerchantID := uuid.New()
	customerID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		otherMerchantID, "cross-merchant-"+otherMerchantID.String()[:8])
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		customerID, otherMerchantID, customerID.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.customers WHERE id = $1`, customerID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchants WHERE id = $1`, otherMerchantID)
	})
	return otherMerchantID, customerID
}

// #889 family: customers.id is globally unique but customer rows are merchant
// isolated, so an id already owned by another merchant must be REFUSED, never
// re-pointed. On a privileged (RLS-bypassing) handle — bootstrap, import, the
// dev owner connection — the upsert used to rewrite the other merchant's row
// and hand the caller an id it does not own.
func TestEnsureCustomerID_RefusesCrossMerchantClaim(t *testing.T) {
	ctx := context.Background()
	// The privileged handle IS the subject of this test: the guard has to hold
	// where RLS does not. A bare SharedPGXPool is RLS-enforcing with no
	// app.merchant_id, so seeding customers on it fails WITH CHECK (42501).
	pool := dbtest.SharedSuperuserPGXPool(t)

	dbtest.EnsureTestMerchant(ctx, t, pool)
	otherMerchantID, customerID := seedForeignCustomer(ctx, t, pool)

	_, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), customerID.String())
	require.ErrorIs(t, err, db.ErrCustomerOwnedByAnotherMerchant)

	var ownerID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT merchant_id FROM openrails.customers WHERE id = $1`, customerID,
	).Scan(&ownerID))
	require.Equal(t, otherMerchantID, ownerID, "the owning merchant must not change")
}

// EnsureCustomerRow is the FK-target materializer every commerce Create calls.
// Its ON CONFLICT DO NOTHING used to make a cross-merchant id a SILENT success,
// so the caller's row landed pointing at another merchant's customer (FK checks
// bypass RLS).
func TestEnsureCustomerRow_RefusesCrossMerchantClaim(t *testing.T) {
	ctx := context.Background()
	// Privileged for the same reason as the sibling above: FK checks bypass RLS,
	// so the silent-no-op corruption this guards is reachable precisely here.
	pool := dbtest.SharedSuperuserPGXPool(t)

	dbtest.EnsureTestMerchant(ctx, t, pool)
	otherMerchantID, customerID := seedForeignCustomer(ctx, t, pool)

	err := db.EnsureCustomerRow(ctx, pool, dbtest.TestMerchantID.UUID(), customerID)
	require.ErrorIs(t, err, db.ErrCustomerOwnedByAnotherMerchant)

	var ownerID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT merchant_id FROM openrails.customers WHERE id = $1`, customerID,
	).Scan(&ownerID))
	require.Equal(t, otherMerchantID, ownerID)
}

// The same id under the SAME merchant stays a plain idempotent no-op.
func TestEnsureCustomerRow_RepeatIsIdempotent(t *testing.T) {
	ctx := context.Background()
	// One merchant's own rows: the RLS-enforcing pinned pool, so the policies
	// stay live and the fixture proves the merchant can write its own customer.
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())

	dbtest.EnsureTestMerchant(ctx, t, pool)
	tenantID := dbtest.TestMerchantID.UUID()
	customerID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.customers WHERE id = $1`, customerID)
	})

	require.NoError(t, db.EnsureCustomerRow(ctx, pool, tenantID, customerID))
	require.NoError(t, db.EnsureCustomerRow(ctx, pool, tenantID, customerID))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.customers WHERE id = $1 AND merchant_id = $2`,
		customerID, tenantID,
	).Scan(&count))
	require.Equal(t, 1, count)
}

// A concurrent first-touch of the SAME customer under the same merchant must
// converge, not be mistaken for a foreign owner: the insert sees nothing (the
// other writer is uncommitted at snapshot time) and the locking re-read settles
// it once that writer commits (#889).
func TestEnsureCustomerRow_ConcurrentFirstTouchConverges(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())

	dbtest.EnsureTestMerchant(ctx, t, pool)
	tenantID := dbtest.TestMerchantID.UUID()
	customerID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.customers WHERE id = $1`, customerID)
	})

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	// The committing goroutine below is only reached if every assertion between
	// here and it passes. Without this, ANY failure in between abandons a
	// checked-out connection and the pool's Close cleanup blocks forever —
	// turning a one-line assertion failure into a whole-package hang.
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		customerID, tenantID, customerID.String())
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
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())

	_, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), "legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")

	id, err := db.EnsureCustomerID(ctx, pool, dbtest.TestMerchantID.UUID(), "")
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, id)
}
