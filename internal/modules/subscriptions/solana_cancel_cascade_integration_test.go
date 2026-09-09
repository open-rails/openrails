//go:build integration

package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
)

// TestCancelMembership_CascadesToSolanaCranker proves #264: cancelling a Solana
// membership flips the linked openrails.solana_subscriptions row to cancelled so
// the hourly cranker's ListDue (status = active filter) no longer returns it,
// which stops billing because OpenRails is the only puller.
func TestCancelMembership_CascadesToSolanaCranker(t *testing.T) {

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 720
	periodStart := now
	paidEnd := now.Add(30 * 24 * time.Hour)

	insertCatalogAndSub(ctx, t, dbi, now, billingDays, productID, priceID, subID, userID, periodStart, paidEnd)

	// Linked active solana_subscriptions row, due now so it would be cranked.
	solRepo := solanasubs.NewSolanaSubscriptionRepo(dbi)
	solRow := &models.SolanaSubscription{
		ID:                       uuid.New(),
		MerchantID:               dbtest.TestMerchantID.UUID(),
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
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.solana_subscriptions WHERE subscription_id = $1", subID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
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

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 720

	insertCatalogAndSub(ctx, t, dbi, now, billingDays, productID, priceID, subID, userID, now, now.Add(30*24*time.Hour))

	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	lifecycle := newLifecycleForTest(dbi)
	cancelType := models.CancelTypeUser
	require.NoError(t, lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &subID,
		CancelType:     cancelType,
		RevokeAccess:   true,
	}), "cancel must succeed even with no enrolled solana row")

	var subStatus string
	require.NoError(t, dbi.Pool().QueryRow(ctx,
		"SELECT status FROM openrails.subscriptions WHERE id = $1", subID,
	).Scan(&subStatus))
	require.Equal(t, string(models.StatusCancelled), subStatus)
}

func TestCancelMembership_SolanaCascadeFailureRollsBack(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	insertCatalogAndSub(ctx, t, dbi, now, 30, productID, priceID, subID, userID, now, now.Add(30*24*time.Hour))

	solRepo := solanasubs.NewSolanaSubscriptionRepo(dbi)
	solRow := newDueSolanaSubscription(now, subID)
	require.NoError(t, solRepo.Upsert(ctx, solRow))
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.solana_subscriptions WHERE subscription_id = $1", subID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	lifecycle := newLifecycleForTest(dbi)
	injected := errors.New("injected Solana cascade failure")
	lifecycle.cancelSolanaSubscription = func(context.Context, *db.DB, uuid.UUID) error { return injected }
	err := lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &subID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   true,
	})
	require.ErrorIs(t, err, injected)

	var parentStatus string
	require.NoError(t, dbi.Pool().QueryRow(ctx, "SELECT status FROM openrails.subscriptions WHERE id = $1", subID).Scan(&parentStatus))
	require.Equal(t, string(models.StatusActive), parentStatus, "cascade failure must roll the parent cancellation back")
	got, err := solRepo.GetBySubscriptionID(ctx, subID)
	require.NoError(t, err)
	require.Equal(t, models.SolanaSubscriptionActive, got.Status)

	lifecycle.cancelSolanaSubscription = nil
	require.NoError(t, lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &subID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   true,
	}))
	require.NoError(t, dbi.Pool().QueryRow(ctx, "SELECT status FROM openrails.subscriptions WHERE id = $1", subID).Scan(&parentStatus))
	require.Equal(t, string(models.StatusCancelled), parentStatus)
	got, err = solRepo.GetBySubscriptionID(ctx, subID)
	require.NoError(t, err)
	require.Equal(t, models.SolanaSubscriptionCancelled, got.Status)
}

func TestListDueSolanaSubscriptions_ExcludesTerminalParent(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	insertCatalogAndSub(ctx, t, dbi, now, 30, productID, priceID, subID, userID, now, now.Add(30*24*time.Hour))

	solRepo := solanasubs.NewSolanaSubscriptionRepo(dbi)
	solRow := newDueSolanaSubscription(now, subID)
	require.NoError(t, solRepo.Upsert(ctx, solRow))
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.solana_subscriptions WHERE subscription_id = $1", subID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	due, err := solRepo.ListDue(ctx, now, 0)
	require.NoError(t, err)
	require.True(t, containsSolanaSub(due, solRow.ID))

	_, err = dbi.Pool().Exec(ctx,
		`UPDATE openrails.subscriptions
		 SET status = 'cancelled', cancel_type = 'user', cancelled_at = $2, ended_at = $2, updated_at = $2
		 WHERE id = $1`, subID, now)
	require.NoError(t, err)

	due, err = solRepo.ListDue(ctx, now, 0)
	require.NoError(t, err)
	require.False(t, containsSolanaSub(due, solRow.ID), "an active mirror must not make a terminal parent billable")
}

func newDueSolanaSubscription(now time.Time, subID uuid.UUID) *models.SolanaSubscription {
	return &models.SolanaSubscription{
		ID:                       uuid.New(),
		MerchantID:               dbtest.TestMerchantID.UUID(),
		SubscriptionID:           subID,
		SubscriberWallet:         "wallet_" + uuid.NewString(),
		AuthorityPDA:             "auth_" + uuid.NewString(),
		SubscriptionPDA:          "subpda_" + uuid.NewString(),
		PlanPDA:                  "plan_" + uuid.NewString(),
		MerchantAddress:          "merchant_" + uuid.NewString(),
		Mint:                     "mint_" + uuid.NewString(),
		PlanCreatedAtFingerprint: 123,
		NextPullAt:               now.Add(-time.Hour),
		Status:                   models.SolanaSubscriptionActive,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
}

func insertCatalogAndSub(ctx context.Context, t *testing.T, dbi *db.DB, now time.Time, billingDays int, productID, priceID, subID uuid.UUID, userID string, periodStart, paidEnd time.Time) {
	t.Helper()
	q := gen.New(dbi.Pool())

	entSpec, err := json.Marshal(map[string]*int{"premium": nil})
	require.NoError(t, err)
	desc := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:               productID,
		Key:              "test_product_" + uuid.NewString(),
		DisplayName:      "Test Product",
		MerchantID:       dbtest.TestMerchantID.UUID(),
		Description:      &desc,
		EntitlementsSpec: entSpec,
		Archived:         false,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	cycleHours := int32(billingDays * 24)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		ProductID:           productID,
		Archived:            false,
		Amount:              999,
		Currency:            "USD",
		MerchantID:          dbtest.TestMerchantID.UUID(),
		AccessDurationHours: &cycleHours,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	subPriceID := priceID
	pspID := dbtest.EnsureTestPSP(ctx, t, dbi.Pool(), dbtest.TestMerchantID.UUID(), string(models.RailSolana))
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    subID,
		CustomerID:            dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Pool(), userID),
		ProductID:             productID,
		PriceID:               &subPriceID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailSolana),
		PspID:                 pspID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &paidEnd,
		StartedAt:             now,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
	require.NoError(t, err)
}

func newLifecycleForTest(dbi *db.DB) *SubscriptionLifecycleService {
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	return NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, nil, nil, nil)
}

func containsSolanaSub(rows []*models.SolanaSubscription, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
