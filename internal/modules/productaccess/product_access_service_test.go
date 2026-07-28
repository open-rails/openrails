//go:build integration

package productaccess

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// newTestService opens a real DB from OPENRAILS_TEST_DB_URL (same env-hatch the
// entitlement service unit tests use) and seeds a product so grants satisfy the
// product_id FK. Skips when no DSN is configured.
func newTestService(t *testing.T, now time.Time) (*Service, context.Context, uuid.UUID) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)

	// The Service is RLS-scoped (MerchantTx), so it needs a merchant on the ctx.
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)

	productID := uuid.New()
	_, err := gen.New(pool).CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "test-product-" + productID.String(),
		DisplayName: "Test Product",
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Ownership now lives in the append-only grant ledger (#511) — grants are
		// immutable (REVOKE DELETE), so we can't purge them; leftover grants for
		// this run's unique product/customer are harmless in the shared test DB.
		// The product delete is best-effort (a grant's product_id FK may pin it).
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	return NewService(dbi, clockwork.NewFakeClockAt(now)), ctx, productID
}

func TestGrantProductAccess_Idempotent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, ctx, productID := newTestService(t, now)
	userID := uuid.New().String()
	paymentID := uuid.New()

	params := GrantParams{
		UserID:     userID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourcePurchase,
		SourceID:   paymentID.String(), // SourceID is the idempotency key
	}

	first, created, err := svc.GrantProductAccess(ctx, params)
	require.NoError(t, err)
	require.True(t, created, "first grant should be newly created")
	require.NotNil(t, first)

	// Re-granting the same purchase is a no-op: same grant id, not created.
	second, created2, err := svc.GrantProductAccess(ctx, params)
	require.NoError(t, err)
	require.False(t, created2, "duplicate grant must not be created again")
	require.Equal(t, first.ID, second.ID)

	grants, err := svc.ListAccessibleProducts(ctx, userID)
	require.NoError(t, err)
	require.Len(t, grants, 1, "duplicate purchase must yield exactly one grant")
}

func TestHasProductAccess_And_List(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, ctx, productID := newTestService(t, now)
	userID := uuid.New().String()
	paymentID := uuid.New()

	has, err := svc.HasProductAccess(ctx, userID, productID)
	require.NoError(t, err)
	require.False(t, has, "no grant => no access")

	_, _, err = svc.GrantProductAccess(ctx, GrantParams{
		UserID:     userID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourcePurchase,
		SourceID:   paymentID.String(),
	})
	require.NoError(t, err)

	has, err = svc.HasProductAccess(ctx, userID, productID)
	require.NoError(t, err)
	require.True(t, has, "after grant => access")

	grants, err := svc.ListAccessibleProducts(ctx, userID)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, productID, grants[0].ProductID)
}

func TestRevokeProductAccessByPayment_OnRefund(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, ctx, productID := newTestService(t, now)
	userID := uuid.New().String()
	paymentID := uuid.New()

	// grants.payment_id has a real FK to payments (the old product_access_grants
	// table had none), so seed a real purchase payment for this customer/product.
	seedPool := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
	custID := dbtest.EnsureCustomerIDPgx(ctx, t, seedPool, userID)
	priceID := uuid.New()
	_, err := seedPool.Exec(ctx, `INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,999,'USD',720,true,$3)`,
		priceID, productID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	_, err = seedPool.Exec(ctx, `INSERT INTO openrails.payments (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, merchant_id, customer_id)
	                             VALUES ($1,$2,'nmi',$3,999,999,'USD','completed',$4,$5,$6)`,
		paymentID, priceID, "txn-"+paymentID.String(), now, dbtest.TestMerchantID.UUID(), custID)
	require.NoError(t, err)

	_, _, err = svc.GrantProductAccess(ctx, GrantParams{
		UserID:     userID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourcePurchase,
		SourceID:   paymentID.String(),
		PaymentID:  &paymentID,
	})
	require.NoError(t, err)

	n, err := svc.RevokeProductAccessByPayment(ctx, paymentID, models.ProductAccessRevokeRefund)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "refund must revoke the grant tied to the payment")

	has, err := svc.HasProductAccess(ctx, userID, productID)
	require.NoError(t, err)
	require.False(t, has, "after refund => no access")

	// Idempotent: a second revoke affects zero rows.
	n2, err := svc.RevokeProductAccessByPayment(ctx, paymentID, models.ProductAccessRevokeRefund)
	require.NoError(t, err)
	require.Equal(t, int64(0), n2)

	// The grant still appears in the full admin list, marked revoked.
	all, err := svc.ListAllGrantsByUser(ctx, userID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, models.ProductAccessStatusRevoked, all[0].Status)
	require.NotNil(t, all[0].RevokedAt)
}

func TestRevokeProductAccess_ByID(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, ctx, productID := newTestService(t, now)
	userID := uuid.New().String()

	grant, _, err := svc.GrantProductAccess(ctx, GrantParams{
		UserID:     userID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourceAdmin,
		SourceID:   "admin:" + userID + ":" + productID.String(),
	})
	require.NoError(t, err)

	found, err := svc.RevokeProductAccess(ctx, grant.ID, models.ProductAccessRevokeAdmin)
	require.NoError(t, err)
	require.True(t, found)

	// Revoking again => not found (already revoked).
	found2, err := svc.RevokeProductAccess(ctx, grant.ID, models.ProductAccessRevokeAdmin)
	require.NoError(t, err)
	require.False(t, found2)
}
