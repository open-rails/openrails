//go:build integration

package subscriptions

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// TestCancelMembership_CascadesToSolanaCranker proves #264: cancelling a Solana
// membership flips the linked billing.solana_subscriptions row to cancelled so
// the hourly cranker's ListDue (status = active filter) no longer returns it,
// which stops billing because OpenRails is the only puller.
func TestCancelMembership_CascadesToSolanaCranker(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 30
	periodStart := now
	paidEnd := now.Add(30 * 24 * time.Hour)

	insertCatalogAndSub(ctx, t, bunDB, now, billingDays, productID, priceID, subID, userID, periodStart, paidEnd)

	// Linked active solana_subscriptions row, due now so it would be cranked.
	solRepo := dbrepo.NewSolanaSubscriptionRepo(dbi)
	solRow := &models.SolanaSubscription{
		ID:                       uuid.New(),
		TenantID:                 uuid.New(),
		SubscriptionID:           subID,
		SubscriberWallet:         "wallet_" + uuid.NewString(),
		AuthorityPDA:             "auth_" + uuid.NewString(),
		SubscriptionPDA:          "subpda_" + uuid.NewString(),
		PlanPDA:                  "plan_" + uuid.NewString(),
		MerchantAddress:          "merchant_" + uuid.NewString(),
		Mint:                     "mint_" + uuid.NewString(),
		PlanCreatedAtFingerprint: 123,
		NextPullAt:               now.Add(-time.Hour), // overdue → ListDue returns it
		Status:                   models.SolanaSubscriptionActive,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	require.NoError(t, solRepo.Upsert(ctx, solRow))

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.SolanaSubscription)(nil)).Where("subscription_id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	// Sanity: ListDue returns it before cancellation.
	due, err := solRepo.ListDue(ctx, now, 0)
	require.NoError(t, err)
	require.True(t, containsSolanaSub(due, solRow.ID), "row should be due before cancel")

	lifecycle := newLifecycleForTest(dbi)
	cancelType := models.CancelTypeUser
	require.NoError(t, lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &subID,
		CancelType:     cancelType,
		RevokeAccess:   true,
	}))

	// The linked solana row is now cancelled...
	got, err := solRepo.GetBySubscriptionID(ctx, subID)
	require.NoError(t, err)
	require.Equal(t, models.SolanaSubscriptionCancelled, got.Status)

	// ...and the cranker's due query no longer returns it.
	dueAfter, err := solRepo.ListDue(ctx, now, 0)
	require.NoError(t, err)
	require.False(t, containsSolanaSub(dueAfter, solRow.ID), "cancelled row must not be due")
}

// TestCancelMembership_SolanaWithoutEnrolledRow proves the cascade tolerates a
// Solana subscription that was never enrolled on-chain (no solana_subscriptions
// row): the cancel still succeeds rather than erroring.
func TestCancelMembership_SolanaWithoutEnrolledRow(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 30

	insertCatalogAndSub(ctx, t, bunDB, now, billingDays, productID, priceID, subID, userID, now, now.Add(30*24*time.Hour))

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	lifecycle := newLifecycleForTest(dbi)
	cancelType := models.CancelTypeUser
	require.NoError(t, lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &subID,
		CancelType:     cancelType,
		RevokeAccess:   true,
	}), "cancel must succeed even with no enrolled solana row")

	var sub models.Subscription
	require.NoError(t, bunDB.NewSelect().Model(&sub).Where("id = ?", subID).Scan(ctx))
	require.Equal(t, models.StatusCancelled, sub.Status)
}

func insertCatalogAndSub(ctx context.Context, t *testing.T, bunDB *bun.DB, now time.Time, billingDays int, productID, priceID, subID uuid.UUID, userID string, periodStart, paidEnd time.Time) {
	t.Helper()
	_, err := bunDB.NewInsert().Model(&models.Product{
		ID:          productID,
		Slug:        "test_product_" + uuid.NewString(),
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
		},
		Status:    models.CatalogStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = bunDB.NewInsert().Model(&models.Price{
		ID:               priceID,
		ProductID:        productID,
		Status:           models.CatalogStatusActive,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = bunDB.NewInsert().Model(&models.Subscription{
		ID:                    subID,
		UserID:                userID,
		ProductID:             productID,
		PriceID:               priceID,
		Status:                models.StatusActive,
		Processor:             models.ProcessorSolana,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &paidEnd,
		StartedAt:             now,
		CreatedAt:             now,
		UpdatedAt:             now,
	}).Exec(ctx)
	require.NoError(t, err)
}

func newLifecycleForTest(dbi *db.DB) *SubscriptionLifecycleService {
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	return NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, nil, nil, nil, nil)
}

func containsSolanaSub(rows []*models.SolanaSubscription, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
