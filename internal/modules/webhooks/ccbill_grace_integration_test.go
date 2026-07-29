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

// #691: a CCBill RenewalFailure marks the sub past_due and dates the
// grace_ends_at PACING marker from CCBill's retry schedule, but appends NO
// grace entitlement windows — the auto-renew sub's STANDING window keeps
// access intact through CCBill's dunning.
func TestCCBillRenewalFailure_NoGraceWindows_StandingAccessIntact(t *testing.T) {
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
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
		MerchantID:       dbtest.TestMerchantID.UUID(),
		ID:               productID,
		Key:              "test_product_" + uuid.New().String(),
		DisplayName:      "Test Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Archived:         false,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ID:                  priceID,
		ProductID:           productID,
		Amount:              9_990_000,
		Currency:            "USD",
		Archived:            false,
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		MerchantID:            dbtest.TestMerchantID.UUID(),
		ID:                    subID,
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               &priceID,
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailCCBill),
		RailSubscriptionID:    ccbillSubID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &paidEnd,
		StartedAt:             now,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
	require.NoError(t, err)

	// #691 shape: the paid access window is STANDING (end_at NULL).
	paidEntID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		MerchantID:  dbtest.TestMerchantID.UUID(),
		ID:          paidEntID,
		CustomerID:  tenantSubjectID,
		Entitlement: "premium",
		StartAt:     periodStart,
		EndAt:       nil,
		SourceType:  string(models.EntitlementSourceSubscription),
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", tenantSubjectID)
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

	// The standing access window is UNTOUCHED — dunning never gates access.
	gotPaid, err := q.GetEntitlementByID(ctx, paidEntID)
	require.NoError(t, err)
	require.Nil(t, gotPaid.EndAt, "standing window stays open through a renewal failure")
	require.Nil(t, gotPaid.RevokedAt)

	// NO grace entitlement windows are appended (#691 deleted them).
	var graceCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.entitlements
		 WHERE customer_id = $1 AND source_type = $2 AND source_id = $3`,
		tenantSubjectID, string(models.EntitlementSourceGrace), subID,
	).Scan(&graceCount))
	require.Zero(t, graceCount, "renewal failure must not mint grace windows")

	// The sub is past_due with the PACING marker dated from CCBill's retry
	// schedule (capped by ccbillGraceCap; for these fixture dates the cap wins).
	var status string
	var graceEndsAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, grace_ends_at FROM openrails.subscriptions WHERE id = $1`, subID,
	).Scan(&status, &graceEndsAt))
	require.Equal(t, "past_due", status)
	require.NotNil(t, graceEndsAt)
	expectedGraceEnd := time.Date(nextRetryAt.Year(), nextRetryAt.Month(), nextRetryAt.Day(), 23, 59, 59, 0, time.UTC)
	if maxGraceEnd := paidEnd.UTC().Add(ccbillGraceCap); expectedGraceEnd.After(maxGraceEnd) {
		expectedGraceEnd = maxGraceEnd
	}
	require.Equal(t, expectedGraceEnd.UTC(), graceEndsAt.UTC())
}

func TestCCBillRenewalSuccess_RevokesAndDeletesGraceEntitlements(t *testing.T) {
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
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
		MerchantID:       dbtest.TestMerchantID.UUID(),
		ID:               productID,
		Key:              "test_product_" + uuid.New().String(),
		DisplayName:      "Test Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Archived:         false,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ID:                  priceID,
		ProductID:           productID,
		Amount:              9_990_000,
		Currency:            "USD",
		Archived:            false,
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		MerchantID:            dbtest.TestMerchantID.UUID(),
		ID:                    subID,
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               &priceID,
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailCCBill),
		RailSubscriptionID:    ccbillSubID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &paidEnd,
		StartedAt:             now,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
	require.NoError(t, err)

	paidEntID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		MerchantID:  dbtest.TestMerchantID.UUID(),
		ID:          paidEntID,
		CustomerID:  tenantSubjectID,
		Entitlement: "premium",
		StartAt:     periodStart,
		EndAt:       &paidEnd,
		SourceType:  string(models.EntitlementSourceSubscription),
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	graceEnd := now.Add(2 * 24 * time.Hour)
	graceActiveID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		MerchantID:  dbtest.TestMerchantID.UUID(),
		ID:          graceActiveID,
		CustomerID:  tenantSubjectID,
		Entitlement: "premium",
		StartAt:     paidEnd,
		EndAt:       &graceEnd,
		SourceType:  string(models.EntitlementSourceGrace),
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	graceFutureEnd := graceEnd.Add(24 * time.Hour)
	graceFutureID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		MerchantID:  dbtest.TestMerchantID.UUID(),
		ID:          graceFutureID,
		CustomerID:  tenantSubjectID,
		Entitlement: "premium",
		StartAt:     graceEnd,
		EndAt:       &graceFutureEnd,
		SourceType:  string(models.EntitlementSourceGrace),
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc)
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
