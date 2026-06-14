//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/stretchr/testify/require"
)

func TestEntitlementsDunningStateMachine_CCBill_TerminalExpiration(t *testing.T) {
	suite := setupTestSuite(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)
	require.NotNil(t, rt.DB)
	require.NotNil(t, rt.EntitlementService)
	require.NotNil(t, rt.SubscriptionService)
	require.NotNil(t, rt.SubscriptionLifecycleService)

	ctx := dbtest.WithTestTenant(context.Background())

	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-120 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)
	require.IsType(t, &clockwork.FakeClock{}, clock)

	productID := uuid.New()
	priceID := uuid.New()

	billingDays := 30
	suite.InsertProduct(ctx, &models.Product{
		ID:          productID,
		Slug:        "test_ccbill_multi_" + uuid.New().String()[:8],
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
			"extra":   nil,
		},
		Status:    models.CatalogStatusActive,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	})

	suite.InsertPrice(ctx, &models.Price{
		ID:               priceID,
		ProductID:        productID,
		Status:           models.CatalogStatusActive,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	})

	userID := uuid.New().String()
	tenantSubjectID := suite.ensureCustomer(ctx, userID)
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()

	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)

	suite.InsertSubscription(ctx, &models.Subscription{
		ID:                      subID,
		CustomerID:              tenantSubjectID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorCCBill,
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               clock.Now().UTC(),
		CreatedAt:               clock.Now().UTC(),
		UpdatedAt:               clock.Now().UTC(),
	})

	// Paid windows for both entitlements.
	for _, entName := range []string{"premium", "extra"} {
		notBefore := periodStart.UTC()
		endAt := paidEnd.UTC()
		_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
			UserID:      userID,
			Entitlement: entName,
			NotBefore:   &notBefore,
			EndAt:       &endAt,
			SourceType:  models.EntitlementSourceSubscription,
			SourceID:    subID,
		})
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", tenantSubjectID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	clock.Advance(paidEnd.Sub(clock.Now().UTC()))

	sendRenewalFailure := func(nextRetryDate string) {
		body, err := json.Marshal(webhooks.CCBillRenewalFailureEvent{
			TransactionID:  "txn_" + uuid.New().String(),
			SubscriptionID: ccbillSubID,
			ClientAccnum:   "1234",
			ClientSubacc:   "0000",
			Timestamp:      clock.Now().UTC().Format("2006-01-02 15:04:05"),
			NextRetryDate:  nextRetryDate,
		})
		require.NoError(t, err)

		svc := &webhooks.CCBillWebhookService{
			Data: webhooks.CCBillWebhookEvent{
				EventType: webhooks.EventTypeRenewalFailure,
				EventBody: body,
			},
			DB:                  rt.DB,
			Clock:               clock,
			SubscriptionService: rt.SubscriptionService,
		}
		require.NoError(t, svc.HandleCCBillWebhook(ctx))
	}

	// Build a grace tail with a future window, then expire during the active grace window.
	sendRenewalFailure(paidEnd.Add(3 * 24 * time.Hour).Format("2006-01-02"))
	sendRenewalFailure(paidEnd.Add(5 * 24 * time.Hour).Format("2006-01-02"))
	sendRenewalFailure(paidEnd.Add(7 * 24 * time.Hour).Format("2006-01-02"))

	expireAt := paidEnd.Add(4 * 24 * time.Hour)
	clock.Advance(expireAt.Sub(clock.Now().UTC()))

	expBody, err := json.Marshal(webhooks.CCBillExpirationEvent{
		CCBillCommonFields: webhooks.CCBillCommonFields{
			SubscriptionID: ccbillSubID,
			ClientAccnum:   "1234",
			ClientSubacc:   "0000",
			Timestamp:      clock.Now().UTC().Format("2006-01-02 15:04:05"),
		},
	})
	require.NoError(t, err)

	expSvc := &webhooks.CCBillWebhookService{
		Data: webhooks.CCBillWebhookEvent{
			EventType: webhooks.EventTypeExpiration,
			EventBody: expBody,
		},
		DB:                           rt.DB,
		Clock:                        clock,
		SubscriptionService:          rt.SubscriptionService,
		SubscriptionLifecycleService: rt.SubscriptionLifecycleService,
		NotificationService:          rt.NotificationService,
		EventLogService:              rt.EventLogService,
	}
	require.NoError(t, expSvc.HandleCCBillWebhook(ctx))

	// Terminal failure: grace should not keep access after expiration.
	for _, entName := range []string{"premium", "extra"} {
		ok, err := rt.EntitlementService.IsEntitled(ctx, userID, entName, clock.Now().UTC().Add(time.Second))
		require.NoError(t, err)
		require.False(t, ok)
	}

	// No new paid window should have been pushed.
	for _, entName := range []string{"premium", "extra"} {
		n := suite.Count(ctx, `
			SELECT COUNT(*) FROM openrails.entitlements
			WHERE customer_id = $1 AND entitlement = $2
			  AND source_type = $3 AND source_id = $4
			  AND deleted_at IS NULL`,
			tenantSubjectID, entName, string(models.EntitlementSourceSubscription), subID)
		require.Equal(t, 1, n)
	}
}

func TestEntitlementsDunningStateMachine_CCBill_DuplicateRenewalSuccess(t *testing.T) {
	suite := setupTestSuite(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)
	require.NotNil(t, rt.DB)
	require.NotNil(t, rt.DeduplicationService)
	require.NotNil(t, rt.EntitlementService)
	require.NotNil(t, rt.SubscriptionService)
	require.NotNil(t, rt.SubscriptionLifecycleService)

	ctx := dbtest.WithTestTenant(context.Background())
	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-120 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)

	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 30

	suite.InsertProduct(ctx, &models.Product{
		ID:          productID,
		Slug:        "test_ccbill_dupe_" + uuid.New().String()[:8],
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
		},
		Status:    models.CatalogStatusActive,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	})

	suite.InsertPrice(ctx, &models.Price{
		ID:               priceID,
		ProductID:        productID,
		Status:           models.CatalogStatusActive,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	})

	userID := uuid.New().String()
	tenantSubjectID := suite.ensureCustomer(ctx, userID)
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()

	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)

	suite.InsertSubscription(ctx, &models.Subscription{
		ID:                      subID,
		CustomerID:              tenantSubjectID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorCCBill,
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               clock.Now().UTC(),
		CreatedAt:               clock.Now().UTC(),
		UpdatedAt:               clock.Now().UTC(),
	})

	notBefore := periodStart.UTC()
	endAt := paidEnd.UTC()
	_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		NotBefore:   &notBefore,
		EndAt:       &endAt,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    subID,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", tenantSubjectID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	clock.Advance(paidEnd.Sub(clock.Now().UTC()))
	successAt := paidEnd.Add(2 * time.Hour)
	clock.Advance(successAt.Sub(clock.Now().UTC()))

	txid := "txn_dupe_" + uuid.New().String()
	nextRenewal := paidEnd.Add(30 * 24 * time.Hour).Format("2006-01-02")
	successBody, err := json.Marshal(webhooks.CCBillRenewalSuccessEvent{
		TransactionID:      txid,
		SubscriptionID:     ccbillSubID,
		ClientAccnum:       "1234",
		ClientSubacc:       "0000",
		Timestamp:          clock.Now().UTC().Format("2006-01-02 15:04:05"),
		BilledAmount:       "9.99",
		BilledCurrencyCode: "usd",
		NextRenewalDate:    nextRenewal,
	})
	require.NoError(t, err)

	webhook := &webhooks.CCBillWebhookService{
		Data: webhooks.CCBillWebhookEvent{
			EventType: webhooks.EventTypeRenewalSuccess,
			EventBody: successBody,
		},
		DB:                           rt.DB,
		Clock:                        clock,
		DeduplicationService:         rt.DeduplicationService,
		SubscriptionService:          rt.SubscriptionService,
		SubscriptionLifecycleService: rt.SubscriptionLifecycleService,
		NotificationService:          rt.NotificationService,
		EventLogService:              rt.EventLogService,
		MoneyService:                 rt.MoneyService,
	}

	require.NoError(t, webhook.HandleCCBillWebhook(ctx))
	require.NoError(t, webhook.HandleCCBillWebhook(ctx)) // duplicate

	// Only one renewal window should exist for this subscription.
	n := suite.Count(ctx, `
		SELECT COUNT(*) FROM openrails.entitlements
		WHERE customer_id = $1 AND entitlement = $2
		  AND source_type = $3 AND source_id = $4
		  AND deleted_at IS NULL`,
		suite.ensureCustomer(ctx, userID), "premium",
		string(models.EntitlementSourceSubscription), subID)
	require.Equal(t, 2, n) // original + one renewal
}
