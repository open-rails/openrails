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
	"github.com/open-rails/openrails/internal/modules/money"
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
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='money_blocks')").
		Scan(&exists))
	if !exists {
		t.Skip("openrails.money_blocks not found; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)
	billingDays := int32(30)

	// The grant spec key is just a label now (#472: money has no credit_type).
	grantLabel := "test_credits_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	ccbillSubID := "ccbill_sub_" + uuid.New().String()

	// Unit "USD" so the grant deposits into the USD money balance.
	creditsSpecJSON, err := json.Marshal(models.CreditsSpec{
		grantLabel: {Unit: "USD", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
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
		CustomerID:              tenantSubjectID,
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_balances WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
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
	moneySvc := money.NewMoneyService(dbi)

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
		MoneyService:                 moneySvc,
	}

	require.NoError(t, svc.handleRenewalSuccess(ctx))

	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.money_transactions
		 WHERE customer_id = $1 AND currency = 'USD'
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'`,
		tenantSubjectID).Scan(&depositCount))
	require.Equal(t, 1, depositCount)
}
