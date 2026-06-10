//go:build integration

package riverjobs

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
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

func TestDunningWorker_RebillSuccess_GrantsCreditsOnce(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

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

	creditTypeName := "test_credits_" + uuid.New().String()
	creditTypeID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	billingDays := 30

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

	product := &models.Product{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: "Test",
		CreditsSpec: models.CreditsSpec{
			creditTypeName: {Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
		},
		Status:    models.CatalogStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err = bunDB.NewInsert().Model(product).Exec(ctx)
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
	_, err = bunDB.NewInsert().Model(price).Exec(ctx)
	require.NoError(t, err)

	billingID := "bill_" + uuid.New().String()
	tenantSubjectID := dbtest.EnsureTenantSubjectID(ctx, t, bunDB, userID)
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
	_, err = bunDB.NewInsert().Model(paymentMethod).Exec(ctx)
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
	_, err = bunDB.NewInsert().Model(sub).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditBlock)(nil)).Where("tenant_subject_id = ?", tenantSubjectID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("tenant_subject_id = ?", tenantSubjectID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditBalance)(nil)).Where("tenant_subject_id = ?", tenantSubjectID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Payment)(nil)).Where("subscription_id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.PaymentMethod)(nil)).Where("id = ?", paymentMethodID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", creditTypeID).Exec(ctx)
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

	depositCount, err := bunDB.NewSelect().
		Model((*models.CreditTransaction)(nil)).
		Where("tenant_subject_id = ? AND credit_type_id = ?", tenantSubjectID, creditTypeID).
		Where("transaction_type = 'deposit' AND source = 'subscription_renewal'").
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, depositCount)
}
