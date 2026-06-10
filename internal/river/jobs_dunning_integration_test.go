//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
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
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_blocks')").
		Scan(&exists))
	if !exists {
		t.Skip("billing.credit_blocks not found; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)

	creditTypeName := "test_credits_" + uuid.New().String()
	creditTypeID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	billingDays := 30
	billingDays32 := int32(billingDays)

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
	tenantSubjectID := dbtest.EnsureTenantSubjectIDPgx(ctx, t, pool, userID)
	paymentMethod := &models.PaymentMethod{
		ID:                   paymentMethodID,
		TenantSubjectID:      tenantSubjectID,
		Processor:            models.ProcessorMobius,
		VaultID:              "vault_" + uuid.New().String(),
		BillingID:            &billingID,
		InitialTransactionID: "txn_initial_" + uuid.New().String(),
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   paymentMethod.ID,
		TenantSubjectID:      paymentMethod.TenantSubjectID,
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
		TenantSubjectID:         tenantSubjectID,
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
		TenantSubjectID:         sub.TenantSubjectID,
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
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_blocks WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_transactions WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_balances WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_types WHERE id = $1", creditTypeID)
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
	creditsSvc := credits.NewCreditsService(dbi, nil)

	require.True(t, worker.processSubscription(ctx, sub, lifecycle, priceSvc, creditsSvc))

	var depositCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM billing.credit_transactions
		 WHERE tenant_subject_id = $1 AND credit_type_id = $2
		   AND transaction_type = 'deposit' AND source = 'subscription_renewal'`,
		tenantSubjectID, creditTypeID).Scan(&depositCount))
	require.Equal(t, 1, depositCount)
}
