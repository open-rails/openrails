//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestHandleAddSubscription_ActivatesPendingWithSettledTransactionMetadata(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	svc, dbi, ids := setupNMIAddSubscriptionTest(t, dsn, true)
	ctx := context.Background()
	pool := dbi.Pool()
	q := gen.New(pool)

	require.NoError(t, svc.handleAddSubscription(ctx))

	gotSub, err := q.GetSubscriptionByID(ctx, ids.subscriptionID)
	require.NoError(t, err)
	require.Equal(t, string(models.StatusActive), string(gotSub.Status))
	require.NotNil(t, gotSub.CurrentPeriodStartsAt)
	require.NotNil(t, gotSub.CurrentPeriodEndsAt)
	require.Equal(t, time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC), gotSub.CurrentPeriodEndsAt.UTC())

	var paymentTenantSubjectID uuid.UUID
	var paymentSubscriptionID *uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT tenant_subject_id, subscription_id FROM openrails.payments WHERE transaction_id = $1",
		ids.transactionID).Scan(&paymentTenantSubjectID, &paymentSubscriptionID))
	require.Equal(t, ids.tenantSubjectID, paymentTenantSubjectID)
	require.NotNil(t, paymentSubscriptionID)
	require.Equal(t, ids.subscriptionID, *paymentSubscriptionID)

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM openrails.entitlements
			WHERE tenant_subject_id = $1
			  AND entitlement = $2
			  AND source_type = $3
			  AND source_id = $4
			  AND revoked_at IS NULL
			  AND deleted_at IS NULL
		)`,
		ids.tenantSubjectID, "premium", string(models.EntitlementSourceSubscription), ids.subscriptionID,
	).Scan(&exists))
	require.True(t, exists)

	svc.Data = NMIWebhookEvent{EventID: uuid.New().String(), EventType: string(EventTypeNMITransactionSuccess), EventBody: ids.transactionBody}
	require.NoError(t, svc.handleTransactionSaleSuccess(ctx))
	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE transaction_id = $1", ids.transactionID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestHandleTransactionSaleSuccess_AcksDuplicateInitialChargeOnActiveSubscription(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	svc, dbi, ids := setupNMIAddSubscriptionTest(t, dsn, true)
	ctx := context.Background()
	pool := dbi.Pool()

	// Activate the subscription and record the initial charge (the analogue of
	// the synchronous saved-card checkout activation, #330).
	require.NoError(t, svc.handleAddSubscription(ctx))

	// Live NMI sale.success notifications for the initial charge carry no
	// explicit subscription reference — only the order id — and arrive after
	// the subscription is already active. The already-recorded transaction
	// must be acknowledged as a duplicate, not surfaced as a webhook error.
	implicitBody, err := json.Marshal(NMITransactionEventBody{
		TransactionID: Stringish(ids.transactionID),
		Amount:        Stringish("23.99"),
		Currency:      Stringish("USD"),
		OrderID:       Stringish(ids.providerSubID),
	})
	require.NoError(t, err)
	svc.Data = NMIWebhookEvent{EventID: uuid.New().String(), EventType: string(EventTypeNMITransactionSuccess), EventBody: implicitBody}
	require.NoError(t, svc.handleTransactionSaleSuccess(ctx))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE transaction_id = $1", ids.transactionID).Scan(&count))
	require.Equal(t, 1, count)
	gotSub, err := gen.New(pool).GetSubscriptionByID(ctx, ids.subscriptionID)
	require.NoError(t, err)
	require.Equal(t, string(models.StatusActive), string(gotSub.Status))

	// A non-recurring transaction that is NOT already recorded must still be
	// rejected rather than mutate the active subscription.
	unknownBody, err := json.Marshal(NMITransactionEventBody{
		TransactionID: Stringish("txn_unknown_" + uuid.New().String()),
		Amount:        Stringish("23.99"),
		Currency:      Stringish("USD"),
		OrderID:       Stringish(ids.providerSubID),
	})
	require.NoError(t, err)
	svc.Data = NMIWebhookEvent{EventID: uuid.New().String(), EventType: string(EventTypeNMITransactionSuccess), EventBody: unknownBody}
	require.Error(t, svc.handleTransactionSaleSuccess(ctx))
}

func TestHandleAddSubscription_WithoutSettledTransactionMetadataStaysPending(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	svc, dbi, ids := setupNMIAddSubscriptionTest(t, dsn, false)
	ctx := context.Background()
	pool := dbi.Pool()

	require.NoError(t, svc.handleAddSubscription(ctx))

	gotSub, err := gen.New(pool).GetSubscriptionByID(ctx, ids.subscriptionID)
	require.NoError(t, err)
	require.Equal(t, string(models.StatusPending), string(gotSub.Status))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE transaction_id = $1", ids.transactionID).Scan(&count))
	require.Equal(t, 0, count)
}

type nmiAddSubscriptionTestIDs struct {
	userID          string
	tenantSubjectID uuid.UUID
	subscriptionID  uuid.UUID
	providerSubID   string
	transactionID   string
	transactionBody []byte
}

func setupNMIAddSubscriptionTest(t *testing.T, dsn string, includeTransactionMetadata bool) (*NMIWebhookService, *db.DB, nmiAddSubscriptionTestIDs) {
	t.Helper()

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Date(2026, time.May, 18, 13, 28, 21, 0, time.UTC)
	fakeClock := clockwork.NewFakeClockAt(now)
	provider := string(models.ProcessorMobius)
	planID := "premium_test_" + uuid.New().String()
	providerSubID := "nmi_sub_" + uuid.New().String()
	transactionID := "txn_" + uuid.New().String()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureTenantSubjectIDPgx(ctx, t, pool, userID)
	productID := uuid.New()
	priceID := uuid.New()
	subscriptionID := uuid.New()
	durationDays := 30

	entitlementsSpec := map[string]*int{
		"premium": &durationDays,
	}
	entitlementsSpecJSON, err := json.Marshal(entitlementsSpec)
	require.NoError(t, err)

	description := "Test premium product"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:               productID,
		Slug:             "nmi_add_subscription_" + uuid.New().String(),
		DisplayName:      "Premium Membership",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Status:           string(models.CatalogStatusActive),
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	processorsJSON, err := json.Marshal(map[string]map[string]string{
		provider: {
			models.ProcessorKeyPlanID:   planID,
			models.ProcessorKeyProvider: provider,
		},
	})
	require.NoError(t, err)
	billingCycleDays := int32(30)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               priceID,
		ProductID:        productID,
		Amount:           2399,
		Currency:         "USD",
		Status:           string(models.CatalogStatusActive),
		BillingCycleDays: &billingCycleDays,
		Processors:       processorsJSON,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	metadata := map[string]any{
		"order_id": "order_" + uuid.New().String(),
	}
	if includeTransactionMetadata {
		metadata["provider_transaction_id"] = transactionID
	}
	metadataBytes, err := json.Marshal(metadata)
	require.NoError(t, err)

	snapshotJSON, err := json.Marshal(models.CloneEntitlementsSpec(entitlementsSpec))
	require.NoError(t, err)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                       subscriptionID,
		TenantSubjectID:          tenantSubjectID,
		ProductID:                productID,
		PriceID:                  &priceID,
		EntitlementsSpecSnapshot: snapshotJSON,
		Status:                   string(models.StatusPending),
		Processor:                string(models.ProcessorMobius),
		ProcessorSubscriptionID:  providerSubID,
		StartedAt:                now,
		GatewayResponse:          metadataBytes,
		CreatedAt:                now,
		UpdatedAt:                now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.notification_queue WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.entitlements WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.payments WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subscriptionID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.products WHERE id = $1", productID)
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
		}, dbi, nmiAddSubscriptionTestIDs{
			userID:          userID,
			tenantSubjectID: tenantSubjectID,
			subscriptionID:  subscriptionID,
			providerSubID:   providerSubID,
			transactionID:   transactionID,
			transactionBody: transactionBody,
		}
}
