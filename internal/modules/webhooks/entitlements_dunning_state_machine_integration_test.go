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

// #691 CCBill dunning state machine: the auto-renew sub's STANDING window keeps
// access intact through the whole failure/retry cycle — no grace windows are
// minted, grace_ends_at is only a pacing marker — and a renewal success extends
// the paid-through FACT (period end + a bounded per-period grant), never the
// window.
func TestEntitlements_CCBillDunning_StateMachine(t *testing.T) {

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	q := gen.New(pool)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(t0)

	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
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
		MerchantID:       dbtest.TestMerchantID.UUID(),
		Key:              "test_product_" + uuid.New().String(),
		DisplayName:      "Test Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Archived:         false,
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           productID,
		Amount:              9_990_000,
		Currency:            "USD",
		Archived:            false,
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           clock.Now().UTC(),
		UpdatedAt:           clock.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    subID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               &priceID,
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailCCBill),
		RailSubscriptionID:    ccbillSubID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &paidEnd,
		StartedAt:             clock.Now().UTC(),
		CreatedAt:             clock.Now().UTC(),
		UpdatedAt:             clock.Now().UTC(),
	})
	require.NoError(t, err)

	// #691 shape: the access window is STANDING (end_at NULL) from activation.
	paidEntID := uuid.New()
	_, err = q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:          paidEntID,
		MerchantID:  dbtest.TestMerchantID.UUID(),
		CustomerID:  tenantSubjectID,
		Entitlement: "premium",
		StartAt:     periodStart,
		EndAt:       nil,
		SourceType:  string(models.EntitlementSourceSubscription),
		SourceID:    &subID,
		CreatedAt:   clock.Now().UTC(),
		UpdatedAt:   clock.Now().UTC(),
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	entSvc := entitlements.NewEntitlementService(dbi)
	entSvc.SetClock(clock)
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entSvc, notifSvc, paymentSvc)
	lifecycle.SetClock(clock)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	entitled := func(at time.Time) bool {
		ok, err := entSvc.IsEntitled(ctx, userID, "premium", at)
		require.NoError(t, err)
		return ok
	}
	graceRowCount := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE customer_id = $1 AND source_type = $2 AND source_id = $3`,
			tenantSubjectID, string(models.EntitlementSourceGrace), subID).Scan(&n))
		return n
	}

	// (1) Fail-open: access holds before AND past the paid period end.
	require.True(t, entitled(paidEnd.Add(-time.Second)))
	require.True(t, entitled(paidEnd.Add(30*24*time.Hour)), "standing window: silence past period end never gates access")

	// Move time to paid end and process dunning failures (CCBill retries itself).
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

	// Three failures across CCBill's retry schedule: NO grace windows are
	// minted; the sub is past_due with the pacing marker following the retry
	// date (capped by ccbillGraceCap); access never moves.
	failure("2026-02-01")
	clock.Advance(12 * time.Hour)
	failure("2026-02-02")
	clock.Advance(6 * time.Hour)
	failure("2026-02-03")

	require.Zero(t, graceRowCount(), "#691: dunning failures mint no grace windows")
	var status string
	var graceEndsAt *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT status, grace_ends_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&status, &graceEndsAt))
	require.Equal(t, "past_due", status)
	require.NotNil(t, graceEndsAt, "grace_ends_at survives as the pacing marker")
	require.True(t, entitled(clock.Now().UTC()), "access intact mid-dunning")

	// The standing window is untouched.
	gotPaid, err := q.GetEntitlementByID(ctx, paidEntID)
	require.NoError(t, err)
	require.Nil(t, gotPaid.EndAt)
	require.Nil(t, gotPaid.RevokedAt)

	// Renewal success on 2026-02-02 06:00; next renewal date is 2026-03-05.
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

	// The renewal extends the paid-through FACT, not the window: period end
	// advanced, dunning cleared, still exactly ONE live standing window, and a
	// bounded per-period grant recorded for the new period.
	expectedPaidEnd := time.Date(2026, 3, 5, 23, 59, 59, 0, time.UTC)
	var newStatus string
	var periodEnd time.Time
	var clearedGrace *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT status, current_period_ends_at, grace_ends_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&newStatus, &periodEnd, &clearedGrace))
	require.Equal(t, "active", newStatus)
	require.Equal(t, expectedPaidEnd.UTC(), periodEnd.UTC(), "paid-through fact = the rail-provided term end")
	require.Nil(t, clearedGrace, "renewal clears the pacing marker")

	rows, err := pool.Query(ctx,
		`SELECT end_at, revoked_at, deleted_at FROM openrails.entitlements WHERE customer_id = $1 AND source_type = $2 AND source_id = $3`,
		tenantSubjectID, string(models.EntitlementSourceSubscription), subID)
	require.NoError(t, err)
	liveWindows := 0
	for rows.Next() {
		var endAt, revokedAt, deletedAt *time.Time
		require.NoError(t, rows.Scan(&endAt, &revokedAt, &deletedAt))
		if revokedAt == nil && deletedAt == nil {
			liveWindows++
			require.Nil(t, endAt, "the one live window stays standing")
		}
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 1, liveWindows, "renewal must not append windows")

	var periodGrants int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.grants WHERE source_type='subscription' AND source_id=$1 AND event='grant' AND ends_at IS NOT NULL`,
		subID.String()).Scan(&periodGrants))
	require.Equal(t, 1, periodGrants, "the renewal records its bounded per-period grant")

	require.Zero(t, graceRowCount(), "no grace windows at any point")
	require.True(t, entitled(successAt.Add(time.Second)))
	require.True(t, entitled(expectedPaidEnd.Add(24*time.Hour)), "access remains fail-open past the new paid-through")
}
