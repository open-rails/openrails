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
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	// Money is now a #514 grant ledger: credit deposits are rows in
	// openrails.grants (kind='credit'); the old money_blocks/money_transactions
	// tables were dropped (#512). Guard on the current table.
	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name='grants')",
		dbi.DataPool().Schema()).
		Scan(&exists))
	if !exists {
		t.Skip("grants not found in the configured schema; run migrations before integration tests")
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
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: &description,
		CreditsSpec: creditsSpecJSON,
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
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

	periodEnd := now.Add(30 * 24 * time.Hour)
	periodStart := now
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    subID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               &priceID,
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailCCBill),
		RailSubscriptionID:    ccbillSubID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &periodEnd,
		StartedAt:             now,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		// Deposits are now #514 credit grants (openrails.grants) with their
		// double-entry movement in openrails.ledger_transfers (#512).
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.ledger_transfers WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE customer_id = $1", tenantSubjectID)
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
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc)
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

	// The renewal deposit is a #514 credit grant: a single openrails.grants row
	// (kind='credit', event='grant') whose source_type maps from the deposit
	// source ("subscription_renewal" -> 'subscription'). The grant key is
	// idempotent per (subscription, period_end), so re-delivery never doubles it.
	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.grants
		 WHERE customer_id = $1 AND currency = 'USD'
		   AND kind = 'credit' AND event = 'grant' AND source_type = 'subscription'`,
		tenantSubjectID).Scan(&depositCount))
	require.Equal(t, 1, depositCount)

	// The deposit also posts a double-entry movement into the credit balance.
	var transferCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.ledger_transfers
		 WHERE customer_id = $1 AND currency = 'USD'`,
		tenantSubjectID).Scan(&transferCount))
	require.Equal(t, 1, transferCount)
}
