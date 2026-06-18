//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDunningWorker_RebillSuccess_GrantsCreditsOnce(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name='money_blocks')",
		dbi.DataPool().Schema()).
		Scan(&exists))
	if !exists {
		t.Skip("money_blocks not found in the configured schema; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)

	// The grant spec key is just a label now (#472: money has no credit_type);
	// Unit "USD" deposits into the USD money balance.
	grantLabel := "test_credits_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	billingDays := 30
	billingDays32 := int32(billingDays)

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

	price := &models.Price{
		ID:               priceID,
		ProductID:        productID,
		Status:           models.CatalogStatusActive,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               priceID,
		ProductID:        productID,
		Amount:           999,
		Currency:         "usd",
		Status:           string(models.CatalogStatusActive),
		BillingCycleDays: &billingDays32,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	billingID := "bill_" + uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	paymentMethod := &models.PaymentMethod{
		ID:                   paymentMethodID,
		CustomerID:           tenantSubjectID,
		Processor:            models.ProcessorMobius,
		VaultID:              "vault_" + uuid.New().String(),
		BillingID:            &billingID,
		InitialTransactionID: "txn_initial_" + uuid.New().String(),
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   paymentMethod.ID,
		CustomerID:           paymentMethod.CustomerID,
		Processor:            string(paymentMethod.Processor),
		VaultID:              paymentMethod.VaultID,
		BillingID:            paymentMethod.BillingID,
		InitialTransactionID: paymentMethod.InitialTransactionID,
		CreatedAt:            now,
		UpdatedAt:            now,
	})
	require.NoError(t, err)

	periodEnd := now.Add(-1 * time.Minute)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-30 * time.Second)

	sub := &models.Subscription{
		ID:                      subID,
		CustomerID:              tenantSubjectID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusPastDue,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: "sub_test_" + uuid.New().String(),
		PaymentMethodID:         &paymentMethodID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &periodEnd,
		StartedAt:               periodStart,
		NextRetryAt:             &nextRetry,
		CreatedAt:               now,
		UpdatedAt:               now,
		Price:                   price,
		PaymentMethod:           paymentMethod,
	}
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                      sub.ID,
		MerchantID:              dbtest.TestMerchantID.UUID(),
		CustomerID:              sub.CustomerID,
		ProductID:               sub.ProductID,
		PriceID:                 &priceID,
		Status:                  string(sub.Status),
		Processor:               string(sub.Processor),
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		PaymentMethodID:         sub.PaymentMethodID,
		CurrentPeriodStartsAt:   sub.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:     sub.CurrentPeriodEndsAt,
		StartedAt:               sub.StartedAt,
		NextRetryAt:             sub.NextRetryAt,
		CreatedAt:               now,
		UpdatedAt:               now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Stub NMI direct post endpoint for AttemptManualRebill.
	processorTxnID := "txn_test_" + uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, _ = w.Write([]byte("response=1&transactionid=" + processorTxnID))
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	worker := &DunningWorker{
		DB:         dbi,
		NMIClients: map[string]*nmi.NMIClient{"mobius": client},
	}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	require.Equal(t, dunningOutcomeSucceeded, worker.processSubscription(dbtest.WithTestMerchant(ctx), sub, lifecycle, priceSvc, moneySvc, false))

	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.money_transactions
		 WHERE customer_id = $1 AND currency = 'USD'
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'`,
		tenantSubjectID).Scan(&depositCount))
	require.Equal(t, 1, depositCount)
}

// TestDunningWorker_ConflictRepairFromDurableSuccessfulIntent pins the
// crash-repair contract that the manual_rebill_attempts table used to provide
// and the ledger now does (#358 phase C): a previous pass charged the period
// (durable succeeded intent with the transaction id as evidence) but died
// before the lifecycle update. The next pass's enqueue conflicts on the
// content-addressed key, gets the durable success back, repairs the local
// lifecycle — and never sends a second charge.
func TestDunningWorker_ConflictRepairFromDurableSuccessfulIntent(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)

	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	billingDays32 := int32(30)
	description := "Test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: &description,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "usd",
		Status: string(models.CatalogStatusActive), BillingCycleDays: &billingDays32,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	billingID := "bill_" + uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: paymentMethodID, CustomerID: tenantSubjectID, Processor: "mobius",
		VaultID: "vault_" + uuid.New().String(), BillingID: &billingID,
		InitialTransactionID: "txn_initial_" + uuid.New().String(),
		CreatedAt:            now, UpdatedAt: now,
	})
	require.NoError(t, err)

	periodEnd := now.Add(-1 * time.Minute)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-30 * time.Second)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Processor: "mobius",
		ProcessorSubscriptionID: "sub_test_" + uuid.New().String(),
		PaymentMethodID:         &paymentMethodID,
		CurrentPeriodStartsAt:   &periodStart, CurrentPeriodEndsAt: &periodEnd,
		StartedAt: periodStart, NextRetryAt: &nextRetry,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// The durable success from the crashed pass: attempt ordinal 0
	// (retry_attempts is NULL), evidence carries the charge's transaction id.
	durableTxn := "txn_durable_" + uuid.NewString()[:8]
	orderRef := fmt.Sprintf("rebill-%s-%d", subID, periodEnd.Unix())
	payloadJSON := fmt.Sprintf(
		`{"subscription_id":%q,"period_end":%q,"processor":"mobius","order_reference":%q,"attempt":0}`,
		subID, periodEnd.Format(time.RFC3339), orderRef)
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.provider_intents
		  (provider, intent_type, subscription_id, payload, idempotency_key, status, origin, executed_at, result_evidence, merchant_id)
		VALUES ('mobius', $1, $2, $3, $4, 'succeeded', 'system', now(), $5, $6)`,
		intents.TypeManualRebill, subID, payloadJSON,
		intents.ManualRebillIdempotencyKey(subID, periodEnd, "mobius", orderRef, 0),
		fmt.Sprintf(`{"transaction_id": %q}`, durableTxn), dbtest.TestMerchantID.UUID())
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.provider_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Gateway that REFUSES sales: any charge attempt fails the test premise.
	var saleAttempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("type") == "sale" {
			saleAttempts++
		}
		_, _ = w.Write([]byte("response=3&responsetext=should never be called"))
	}))
	t.Cleanup(srv.Close)
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	worker := &DunningWorker{DB: dbi, NMIClients: map[string]*nmi.NMIClient{"mobius": client}}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	sub, err := repo.NewSubscriptionRepo(dbi).GetByID(ctx, subID)
	require.NoError(t, err)

	outcome := worker.processSubscription(dbtest.WithTestMerchant(ctx), sub, lifecycle, priceSvc, moneySvc, false)
	require.Equal(t, dunningOutcomeSucceeded, outcome)
	assert.Zero(t, saleAttempts, "the durable success must be repaired, never re-charged")

	refreshed, err := dbi.Gen(ctx).GetSubscriptionByID(ctx, subID)
	require.NoError(t, err)
	assert.Equal(t, string(models.StatusActive), string(refreshed.Status), "lifecycle repaired from the ledger")
	require.NotNil(t, refreshed.CurrentPeriodEndsAt)
	assert.True(t, refreshed.CurrentPeriodEndsAt.After(periodEnd))

	var paymentCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2",
		subID, durableTxn).Scan(&paymentCount))
	assert.Equal(t, 1, paymentCount, "repair persisted the durable charge")
}
