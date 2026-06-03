//go:build integration

package productaccess

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// newTestService opens a real DB from OPENRAILS_TEST_DB_URL (same env-hatch the
// entitlement service unit tests use) and seeds a product so grants satisfy the
// product_id FK. Skips when no DSN is configured.
func newTestService(t *testing.T, now time.Time) (*Service, context.Context, uuid.UUID) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	productID := uuid.New()
	product := &models.Product{
		ID:          productID,
		Slug:        "test-product-" + productID.String(),
		DisplayName: "Test Product",
		Status:      models.CatalogStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err = bunDB.NewInsert().Model(product).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.ProductAccessGrant)(nil)).Where("product_id = ?", productID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(context.Background())
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
		SourceID:   paymentID.String(),
		PaymentID:  &paymentID,
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
		PaymentID:  &paymentID,
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

	_, _, err := svc.GrantProductAccess(ctx, GrantParams{
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
