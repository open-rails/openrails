//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestEntitlements_CCBillDunning_StateMachine(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(t0)

	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureTenantSubjectIDPgx(ctx, t, pool, userID)
	subID := uuid.New()
	ccbillSubID := "ccbill_sub_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()

	billingDays := int32(30)
	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour) // 2026-01-31 00:00Z

	entitlementsSpecJSON, err := json.Marshal(map[string]*int{"premium": nil})
	require.NoError(t, err)
	description := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:               productID,
		Slug:             "test_product_" + uuid.New().String(),
		DisplayName:      "Test Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Status:           string(models.CatalogStatusActive),
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               priceID,
		ProductID:        productID,
		Amount:           999,
		Currency:         "usd",
		Status:           string(models.CatalogStatusActive),
		BillingCycleDays: &billingDays,
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                      subID,
		TenantSubjectID:         tenantSubjectID,
		ProductID:               productID,
		PriceID:                 &priceID,
		Status:                  string(models.StatusActive),
		Processor:               string(models.ProcessorCCBill),
		ProcessorSubscriptionID: ccbillSubID,
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               clock.Now().UTC(),
		CreatedAt:               clock.Now().UTC(),
		UpdatedAt:               clock.Now().UTC(),
	})
	require.NoError(t, err)

	paidEntID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:              paidEntID,
		TenantSubjectID: tenantSubjectID,
		Entitlement:     "premium",
		StartAt:         periodStart,
		EndAt:           &paidEnd,
		SourceType:      string(models.EntitlementSourceSubscription),
		SourceID:        &subID,
		CreatedAt:       clock.Now().UTC(),
		UpdatedAt:       clock.Now().UTC(),
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE tenant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
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

	// Grace windows are capped at paidTermEnd + ccbillGraceCap (72h) =
	// 2026-02-03 00:00Z (see capCCBillRetryAt). Retry dates must stay distinct
	// *within* that cap so each failure appends a new window rather than being
	// collapsed to the same capped instant and deduplicated.
	//
	// Fail #1: retry on 2026-02-01 → grace until 2026-02-01 23:59:59.
	failure("2026-02-01")

	// Fail #2 (during grace): retry on 2026-02-02 (append) → until 2026-02-02 23:59:59.
	clock.Advance(12 * time.Hour)
	failure("2026-02-02")

	// Fail #3 (during grace): retry on 2026-02-03 (append; capped to 2026-02-03 00:00Z).
	clock.Advance(6 * time.Hour)
	failure("2026-02-03")

	// Sanity: paid window is unchanged.
	gotPaid, err := q.GetEntitlementByID(ctx, paidEntID)
	require.NoError(t, err)
	require.NotNil(t, gotPaid.EndAt)
	require.Equal(t, paidEnd.UTC(), gotPaid.EndAt.UTC())
	require.Nil(t, gotPaid.RevokedAt)

	// During grace, user should still be entitled.
	ok, err = entSvc.IsEntitled(ctx, userID, "premium", clock.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)

	// Renewal success on 2026-02-02 06:00 (inside grace window #2, before window
	// #3 starts, so #1/#2 are revoked-as-active and #3 is deleted-as-future);
	// next renewal date is 2026-03-05.
	successAt := time.Date(2026, 2, 2, 6, 0, 0, 0, time.UTC)
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

	// Grace windows should be cleared: any active grace revoked; any future grace
	// deleted. Raw SQL sees the soft-deleted future window (deleted_at set)
	// without any opt-in.
	graceQuery := `SELECT start_at, end_at, revoked_at, deleted_at FROM billing.entitlements
		WHERE tenant_subject_id = $1 AND entitlement = $2
		  AND source_type = $3
		  AND source_id = $4
		ORDER BY start_at ASC`
	rows, err := pool.Query(ctx, graceQuery, tenantSubjectID, "premium", string(models.EntitlementSourceGrace), subID)
	require.NoError(t, err)
	var graceRows []models.Entitlement
	for rows.Next() {
		var gr models.Entitlement
		require.NoError(t, rows.Scan(&gr.StartAt, &gr.EndAt, &gr.RevokedAt, &gr.DeletedAt))
		graceRows = append(graceRows, gr)
	}
	require.NoError(t, rows.Err())
	require.Len(t, graceRows, 3)

	for _, gr := range graceRows {
		switch {
		case gr.StartAt.After(successAt):
			// Future grace window (starts after renewal): deleted, not revoked.
			require.NotNil(t, gr.DeletedAt, "future grace windows should be deleted")
			require.Nil(t, gr.RevokedAt, "future grace windows should not be revoked")
		case gr.EndAt != nil && !gr.EndAt.After(successAt):
			// Past grace window (already expired before renewal): left as a
			// historical record — neither revoked nor deleted.
			require.Nil(t, gr.DeletedAt, "expired grace windows should not be deleted")
		default:
			// The grace window active at renewal time is revoked.
			require.NotNil(t, gr.RevokedAt, "the active grace window should be revoked")
		}
	}

	// Renewal should append a new paid window that starts now and ends at the processor-provided paid term end.
	expectedPaidEnd := time.Date(2026, 3, 5, 23, 59, 59, 0, time.UTC)
	paidQuery := `SELECT start_at, end_at FROM billing.entitlements
		WHERE tenant_subject_id = $1 AND entitlement = $2
		  AND source_type = $3
		  AND source_id = $4
		  AND revoked_at IS NULL
		  AND deleted_at IS NULL
		ORDER BY start_at ASC`
	rows, err = pool.Query(ctx, paidQuery, tenantSubjectID, "premium", string(models.EntitlementSourceSubscription), subID)
	require.NoError(t, err)
	var paidWindows []models.Entitlement
	for rows.Next() {
		var pw models.Entitlement
		require.NoError(t, rows.Scan(&pw.StartAt, &pw.EndAt))
		paidWindows = append(paidWindows, pw)
	}
	require.NoError(t, rows.Err())
	require.GreaterOrEqual(t, len(paidWindows), 2) // original + new

	latest := paidWindows[len(paidWindows)-1]
	require.NotNil(t, latest.EndAt)
	require.Equal(t, expectedPaidEnd.UTC(), latest.EndAt.UTC())
	require.True(t, latest.StartAt.UTC().Equal(successAt.UTC()) || latest.StartAt.UTC().After(successAt.UTC()))

	ok, err = entSvc.IsEntitled(ctx, userID, "premium", successAt.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)
}
