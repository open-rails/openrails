//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestCCBillRenewalFailure_AppendsGraceEntitlements(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureMerchantSubjectIDPgx(ctx, t, pool, userID)
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()

	billingDays := int32(30)
	periodStart := now
	paidEnd := now.Add(30 * 24 * time.Hour)
	nextRetryAt := paidEnd.Add(3 * 24 * time.Hour)

	entitlementsSpecJSON, err := json.Marshal(map[string]*int{"premium": nil})
	require.NoError(t, err)
	description := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:               productID,
		Slug:             "test_product_" + uuid.New().String(),
		DisplayName:      "Test Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Status:           string(models.CatalogStatusActive),
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               priceID,
		ProductID:        productID,
		Amount:           999,
		Currency:         "usd",
		Status:           string(models.CatalogStatusActive),
		BillingCycleDays: &billingDays,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                      subID,
		MerchantSubjectID:         tenantSubjectID,
		ProductID:               productID,
		PriceID:                 &priceID,
		Status:                  string(models.StatusActive),
		Processor:               string(models.ProcessorCCBill),
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               now,
		CreatedAt:               now,
		UpdatedAt:               now,
	})
	require.NoError(t, err)

	// Paid subscription entitlement window [periodStart, paidEnd)
	paidEntID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:              paidEntID,
		MerchantSubjectID: tenantSubjectID,
		Entitlement:     "premium",
		StartAt:         periodStart,
		EndAt:           &paidEnd,
		SourceType:      string(models.EntitlementSourceSubscription),
		SourceID:        &subID,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE merchant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
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
	gotPaid, err := q.GetEntitlementByID(ctx, paidEntID)
	require.NoError(t, err)
	require.NotNil(t, gotPaid.EndAt)
	require.Equal(t, paidEnd.UTC(), gotPaid.EndAt.UTC())

	// Grace entitlement is appended [paidEnd, nextRetryAt)
	var graceStartAt time.Time
	var graceEndAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT start_at, end_at FROM openrails.entitlements
		 WHERE merchant_subject_id = $1 AND entitlement = $2
		   AND source_type = $3
		   AND source_id = $4
		   AND revoked_at IS NULL
		   AND deleted_at IS NULL
		 LIMIT 1`,
		tenantSubjectID, "premium", string(models.EntitlementSourceGrace), subID,
	).Scan(&graceStartAt, &graceEndAt))
	require.Equal(t, paidEnd.UTC(), graceStartAt.UTC())
	require.NotNil(t, graceEndAt)
	// Grace end mirrors capCCBillRetryAt: the parsed end-of-day retry date,
	// clamped to paidTermEnd + ccbillGraceCap (72h). For these fixture dates
	// the cap wins.
	expectedGraceEnd := time.Date(nextRetryAt.Year(), nextRetryAt.Month(), nextRetryAt.Day(), 23, 59, 59, 0, time.UTC)
	if maxGraceEnd := paidEnd.UTC().Add(ccbillGraceCap); expectedGraceEnd.After(maxGraceEnd) {
		expectedGraceEnd = maxGraceEnd
	}
	require.Equal(t, expectedGraceEnd.UTC(), graceEndAt.UTC())
}

func TestCCBillRenewalSuccess_RevokesAndDeletesGraceEntitlements(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureMerchantSubjectIDPgx(ctx, t, pool, userID)
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()

	billingDays := int32(30)
	periodStart := now.Add(-30 * 24 * time.Hour)
	paidEnd := now.Add(-24 * time.Hour)

	entitlementsSpecJSON, err := json.Marshal(map[string]*int{"premium": nil})
	require.NoError(t, err)
	description := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:               productID,
		Slug:             "test_product_" + uuid.New().String(),
		DisplayName:      "Test Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Status:           string(models.CatalogStatusActive),
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               priceID,
		ProductID:        productID,
		Amount:           999,
		Currency:         "usd",
		Status:           string(models.CatalogStatusActive),
		BillingCycleDays: &billingDays,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                      subID,
		MerchantSubjectID:         tenantSubjectID,
		ProductID:               productID,
		PriceID:                 &priceID,
		Status:                  string(models.StatusActive),
		Processor:               string(models.ProcessorCCBill),
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               now,
		CreatedAt:               now,
		UpdatedAt:               now,
	})
	require.NoError(t, err)

	paidEntID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:              paidEntID,
		MerchantSubjectID: tenantSubjectID,
		Entitlement:     "premium",
		StartAt:         periodStart,
		EndAt:           &paidEnd,
		SourceType:      string(models.EntitlementSourceSubscription),
		SourceID:        &subID,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	require.NoError(t, err)

	graceEnd := now.Add(2 * 24 * time.Hour)
	graceActiveID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:              graceActiveID,
		MerchantSubjectID: tenantSubjectID,
		Entitlement:     "premium",
		StartAt:         paidEnd,
		EndAt:           &graceEnd,
		SourceType:      string(models.EntitlementSourceGrace),
		SourceID:        &subID,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	require.NoError(t, err)

	graceFutureEnd := graceEnd.Add(24 * time.Hour)
	graceFutureID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:              graceFutureID,
		MerchantSubjectID: tenantSubjectID,
		Entitlement:     "premium",
		StartAt:         graceEnd,
		EndAt:           &graceFutureEnd,
		SourceType:      string(models.EntitlementSourceGrace),
		SourceID:        &subID,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE merchant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
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

	var gotRevokedAt, gotDeletedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT revoked_at, deleted_at FROM openrails.entitlements WHERE id = $1",
		graceActiveID).Scan(&gotRevokedAt, &gotDeletedAt))
	require.NotNil(t, gotRevokedAt)
	require.Nil(t, gotDeletedAt)

	// The future grace window is soft-deleted (deleted_at set); raw SQL sees
	// soft-deleted rows without any opt-in.
	var gotFutureDeletedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT deleted_at FROM openrails.entitlements WHERE id = $1",
		graceFutureID).Scan(&gotFutureDeletedAt))
	require.NotNil(t, gotFutureDeletedAt)
}
