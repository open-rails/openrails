//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #696: the provider-confirmed CCBill Cancellation webhook arriving AFTER our
// own local cancel (user cancel + ccbill_cancel_subscription intent) must be a
// NO-OP: no cancel provenance overwrite, no double revoke, no re-notification.
func TestCCBillCancellationWebhookAfterLocalCancelIsNoOp(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	custID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	subID, productID, priceID := uuid.New(), uuid.New(), uuid.New()
	ccbillSubID := "cc-wh-" + uuid.NewString()[:8]
	cancelledAt := now.Add(-2 * time.Hour)
	periodEnd := now.Add(20 * 24 * time.Hour)

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	tenantID := dbtest.TestMerchantID.UUID()
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "ccwh-prod-"+uuid.NewString()[:8], tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 9990000, 'usd', 720, true, $3)`, priceID, productID, tenantID)
	// The post-#696 user-cancel shape: cancelled with USER provenance, paid
	// runway preserved, access window already bounded at the period end.
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         cancelled_at, cancel_type, cancel_feedback, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'cancelled', 'ccbill', $4, $5, $6, $5, $7, 'user', 'too pricey', $8, $9)`,
		subID, priceID, productID, ccbillSubID,
		now.Add(-10*24*time.Hour), periodEnd, cancelledAt, custID, tenantID)
	entID := uuid.New()
	exec(`INSERT INTO openrails.entitlements (id, entitlement, start_at, end_at, source_id, source_type, customer_id, merchant_id)
	      VALUES ($1, 'premium', $2, $3, $4, 'subscription', $5, $6)`,
		entID, now.Add(-10*24*time.Hour), periodEnd, subID, custID, tenantID)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", custID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", custID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entSvc := entitlements.NewEntitlementService(dbi)
	svc := &CCBillWebhookService{
		Data: CCBillWebhookEvent{
			EventType: EventTypeCancellation,
			EventBody: mustJSON(t, CCBillCancellationEvent{
				CCBillCommonFields: CCBillCommonFields{
					ClientAccnum:   Stringish("900100"),
					ClientSubacc:   "0000",
					SubscriptionID: ccbillSubID,
					Timestamp:      now.Format("2006-01-02 15:04:05"),
				},
				Reason: "Merchant initiated cancellation",
				Source: "merchant",
			}),
		},
		DB:                           dbi,
		SubscriptionService:          subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil),
		SubscriptionLifecycleService: subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entSvc, subscriptions.NewNotificationService(dbi, nil), nil),
	}

	require.NoError(t, svc.handleCancel(ctx))

	// Cancel provenance untouched (still the USER cancel, original instant).
	var status, cancelType, feedback string
	var gotCancelledAt, gotPeriodEnd time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, cancel_type, cancel_feedback, cancelled_at, current_period_ends_at
		   FROM openrails.subscriptions WHERE id = $1`, subID).
		Scan(&status, &cancelType, &feedback, &gotCancelledAt, &gotPeriodEnd))
	require.Equal(t, "cancelled", status)
	require.Equal(t, "user", cancelType, "webhook must not overwrite the user cancel provenance")
	require.Equal(t, "too pricey", feedback)
	require.WithinDuration(t, cancelledAt, gotCancelledAt, time.Second, "original cancel instant preserved")
	require.WithinDuration(t, periodEnd, gotPeriodEnd, time.Second, "#691 paid runway untouched")

	// No double revoke: the runway-bounded window is intact.
	var endAt *time.Time
	var revokedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT end_at, revoked_at FROM openrails.entitlements WHERE id = $1`, entID).Scan(&endAt, &revokedAt))
	require.NotNil(t, endAt)
	require.WithinDuration(t, periodEnd, *endAt, time.Second)
	require.Nil(t, revokedAt, "no revoke on a no-op webhook")

	// No re-notification.
	var notifications int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.notification_queue WHERE customer_id = $1`, custID).Scan(&notifications))
	require.Zero(t, notifications, "no duplicate premium-ended notification")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
