package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestCCBillRenewalSuccess_GrantsCreditsOnce(t *testing.T) {
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
	billingDays := 30

	creditTypeName := "test_credits_" + uuid.New().String()
	creditTypeID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()

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

	_, err = bunDB.NewInsert().Model(&models.Price{
		ID:               priceID,
		ProductID:        productID,
		DisplayName:      "Test Monthly",
		IsActive:         true,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Exec(ctx)
	require.NoError(t, err)

	periodEnd := now.Add(30 * 24 * time.Hour)
	periodStart := now
	_, err = bunDB.NewInsert().Model(&models.Subscription{
		ID:                      subID,
		UserID:                  userID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorCCBill,
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &periodEnd,
		StartedAt:               now,
		CreatedAt:               now,
		UpdatedAt:               now,
	}).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditBlock)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Payment)(nil)).Where("subscription_id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", creditTypeID).Exec(ctx)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	notifSvc := NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)
	creditsSvc := credits.NewCreditsService(dbi)

	nextRenewal := now.Add(30 * 24 * time.Hour).Format("2006-01-02")
	ts := now.Format("2006-01-02 15:04:05")
	body, err := json.Marshal(CCBillRenewalSuccessEvent{
		TransactionID:      "txn_" + uuid.New().String(),
		SubscriptionID:     ccbillSubID,
		ClientAccnum:       "1234",
		ClientSubacc:       "0000",
		Timestamp:          ts,
		BilledAmount:       "9.99",
		BilledCurrencyCode: "usd",
		NextRenewalDate:    nextRenewal,
	})
	require.NoError(t, err)

	svc := &CCBillWebhookService{
		Data: CCBillWebhookEvent{
			EventBody: body,
		},
		DB:                           dbi,
		SubscriptionService:          subSvc,
		SubscriptionLifecycleService: lifecycle,
		CreditsService:               creditsSvc,
	}

	require.NoError(t, svc.handleRenewalSuccess(ctx))

	depositCount, err := bunDB.NewSelect().
		Model((*models.CreditTransaction)(nil)).
		Where("user_id = ? AND credit_type_id = ?", userID, creditTypeID).
		Where("transaction_type = 'deposit' AND source = 'subscription_renewal'").
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, depositCount)
}
