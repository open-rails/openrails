//go:build integration

package productaccess

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// bundleEnv wires a real DB + RLS-pinned ctx for the #616 bundle-materialization
// tests. Mirrors product_access_service_test.go's newTestService but exposes the
// pool so the test can seed products + product_includes rows and assert on the
// raw openrails.grants ledger.
type bundleEnv struct {
	svc        *Service
	ctx        context.Context
	pool       *pgxpool.Pool
	merchantID uuid.UUID
	now        time.Time
}

func newBundleEnv(t *testing.T) *bundleEnv {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	return &bundleEnv{
		svc:        NewService(dbi, clockwork.NewFakeClockAt(now)),
		ctx:        ctx,
		pool:       pool,
		merchantID: dbtest.TestMerchantID.UUID(),
		now:        now,
	}
}

// product inserts a unique product and registers best-effort cleanup. Grants are
// append-only (REVOKE DELETE), so we cannot purge the grant rows; the product
// delete is best-effort (a grant's product_id FK may pin it) and leftover rows
// for this run's unique ids are harmless in the shared test DB.
func (e *bundleEnv) product(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(e.ctx,
		`INSERT INTO openrails.products (id, key, display_name, merchant_id)
		 VALUES ($1, $2, $3, $4)`,
		id, "bundle-test-"+id.String(), "Bundle Test Product", e.merchantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM openrails.product_includes WHERE merchant_id=$1 AND (product_id=$2 OR included_product_id=$2)`, e.merchantID, id)
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM openrails.products WHERE id=$1`, id)
	})
	return id
}

// includes records that parent grants/owns child (catalog bundle membership).
func (e *bundleEnv) includes(t *testing.T, parent, child uuid.UUID) {
	t.Helper()
	_, err := e.pool.Exec(e.ctx,
		`INSERT INTO openrails.product_includes (merchant_id, product_id, included_product_id)
		 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		e.merchantID, parent, child)
	require.NoError(t, err)
}

// ownershipGrantCount counts live grant-root ownership rows for a product.
func (e *bundleEnv) ownershipGrantCount(t *testing.T, productID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM openrails.grants
		 WHERE merchant_id=$1 AND product_id=$2 AND kind='ownership' AND event='grant'`,
		e.merchantID, productID).Scan(&n))
	return n
}

// Granting a bundle materializes an ownership grant for each directly-included
// product to the same customer, in the same tx; re-granting the same source is a
// no-op (still exactly one grant per child).
func TestGrantProductAccess_BundleIncludes(t *testing.T) {
	t.Run("DirectIncludesAndIdempotent", func(t *testing.T) {
		e := newBundleEnv(t)
		parent := e.product(t)
		childA := e.product(t)
		childB := e.product(t)
		e.includes(t, parent, childA)
		e.includes(t, parent, childB)

		userID := uuid.New().String()
		params := GrantParams{
			UserID:     userID,
			ProductID:  parent,
			SourceType: models.ProductAccessSourcePurchase,
			SourceID:   "pay_" + uuid.NewString(),
		}

		_, created, err := e.svc.GrantProductAccess(e.ctx, params)
		require.NoError(t, err)
		require.True(t, created)

		require.Equal(t, 1, e.ownershipGrantCount(t, parent), "parent granted")
		require.Equal(t, 1, e.ownershipGrantCount(t, childA), "childA materialized")
		require.Equal(t, 1, e.ownershipGrantCount(t, childB), "childB materialized")

		// Customer truly owns the children (not just a stray row).
		hasA, err := e.svc.HasProductAccess(e.ctx, userID, childA)
		require.NoError(t, err)
		require.True(t, hasA)

		// Re-grant the same source: idempotent, still exactly one grant per child.
		_, created2, err := e.svc.GrantProductAccess(e.ctx, params)
		require.NoError(t, err)
		require.False(t, created2)
		require.Equal(t, 1, e.ownershipGrantCount(t, parent))
		require.Equal(t, 1, e.ownershipGrantCount(t, childA))
		require.Equal(t, 1, e.ownershipGrantCount(t, childB))
	})

	t.Run("TransitiveNesting", func(t *testing.T) {
		e := newBundleEnv(t)
		parent := e.product(t)
		childA := e.product(t)
		grandchild := e.product(t)
		e.includes(t, parent, childA)
		e.includes(t, childA, grandchild)

		userID := uuid.New().String()
		_, _, err := e.svc.GrantProductAccess(e.ctx, GrantParams{
			UserID:     userID,
			ProductID:  parent,
			SourceType: models.ProductAccessSourcePurchase,
			SourceID:   "pay_" + uuid.NewString(),
		})
		require.NoError(t, err)

		require.Equal(t, 1, e.ownershipGrantCount(t, parent))
		require.Equal(t, 1, e.ownershipGrantCount(t, childA))
		require.Equal(t, 1, e.ownershipGrantCount(t, grandchild), "transitively-included grandchild granted")
	})

	t.Run("CycleTerminates", func(t *testing.T) {
		e := newBundleEnv(t)
		x := e.product(t)
		y := e.product(t)
		e.includes(t, x, y)
		e.includes(t, y, x) // X -> Y -> X cycle

		userID := uuid.New().String()
		done := make(chan error, 1)
		go func() {
			_, _, err := e.svc.GrantProductAccess(e.ctx, GrantParams{
				UserID:     userID,
				ProductID:  x,
				SourceType: models.ProductAccessSourcePurchase,
				SourceID:   "pay_" + uuid.NewString(),
			})
			done <- err
		}()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("GrantProductAccess did not terminate on a bundle cycle")
		}

		require.Equal(t, 1, e.ownershipGrantCount(t, x), "X granted once")
		require.Equal(t, 1, e.ownershipGrantCount(t, y), "Y granted once despite the cycle")
	})
}
