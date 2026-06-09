//go:build integration

package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestHandleAddSubscription_ActivatesPendingWithSettledTransactionMetadata(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	svc, bunDB, ids := setupNMIAddSubscriptionTest(t, dsn, true)
	ctx := context.Background()

	require.NoError(t, svc.handleAddSubscription(ctx))

	var gotSub models.Subscription
	require.NoError(t, bunDB.NewSelect().Model(&gotSub).Where("id = ?", ids.subscriptionID).Scan(ctx))
	require.Equal(t, models.StatusActive, gotSub.Status)
	require.NotNil(t, gotSub.CurrentPeriodStartsAt)
	require.NotNil(t, gotSub.CurrentPeriodEndsAt)
	require.Equal(t, time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC), gotSub.CurrentPeriodEndsAt.UTC())

	var payment models.Payment
	require.NoError(t, bunDB.NewSelect().Model(&payment).Where("transaction_id = ?", ids.transactionID).Scan(ctx))
	require.Equal(t, ids.tenantSubjectID, payment.TenantSubjectID)
	require.Equal(t, ids.subscriptionID, *payment.SubscriptionID)

	exists, err := bunDB.NewSelect().Model((*models.Entitlement)(nil)).
		Where("tenant_subject_id = ?", ids.tenantSubjectID).
		Where("entitlement = ?", "premium").
		Where("source_type = ?", models.EntitlementSourceSubscription).
		Where("source_id = ?", ids.subscriptionID).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		Exists(ctx)
	require.NoError(t, err)
	require.True(t, exists)

	svc.Data = NMIWebhookEvent{EventID: uuid.New().String(), EventType: string(EventTypeNMITransactionSuccess), EventBody: ids.transactionBody}
	require.NoError(t, svc.handleTransactionSaleSuccess(ctx))
	count, err := bunDB.NewSelect().Model((*models.Payment)(nil)).Where("transaction_id = ?", ids.transactionID).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestHandleAddSubscription_WithoutSettledTransactionMetadataStaysPending(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	svc, bunDB, ids := setupNMIAddSubscriptionTest(t, dsn, false)
	ctx := context.Background()

	require.NoError(t, svc.handleAddSubscription(ctx))

	var gotSub models.Subscription
	require.NoError(t, bunDB.NewSelect().Model(&gotSub).Where("id = ?", ids.subscriptionID).Scan(ctx))
	require.Equal(t, models.StatusPending, gotSub.Status)

	count, err := bunDB.NewSelect().Model((*models.Payment)(nil)).Where("transaction_id = ?", ids.transactionID).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

type nmiAddSubscriptionTestIDs struct {
	userID          string
	tenantSubjectID uuid.UUID
	subscriptionID  uuid.UUID
	transactionID   string
	transactionBody []byte
}

func setupNMIAddSubscriptionTest(t *testing.T, dsn string, includeTransactionMetadata bool) (*NMIWebhookService, *bun.DB, nmiAddSubscriptionTestIDs) {
	t.Helper()

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Date(2026, time.May, 18, 13, 28, 21, 0, time.UTC)
	fakeClock := clockwork.NewFakeClockAt(now)
	provider := string(models.ProcessorMobius)
	planID := "premium_test_" + uuid.New().String()
	providerSubID := "nmi_sub_" + uuid.New().String()
	transactionID := "txn_" + uuid.New().String()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureTenantSubjectID(ctx, t, bunDB, userID)
	productID := uuid.New()
	priceID := uuid.New()
	subscriptionID := uuid.New()
	durationDays := 30

	product := &models.Product{
		ID:          productID,
		Slug:        "nmi_add_subscription_" + uuid.New().String(),
		DisplayName: "Premium Membership",
		Description: "Test premium product",
		EntitlementsSpec: map[string]*int{
			"premium": &durationDays,
		},
		Status:    models.CatalogStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	price := &models.Price{
		ID:        priceID,
		ProductID: productID,
		Status:    models.CatalogStatusActive,
		Amount:    2399,
		Currency:  "USD",
		BillingCycleDays: func() *int {
			days := 30
			return &days
		}(),
		Processors: map[string]map[string]string{
			provider: {
				models.ProcessorKeyPlanID:   planID,
				models.ProcessorKeyProvider: provider,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	metadata := map[string]any{
		"order_id": "order_" + uuid.New().String(),
	}
	if includeTransactionMetadata {
		metadata["provider_transaction_id"] = transactionID
	}
	metadataBytes, err := json.Marshal(metadata)
	require.NoError(t, err)
	subscription := &models.Subscription{
		ID:                       subscriptionID,
		TenantSubjectID:          tenantSubjectID,
		ProductID:                productID,
		PriceID:                  priceID,
		EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(product.EntitlementsSpec),
		Status:                   models.StatusPending,
		Processor:                models.ProcessorMobius,
		ProcessorSubscriptionID:  providerSubID,
		StartedAt:                now,
		Metadata:                 metadataBytes,
		CreatedAt:                now,
		UpdatedAt:                now,
	}

	_, err = bunDB.NewInsert().Model(product).Exec(ctx)
	require.NoError(t, err)
	_, err = bunDB.NewInsert().Model(price).Exec(ctx)
	require.NoError(t, err)
	_, err = bunDB.NewInsert().Model(subscription).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.NotificationQueue)(nil)).Where("tenant_subject_id = ?", tenantSubjectID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("tenant_subject_id = ?", tenantSubjectID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Payment)(nil)).Where("tenant_subject_id = ?", tenantSubjectID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subscriptionID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(context.Background())
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(context.Background())
	})

	body, err := json.Marshal(NMIRecurringEventBody{
		SubscriptionID: Stringish(providerSubID),
		NextChargeDate: Stringish("2026-06-17"),
		Plan: &NMIPlan{
			ID:     Stringish(planID),
			Amount: Stringish("23.99"),
		},
	})
	require.NoError(t, err)
	transactionBody, err := json.Marshal(NMITransactionEventBody{
		TransactionID: Stringish(transactionID),
		Amount:        Stringish("23.99"),
		Currency:      Stringish("USD"),
		Subscription: &NMISubscriptionRef{
			SubscriptionID: Stringish(providerSubID),
		},
	})
	require.NoError(t, err)

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, fakeClock)
	paymentSvc := payments.NewPaymentService(dbi, fakeClock)
	subscriptionSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil, fakeClock)
	lifecycleSvc := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, nil, paymentSvc, nil, fakeClock)

	return &NMIWebhookService{
			DB:                           dbi,
			Clock:                        fakeClock,
			PriceService:                 priceSvc,
			ProductService:               productSvc,
			Data:                         NMIWebhookEvent{EventID: uuid.New().String(), EventType: string(EventTypeNMIAddSubscription), EventBody: body},
			Processor:                    provider,
			SubscriptionService:          subscriptionSvc,
			PaymentService:               paymentSvc,
			SubscriptionLifecycleService: lifecycleSvc,
		}, bunDB, nmiAddSubscriptionTestIDs{
			userID:          userID,
			tenantSubjectID: tenantSubjectID,
			subscriptionID:  subscriptionID,
			transactionID:   transactionID,
			transactionBody: transactionBody,
		}
}
