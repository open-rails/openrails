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
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestCCBillRenewalFailure_AppendsGraceEntitlements(t *testing.T) {
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

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()

	billingDays := 30
	periodStart := now
	paidEnd := now.Add(30 * 24 * time.Hour)
	nextRetryAt := paidEnd.Add(3 * 24 * time.Hour)

	_, err = bunDB.NewInsert().Model(&models.Product{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
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

	_, err = bunDB.NewInsert().Model(&models.Subscription{
		ID:                      subID,
		UserID:                  userID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorCCBill,
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               now,
		CreatedAt:               now,
		UpdatedAt:               now,
	}).Exec(ctx)
	require.NoError(t, err)

	// Paid subscription entitlement window [periodStart, paidEnd)
	paidEnt := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: "premium",
		StartAt:     periodStart,
		EndAt:       &paidEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err = bunDB.NewInsert().Model(paidEnt).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	body, err := json.Marshal(CCBillRenewalFailureEvent{
		TransactionID:  "txn_" + uuid.New().String(),
		SubscriptionID: ccbillSubID,
		ClientAccnum:   "1234",
		ClientSubacc:   "0000",
		Timestamp:      now.Format("2006-01-02 15:04:05"),
		NextRetryDate:  nextRetryAt.Format("2006-01-02"),
		FailureCode:    "declined",
		FailureReason:  "declined",
	})
	require.NoError(t, err)

	svc := &CCBillWebhookService{
		Data: CCBillWebhookEvent{
			EventBody: body,
		},
		DB:                  dbi,
		SubscriptionService: subSvc,
	}

	require.NoError(t, svc.handleRenewalFailure(ctx))

	// Subscription entitlement remains paid-through.
	var gotPaid models.Entitlement
	require.NoError(t, bunDB.NewSelect().Model(&gotPaid).Where("id = ?", paidEnt.ID).Scan(ctx))
	require.NotNil(t, gotPaid.EndAt)
	require.Equal(t, paidEnd.UTC(), gotPaid.EndAt.UTC())

	// Grace entitlement is appended [paidEnd, nextRetryAt)
	var grace models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&grace).
		Where("user_id = ? AND entitlement = ?", userID, "premium").
		Where("source_type = ?", models.EntitlementSourceGrace).
		Where("source_id = ?", subID).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		Limit(1).
		Scan(ctx))
	require.Equal(t, paidEnd.UTC(), grace.StartAt.UTC())
	require.NotNil(t, grace.EndAt)
	expectedGraceEnd := time.Date(nextRetryAt.Year(), nextRetryAt.Month(), nextRetryAt.Day(), 23, 59, 59, 0, time.UTC)
	require.Equal(t, expectedGraceEnd.UTC(), grace.EndAt.UTC())
}

func TestCCBillRenewalSuccess_RevokesAndDeletesGraceEntitlements(t *testing.T) {
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

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()

	billingDays := 30
	periodStart := now.Add(-30 * 24 * time.Hour)
	paidEnd := now.Add(-24 * time.Hour)

	_, err = bunDB.NewInsert().Model(&models.Product{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
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

	_, err = bunDB.NewInsert().Model(&models.Subscription{
		ID:                      subID,
		UserID:                  userID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorCCBill,
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               now,
		CreatedAt:               now,
		UpdatedAt:               now,
	}).Exec(ctx)
	require.NoError(t, err)

	paidEnt := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: "premium",
		StartAt:     periodStart,
		EndAt:       &paidEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err = bunDB.NewInsert().Model(paidEnt).Exec(ctx)
	require.NoError(t, err)

	graceEnd := now.Add(2 * 24 * time.Hour)
	graceActive := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: "premium",
		StartAt:     paidEnd,
		EndAt:       &graceEnd,
		SourceType:  models.EntitlementSourceGrace,
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err = bunDB.NewInsert().Model(graceActive).Exec(ctx)
	require.NoError(t, err)

	graceFutureEnd := graceEnd.Add(24 * time.Hour)
	graceFuture := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: "premium",
		StartAt:     graceEnd,
		EndAt:       &graceFutureEnd,
		SourceType:  models.EntitlementSourceGrace,
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err = bunDB.NewInsert().Model(graceFuture).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	nextRenewal := paidEnd.Add(30 * 24 * time.Hour)
	body, err := json.Marshal(CCBillRenewalSuccessEvent{
		TransactionID:      "txn_" + uuid.New().String(),
		SubscriptionID:     ccbillSubID,
		ClientAccnum:       "1234",
		ClientSubacc:       "0000",
		Timestamp:          now.Format("2006-01-02 15:04:05"),
		BilledAmount:       "9.99",
		BilledCurrencyCode: "usd",
		NextRenewalDate:    nextRenewal.Format("2006-01-02"),
	})
	require.NoError(t, err)

	svc := &CCBillWebhookService{
		Data: CCBillWebhookEvent{
			EventBody: body,
		},
		DB:                           dbi,
		SubscriptionService:          subSvc,
		SubscriptionLifecycleService: lifecycle,
	}

	require.NoError(t, svc.handleRenewalSuccess(ctx))

	var got models.Entitlement
	require.NoError(t, bunDB.NewSelect().Model(&got).Where("id = ?", graceActive.ID).Scan(ctx))
	require.NotNil(t, got.RevokedAt)
	require.Nil(t, got.DeletedAt)

	var gotFuture models.Entitlement
	require.NoError(t, bunDB.NewSelect().Model(&gotFuture).Where("id = ?", graceFuture.ID).Scan(ctx))
	require.NotNil(t, gotFuture.DeletedAt)
}
