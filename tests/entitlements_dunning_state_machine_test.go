//go:build integration

package tests

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/stretchr/testify/require"
)

func TestEntitlementsDunningStateMachine_CCBill(t *testing.T) {
	suite := setupTestSuite(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)
	require.NotNil(t, rt.DB)
	require.NotNil(t, rt.EntitlementService)
	require.NotNil(t, rt.SubscriptionService)
	require.NotNil(t, rt.SubscriptionLifecycleService)

	ctx := suite.MerchantCtx()

	// Keep simulated times in the past relative to the DB server's NOW() to avoid
	// constraints like chk_payment_not_future during RenewalSuccess.
	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-90 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)
	require.IsType(t, &clockwork.FakeClock{}, clock)

	userID := uuid.New().String()
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()

	billingDays := 720
	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)

	suite.InsertProduct(ctx, &models.Product{
		ID:          productID,
		Key:         "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
		},
		Archived:  false,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	})

	suite.InsertPrice(ctx, &models.Price{
		ID:                  priceID,
		ProductID:           productID,
		Archived:            false,
		Amount:              9_990_000,
		Currency:            "usd",
		AccessDurationHours: &billingDays, AutoRenew: true,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	})

	suite.InsertSubscription(ctx, &models.Subscription{
		ID:                    subID,
		CustomerID:            suite.ensureCustomer(ctx, userID),
		ProductID:             productID,
		PriceID:               priceID,
		Status:                models.StatusActive,
		Rail:                  models.RailCCBill,
		RailSubscriptionID:    ccbillSubID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &paidEnd,
		StartedAt:             clock.Now().UTC(),
		CreatedAt:             clock.Now().UTC(),
		UpdatedAt:             clock.Now().UTC(),
	})

	// Seed through the REAL projection (#691): an active auto-renew sub gets a
	// STANDING window (end_at NULL); paid-through lives on the subscription.
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
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", suite.ensureCustomer(ctx, userID))
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// (1) #691 standing access: entitled inside the paid window AND past its
	// end — access ends only by proven closure, never by clock.
	ok, err := rt.EntitlementService.IsEntitled(ctx, userID, "premium", paidEnd.Add(-time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", paidEnd.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok, "standing window: paid-window lapse alone never removes access (#691)")

	// Jump time to the paid term end.
	clock.Advance(paidEnd.Sub(clock.Now().UTC()))

	countGrace := func() int {
		return suite.Count(ctx, `
			SELECT COUNT(*) FROM openrails.entitlements
			WHERE customer_id = $1 AND entitlement = $2
			  AND source_type = $3 AND source_id = $4
			  AND deleted_at IS NULL`,
			suite.ensureCustomer(ctx, userID), "premium",
			string(models.EntitlementSourceGrace), subID)
	}
	callRenewalFailure := func(nextRetryDate string) {
		body, err := json.Marshal(webhooks.CCBillRenewalFailureEvent{
			TransactionID:  "txn_" + uuid.New().String(),
			SubscriptionID: ccbillSubID,
			ClientAccnum:   "1234",
			ClientSubacc:   "0000",
			Timestamp:      clock.Now().UTC().Format("2006-01-02 15:04:05"),
			NextRetryDate:  nextRetryDate,
			FailureCode:    "declined",
			FailureReason:  "declined",
		})
		require.NoError(t, err)

		svc := &webhooks.CCBillWebhookService{
			Data: webhooks.CCBillWebhookEvent{
				EventType: webhooks.EventTypeRenewalFailure,
				EventBody: body,
			},
			DB:                  rt.DB,
			Clock:               clock,
			CCBillClient:        testCCBillWebhookClient(),
			SubscriptionService: rt.SubscriptionService,
		}
		require.NoError(t, svc.HandleCCBillWebhook(ctx))
	}

	// Fail #1: past_due + grace_ends_at PACING marker on the sub (from
	// nextRetryDate). #691 deleted grace entitlement appends — access is the
	// untouched standing window, so ZERO grace rows ever exist.
	grace1 := paidEnd.Add(3 * 24 * time.Hour)
	callRenewalFailure(grace1.Format("2006-01-02"))
	require.Equal(t, 0, countGrace(), "#691: no grace entitlement windows are appended")
	sub := suite.GetSubscription(subID)
	require.Equal(t, models.StatusPastDue, sub.Status)
	require.NotNil(t, sub.GraceEndsAt)

	// Fails #2/#3 (during dunning): state unchanged, still zero grace rows.
	clock.Advance(24 * time.Hour)
	callRenewalFailure(paidEnd.Add(5 * 24 * time.Hour).Format("2006-01-02"))
	clock.Advance(12 * time.Hour)
	callRenewalFailure(paidEnd.Add(7 * 24 * time.Hour).Format("2006-01-02"))
	require.Equal(t, 0, countGrace())
	require.Equal(t, models.StatusPastDue, suite.GetSubscription(subID).Status)

	// Throughout dunning the user stays entitled — the standing window is
	// untouched (dunning never gates access, #691).
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)

	// Renewal success occurs mid-way through the grace timeline.
	successAt := paidEnd.Add(2 * 24 * time.Hour)
	clock.Advance(successAt.Sub(clock.Now().UTC()))

	successBody, err := json.Marshal(webhooks.CCBillRenewalSuccessEvent{
		TransactionID:      "txn_" + uuid.New().String(),
		SubscriptionID:     ccbillSubID,
		ClientAccnum:       "1234",
		ClientSubacc:       "0000",
		Timestamp:          clock.Now().UTC().Format("2006-01-02 15:04:05"),
		BilledAmount:       "9.99",
		BilledCurrencyCode: "usd",
		NextRenewalDate:    paidEnd.Add(30 * 24 * time.Hour).Format("2006-01-02"),
	})
	require.NoError(t, err)

	webhook := &webhooks.CCBillWebhookService{
		Data: webhooks.CCBillWebhookEvent{
			EventType: webhooks.EventTypeRenewalSuccess,
			EventBody: successBody,
		},
		DB:                           rt.DB,
		Clock:                        clock,
		CCBillClient:                 testCCBillWebhookClient(),
		SubscriptionService:          rt.SubscriptionService,
		SubscriptionLifecycleService: rt.SubscriptionLifecycleService,
		MoneyService:                 rt.MoneyService,
	}
	require.NoError(t, webhook.HandleCCBillWebhook(ctx))

	// #691: no grace rows ever existed, renewal appends no windows — the sub
	// keeps its ONE standing window; the renewal advances the paid-through FACT.
	require.Equal(t, 0, countGrace())

	expectedPaidEnd := time.Date(
		paidEnd.Add(30*24*time.Hour).Year(),
		paidEnd.Add(30*24*time.Hour).Month(),
		paidEnd.Add(30*24*time.Hour).Day(),
		23, 59, 59, 0, time.UTC,
	)
	sub = suite.GetSubscription(subID)
	require.Equal(t, models.StatusActive, sub.Status)
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(expectedPaidEnd), "renewal advances the paid-through fact")
	require.Nil(t, sub.GraceEndsAt)

	paidWindows := suite.QueryEntitlements(ctx, `
		WHERE customer_id = $1 AND entitlement = $2
		  AND source_type = $3 AND source_id = $4
		  AND revoked_at IS NULL
		  AND deleted_at IS NULL
		ORDER BY start_at ASC`,
		suite.ensureCustomer(ctx, userID), "premium",
		string(models.EntitlementSourceSubscription), subID)
	require.Len(t, paidWindows, 1, "one standing window, no per-period appends (#691)")
	require.Nil(t, paidWindows[0].EndAt, "the window is standing")

	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", successAt.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)
}
