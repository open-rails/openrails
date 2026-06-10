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
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestCCBillRenewalSuccess_GrantsCreditsOnce(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_blocks')").
		Scan(&exists))
	if !exists {
		t.Skip("billing.credit_blocks not found; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)
	billingDays := int32(30)

	creditTypeName := "test_credits_" + uuid.New().String()
	creditTypeID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureTenantSubjectIDPgx(ctx, t, pool, userID)
	ccbillSubID := "ccbill_sub_" + uuid.New().String()

	_, err := q.CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            creditTypeID,
		Name:          creditTypeName,
		DisplayName:   "Test Credits",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	})
	require.NoError(t, err)

	creditsSpecJSON, err := json.Marshal(models.CreditsSpec{
		creditTypeName: {Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
	})
	require.NoError(t, err)
	description := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: &description,
		CreditsSpec: creditsSpecJSON,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
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

	periodEnd := now.Add(30 * 24 * time.Hour)
	periodStart := now
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                      subID,
		TenantSubjectID:         tenantSubjectID,
		ProductID:               productID,
		PriceID:                 &priceID,
		Status:                  string(models.StatusActive),
		Processor:               string(models.ProcessorCCBill),
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &periodEnd,
		StartedAt:               now,
		CreatedAt:               now,
		UpdatedAt:               now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_blocks WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_transactions WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_balances WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_types WHERE id = $1", creditTypeID)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
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

	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM billing.credit_transactions
		 WHERE tenant_subject_id = $1 AND credit_type_id = $2
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'`,
		tenantSubjectID, creditTypeID).Scan(&depositCount))
	require.Equal(t, 1, depositCount)
}
