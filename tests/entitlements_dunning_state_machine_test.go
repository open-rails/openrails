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

	ctx := context.Background()

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

	billingDays := 30
	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)

	_, err := suite.BunDB.NewInsert().Model(&models.Product{
		ID:          productID,
		Slug:        "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
		},
		IsActive:  true,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = suite.BunDB.NewInsert().Model(&models.Price{
		ID:               priceID,
		ProductID:        productID,
		DisplayName:      "Test Monthly",
		IsActive:         true,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = suite.BunDB.NewInsert().Model(&models.Subscription{
		ID:                      subID,
		UserID:                  userID,
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
	}).Exec(ctx)
	require.NoError(t, err)

	paidEnt := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: "premium",
		StartAt:     periodStart,
		EndAt:       &paidEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &subID,
		CreatedAt:   clock.Now().UTC(),
		UpdatedAt:   clock.Now().UTC(),
	}
	_, err = suite.BunDB.NewInsert().Model(paidEnt).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = suite.BunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	// (1) Subscription entitlement is active until paidEnd.
	ok, err := rt.EntitlementService.IsEntitled(ctx, userID, "premium", paidEnd.Add(-time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", paidEnd.Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)

	// Jump time to the paid term end.
	clock.Advance(paidEnd.Sub(clock.Now().UTC()))

	countGrace := func() int {
		n, err := suite.BunDB.NewSelect().
			Model((*models.Entitlement)(nil)).
			Where("user_id = ? AND entitlement = ?", userID, "premium").
			Where("source_type = ?", models.EntitlementSourceGrace).
			Where("source_id = ?", subID).
			Count(ctx)
		require.NoError(t, err)
		return n
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
			SubscriptionService: rt.SubscriptionService,
		}
		require.NoError(t, svc.HandleCCBillWebhook(ctx))
	}

	// Fail #1: grace until +3d (end-of-day).
	callRenewalFailure(paidEnd.Add(3 * 24 * time.Hour).Format("2006-01-02"))
	require.Equal(t, 1, countGrace())

	// Fail #2 (during grace): grace until +5d (append).
	clock.Advance(24 * time.Hour)
	callRenewalFailure(paidEnd.Add(5 * 24 * time.Hour).Format("2006-01-02"))
	require.Equal(t, 2, countGrace())

	// Fail #3 (during grace): grace until +7d (append; future window exists).
	clock.Advance(12 * time.Hour)
	callRenewalFailure(paidEnd.Add(7 * 24 * time.Hour).Format("2006-01-02"))
	require.Equal(t, 3, countGrace())

	// Sanity: paid window is unchanged.
	var gotPaid models.Entitlement
	require.NoError(t, suite.BunDB.NewSelect().Model(&gotPaid).Where("id = ?", paidEnt.ID).Scan(ctx))
	require.NotNil(t, gotPaid.EndAt)
	require.Equal(t, paidEnd.UTC(), gotPaid.EndAt.UTC())
	require.Nil(t, gotPaid.RevokedAt)

	// During grace, user should still be entitled.
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)

	// Renewal success occurs mid-way through the grace timeline.
	successAt := paidEnd.Add(4*24*time.Hour + 12*time.Hour)
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
		SubscriptionService:          rt.SubscriptionService,
		SubscriptionLifecycleService: rt.SubscriptionLifecycleService,
		CreditsService:               rt.CreditsService,
		EventLogService:              rt.EventLogService,
	}
	require.NoError(t, webhook.HandleCCBillWebhook(ctx))

	// Grace windows should be cleared: any active grace revoked; any future grace deleted.
	var graceRows []models.Entitlement
	require.NoError(t, suite.BunDB.NewSelect().
		Model(&graceRows).
		WhereAllWithDeleted().
		Where("user_id = ? AND entitlement = ?", userID, "premium").
		Where("source_type = ?", models.EntitlementSourceGrace).
		Where("source_id = ?", subID).
		OrderExpr("start_at ASC").
		Scan(ctx))
	require.Len(t, graceRows, 3)

	for _, gr := range graceRows {
		if gr.StartAt.After(successAt) {
			require.NotNil(t, gr.DeletedAt, "future grace windows should be deleted")
			require.Nil(t, gr.RevokedAt, "future grace windows should not be revoked")
			continue
		}
		// Only the grace window that is active at successAt should be revoked.
		if gr.EndAt != nil && gr.EndAt.After(successAt) {
			require.NotNil(t, gr.RevokedAt, "active grace windows should be revoked")
		}
	}

	expectedPaidEnd := time.Date(
		paidEnd.Add(30*24*time.Hour).Year(),
		paidEnd.Add(30*24*time.Hour).Month(),
		paidEnd.Add(30*24*time.Hour).Day(),
		23, 59, 59, 0, time.UTC,
	)
	var paidWindows []models.Entitlement
	require.NoError(t, suite.BunDB.NewSelect().
		Model(&paidWindows).
		Where("user_id = ? AND entitlement = ?", userID, "premium").
		Where("source_type = ?", models.EntitlementSourceSubscription).
		Where("source_id = ?", subID).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		OrderExpr("start_at ASC").
		Scan(ctx))
	require.GreaterOrEqual(t, len(paidWindows), 2)

	latest := paidWindows[len(paidWindows)-1]
	require.NotNil(t, latest.EndAt)
	require.Equal(t, expectedPaidEnd.UTC(), latest.EndAt.UTC())
	require.True(t, latest.StartAt.UTC().Equal(successAt.UTC()) || latest.StartAt.UTC().After(successAt.UTC()))

	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", successAt.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)
}
