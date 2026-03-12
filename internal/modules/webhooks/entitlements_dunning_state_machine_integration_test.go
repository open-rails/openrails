package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestEntitlements_CCBillDunning_StateMachine(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}

	ctx := context.Background()
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(t0)

	userID := uuid.New().String()
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()

	billingDays := 30
	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour) // 2026-01-31 00:00Z

	_, err = bunDB.NewInsert().Model(&models.Product{
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

	_, err = bunDB.NewInsert().Model(&models.Price{
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

	_, err = bunDB.NewInsert().Model(&models.Subscription{
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
	_, err = bunDB.NewInsert().Model(paidEnt).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	entSvc := entitlements.NewEntitlementService(dbi)
	entSvc.SetClock(clock)
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entSvc, notifSvc, paymentSvc, nil)
	lifecycle.SetClock(clock)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	// (1) Paid entitlement exists and expires at paidEnd.
	ok, err := entSvc.IsEntitled(ctx, userID, "premium", paidEnd.Add(-time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = entSvc.IsEntitled(ctx, userID, "premium", paidEnd.Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)

	// Move time to paid end and process dunning failures (CCBill retry schedule dictates grace).
	clock.Advance(paidEnd.Sub(clock.Now().UTC()))

	failure := func(nextRetryDate string) {
		body, err := json.Marshal(CCBillRenewalFailureEvent{
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

		svc := &CCBillWebhookService{
			Data: CCBillWebhookEvent{
				EventBody: body,
			},
			DB:                  dbi,
			Clock:               clock,
			SubscriptionService: subSvc,
		}
		require.NoError(t, svc.handleRenewalFailure(ctx))
	}

	// Fail #1: retry on 2026-02-03 (grace until end-of-day).
	failure("2026-02-03")

	// Fail #2 (during grace): retry on 2026-02-05 (append).
	clock.Advance(24 * time.Hour)
	failure("2026-02-05")

	// Fail #3 (during grace): retry on 2026-02-07 (append; future window exists).
	clock.Advance(12 * time.Hour)
	failure("2026-02-07")

	// Sanity: paid window is unchanged.
	var gotPaid models.Entitlement
	require.NoError(t, bunDB.NewSelect().Model(&gotPaid).Where("id = ?", paidEnt.ID).Scan(ctx))
	require.NotNil(t, gotPaid.EndAt)
	require.Equal(t, paidEnd.UTC(), gotPaid.EndAt.UTC())
	require.Nil(t, gotPaid.RevokedAt)

	// During grace, user should still be entitled.
	ok, err = entSvc.IsEntitled(ctx, userID, "premium", clock.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)

	// Renewal success on 2026-02-04; next renewal date is 2026-03-05.
	successAt := time.Date(2026, 2, 4, 12, 0, 0, 0, time.UTC)
	clock.Advance(successAt.Sub(clock.Now().UTC()))

	successBody, err := json.Marshal(CCBillRenewalSuccessEvent{
		TransactionID:      "txn_" + uuid.New().String(),
		SubscriptionID:     ccbillSubID,
		ClientAccnum:       "1234",
		ClientSubacc:       "0000",
		Timestamp:          clock.Now().UTC().Format("2006-01-02 15:04:05"),
		BilledAmount:       "9.99",
		BilledCurrencyCode: "usd",
		NextRenewalDate:    "2026-03-05",
	})
	require.NoError(t, err)

	webhook := &CCBillWebhookService{
		Data: CCBillWebhookEvent{
			EventBody: successBody,
		},
		DB:                           dbi,
		Clock:                        clock,
		SubscriptionService:          subSvc,
		SubscriptionLifecycleService: lifecycle,
	}
	require.NoError(t, webhook.handleRenewalSuccess(ctx))

	// Grace windows should be cleared: any active grace revoked; any future grace deleted.
	var graceRows []models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&graceRows).
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
		require.NotNil(t, gr.RevokedAt, "active grace windows should be revoked")
	}

	// Renewal should append a new paid window that starts now and ends at the processor-provided paid term end.
	expectedPaidEnd := time.Date(2026, 3, 5, 23, 59, 59, 0, time.UTC)
	var paidWindows []models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&paidWindows).
		Where("user_id = ? AND entitlement = ?", userID, "premium").
		Where("source_type = ?", models.EntitlementSourceSubscription).
		Where("source_id = ?", subID).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		OrderExpr("start_at ASC").
		Scan(ctx))
	require.GreaterOrEqual(t, len(paidWindows), 2) // original + new

	latest := paidWindows[len(paidWindows)-1]
	require.NotNil(t, latest.EndAt)
	require.Equal(t, expectedPaidEnd.UTC(), latest.EndAt.UTC())
	require.True(t, latest.StartAt.UTC().Equal(successAt.UTC()) || latest.StartAt.UTC().After(successAt.UTC()))

	ok, err = entSvc.IsEntitled(ctx, userID, "premium", successAt.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)
}
