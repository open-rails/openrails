package credits_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func runGrantSubscriptionCredits_Idempotent_PerPeriod(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	// Ensure migration table exists; if not, fail fast with a helpful message.
	var exists bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_blocks')").
		Scan(ctx, &exists))
	if !exists {
		t.Skip("billing.credit_blocks not found; run migrations before integration tests")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(30 * 24 * time.Hour)

	creditTypeName := "test_credits_" + uuid.New().String()
	creditTypeID := uuid.New()
	productID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID:            creditTypeID,
		Name:          creditTypeName,
		DisplayName:   "Test Credits",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = bunDB.NewInsert().Model(&models.Product{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: "Test",
		CreditsSpec: models.CreditsSpec{
			creditTypeName: {Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
		},
		IsActive:  true,
		CreatedAt: now,
		UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	priceID := uuid.New()
	cycle := 30
	_, err = bunDB.NewInsert().Model(&models.Price{
		ID:               priceID,
		ProductID:        productID,
		DisplayName:      "Test Price",
		IsActive:         true,
		Amount:           100,
		Currency:         "USD",
		BillingCycleDays: &cycle,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = bunDB.NewInsert().Model(&models.Subscription{
		ID:                      subID,
		UserID:                  userID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorStripe,
		ProcessorSubscriptionID: "sub_test_" + uuid.New().String(),
		CurrentPeriodStartsAt:   &now,
		CurrentPeriodEndsAt:     &periodEnd,
		StartedAt:               now,
	}).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditBlock)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", creditTypeID).Exec(ctx)
	})

	creditsSvc := credits.NewCreditsService(dbi)
	creditsSvc.Clock = nil

	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))

	depositCount, err := bunDB.NewSelect().
		Model((*models.CreditTransaction)(nil)).
		Where("user_id = ? AND credit_type_id = ?", userID, creditTypeID).
		Where("transaction_type = 'deposit' AND source = 'subscription_renewal'").
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, depositCount)

	expectedGrantID := uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte(fmt.Sprintf("openrails:sub_credit_grant:%s:%s:%s", subID, creditTypeID, periodEnd.UTC().Format(time.RFC3339Nano))),
	)

	dep := new(models.CreditTransaction)
	require.NoError(t, bunDB.NewSelect().
		Model(dep).
		Where("user_id = ? AND credit_type_id = ?", userID, creditTypeID).
		Where("transaction_type = 'deposit' AND source = 'subscription_renewal'").
		Limit(1).
		Scan(ctx))
	require.NotNil(t, dep.SourceID)
	require.Equal(t, expectedGrantID.String(), *dep.SourceID)

	bal := new(models.UserCreditBalance)
	require.NoError(t, bunDB.NewSelect().
		Model(bal).
		Where("user_id = ? AND credit_type_id = ?", userID, creditTypeID).
		Limit(1).
		Scan(ctx))
	require.Equal(t, int64(100), bal.Balance)
}

func TestGrantSubscriptionCredits_Idempotent_PerPeriod(t *testing.T) {
	runGrantSubscriptionCredits_Idempotent_PerPeriod(t)
}

func TestGrantSubscriptionCredits_ReplaySafety_StripeStyle(t *testing.T) {
	// This simulates replayed “renewal success” processing (e.g., Stripe invoice.paid re-delivery)
	// by calling GrantSubscriptionCredits twice for the same (subscription_id, credit_type, period_end).
	runGrantSubscriptionCredits_Idempotent_PerPeriod(t)
}

func TestGrantSubscriptionCredits_MixedCadence(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var exists bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_blocks')").
		Scan(ctx, &exists))
	if !exists {
		t.Skip("billing.credit_blocks not found; run migrations before integration tests")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(30 * 24 * time.Hour)

	ctOnceName := "test_once_" + uuid.New().String()
	ctRenewName := "test_renew_" + uuid.New().String()
	ctOnceID := uuid.New()
	ctRenewID := uuid.New()
	productID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID:            ctOnceID,
		Name:          ctOnceName,
		DisplayName:   "Once Credits",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	}).Exec(ctx)
	require.NoError(t, err)
	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID:            ctRenewID,
		Name:          ctRenewName,
		DisplayName:   "Renew Credits",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = bunDB.NewInsert().Model(&models.Product{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: "Test",
		CreditsSpec: models.CreditsSpec{
			ctOnceName:  {Amount: 10, Cadence: models.CreditGrantCadenceOnce},
			ctRenewName: {Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
		},
		IsActive:  true,
		CreatedAt: now,
		UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	priceID := uuid.New()
	cycle := 30
	_, err = bunDB.NewInsert().Model(&models.Price{
		ID:               priceID,
		ProductID:        productID,
		DisplayName:      "Test Price",
		IsActive:         true,
		Amount:           100,
		Currency:         "USD",
		BillingCycleDays: &cycle,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = bunDB.NewInsert().Model(&models.Subscription{
		ID:                      subID,
		UserID:                  userID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorStripe,
		ProcessorSubscriptionID: "sub_test_" + uuid.New().String(),
		CurrentPeriodStartsAt:   &now,
		CurrentPeriodEndsAt:     &periodEnd,
		StartedAt:               now,
	}).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id IN (?)", bun.In([]uuid.UUID{ctOnceID, ctRenewID})).Exec(ctx)
	})

	creditsSvc := credits.NewCreditsService(dbi)

	// Once grant should apply once.
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadenceOnce,
		Source:         "subscription_initial",
	}))
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadenceOnce,
		Source:         "subscription_initial",
	}))

	// Renewal grant should apply once.
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))
	require.NoError(t, creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
		SubscriptionID: subID,
		PeriodEnd:      periodEnd,
		Cadence:        models.CreditGrantCadencePerRenewal,
		Source:         "subscription_renewal",
	}))

	// Total should be 10 + 100 across two types, each exactly once.
	balOnce := new(models.UserCreditBalance)
	require.NoError(t, bunDB.NewSelect().Model(balOnce).Where("user_id = ? AND credit_type_id = ?", userID, ctOnceID).Scan(ctx))
	require.Equal(t, int64(10), balOnce.Balance)
	balRenew := new(models.UserCreditBalance)
	require.NoError(t, bunDB.NewSelect().Model(balRenew).Where("user_id = ? AND credit_type_id = ?", userID, ctRenewID).Scan(ctx))
	require.Equal(t, int64(100), balRenew.Balance)
}
