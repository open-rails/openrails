//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #1094: every CCBill post applies one lifecycle event to the locked row.
// Only a payment extends or restores a paid period.

// sendCCBill posts one CCBill event without asserting, for use off the test
// goroutine.
func (w *world) sendCCBill(ctx context.Context, eventType string, fields map[string]string) int {
	form := url.Values{"clientAccnum": {"945280"}, "clientSubacc": {"0000"}, "timestamp": {ccbillTimestamp(w.clock.Now())}}
	for k, v := range fields {
		form.Set(k, v)
	}
	query := url.Values{"eventType": {eventType}, "eventGroupType": {"Subscription"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.server.URL+mountPrefix+"/v1/webhooks/ccbill/"+ccbillAcct+"?"+query.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", ccbillSourceIP)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// raceCCBill holds the subscription's row lock, delivers the events in order
// so each queues behind the lock, then releases it: the events apply one at a
// time in delivery order, each on the row the previous one committed.
func (w *world) raceCCBill(m *ccbillMember, events ...ccbillEvent) {
	t, ctx := w.t, w.t.Context()
	tx, err := w.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	_, err = tx.Exec(ctx, w.q(`SELECT 1 FROM openrails.subscriptions WHERE id = $1 FOR UPDATE`), m.sub.UUID())
	require.NoError(t, err)

	done := make(chan int, len(events))
	for i, e := range events {
		go func() { done <- w.sendCCBill(ctx, e.kind, e.fields) }()
		require.Eventually(t, func() bool {
			var waiting int
			err := w.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database() AND query LIKE '%' || $1 || '%subscriptions%'`, w.schema).Scan(&waiting)
			return err == nil && waiting > i
		}, 20*time.Second, 10*time.Millisecond, "%s queues behind the row lock", e.kind)
	}
	require.NoError(t, tx.Rollback(ctx))
	for range events {
		require.Equal(t, http.StatusOK, <-done)
	}
	w.settle()
}

type ccbillEvent struct {
	kind   string
	fields map[string]string
}

func (m *ccbillMember) failure() ccbillEvent {
	return ccbillEvent{"RenewalFailure", map[string]string{
		"subscriptionId": m.railSub, "transactionId": ccbillNumericID(),
		"failureReason": "Insufficient funds", "failureCode": "BE-140",
		"renewalDate": ccbillDate(m.paidThrough), "nextRetryDate": ccbillDate(m.paidThrough.Add(2 * day)),
		"cardType": "VISA", "paymentType": "CREDIT",
	}}
}

func TestCCBillDatesAreNotPayment(t *testing.T) {
	w := newWorld(t)
	m := importCCBill(t, w)
	w.advance(21 * day)
	later := w.clock.Now().Add(30 * day)

	w.deliverCCBill("BillingDateChange", map[string]string{"subscriptionId": m.railSub, "nextRenewalDate": ccbillDate(later)})
	sub := w.subscription(embedded, m.sub)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(m.paidThrough), "a new rebill date is not a paid period: %v", sub.CurrentPeriodEndsAt)

	reactivation := map[string]string{"subscriptionId": m.railSub, "transactionId": ccbillNumericID(), "price": "9.99", "email": "member@example.test", "nextRenewalDate": ccbillDate(later)}
	w.deliverCCBill("UserReactivation", reactivation)
	require.True(t, w.subscription(embedded, m.sub).CurrentPeriodEndsAt.Equal(m.paidThrough), "a live reactivation pays for nothing")

	w.deliverCCBill("Cancellation", map[string]string{"subscriptionId": m.railSub, "reason": "Too expensive", "source": "webAdmin"})
	require.Equal(t, "cancelled", w.subscription(embedded, m.sub).Status)
	require.False(t, m.c.entitled(m.ent), "the paid period is over")

	reactivation["transactionId"] = ccbillNumericID()
	w.deliverCCBill("UserReactivation", reactivation)
	sub = w.subscription(embedded, m.sub)
	require.Equal(t, "cancelled", sub.Status, "no paid period is left to resume")
	require.True(t, sub.CurrentPeriodEndsAt.Equal(m.paidThrough))
	require.False(t, m.c.entitled(m.ent))
	require.Contains(t, w.openFindings("life.ccbill.reactivation_unapplied"), "subscription:"+m.sub.UUID().String())
	require.Zero(t, w.engineCharges())
}

func TestCCBillRenewalAfterRefundIsRefundReview(t *testing.T) {
	w := newWorld(t)
	m := importCCBill(t, w)
	w.deliverCCBill("Refund", m.reversal(ccbillNumericID(), "9.99"))
	require.Equal(t, "cancelled", w.subscription(embedded, m.sub).Status)

	w.advance(20 * day)
	txn := ccbillNumericID()
	w.deliverCCBill("RenewalSuccess", m.renewal(txn, m.paidThrough.Add(monthHours*time.Hour)))
	sub := w.subscription(embedded, m.sub)
	require.Equal(t, "cancelled", sub.Status, "a charge never reopens a refunded membership")
	require.False(t, m.c.entitled(m.ent))
	charge := m.payment(txn)
	require.NotNil(t, charge, "the charge is on the ledger, not dropped")
	require.Equal(t, "succeeded", charge.Status)
	var review string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT coalesce(metadata->>'refund_review', '') FROM openrails.payments WHERE transaction_id = $1`), txn).Scan(&review))
	require.NotEmpty(t, review, "the charge waits for refund review")

	before := len(w.payments(embedded, m.c.id))
	w.deliverCCBill("RenewalSuccess", m.renewal(txn, m.paidThrough.Add(monthHours*time.Hour)))
	require.Len(t, w.payments(embedded, m.c.id), before, "a redelivery records it once")
}

func TestCCBillDeclineCannotResurrectAChargeback(t *testing.T) {
	for _, order := range []string{"chargeback_first", "failure_first"} {
		t.Run(order, func(t *testing.T) {
			w := newWorld(t)
			m := importCCBill(t, w)
			w.advance(20 * day)
			chargeback := ccbillEvent{"Chargeback", m.reversal(ccbillNumericID(), "9.99")}
			if order == "chargeback_first" {
				w.raceCCBill(m, chargeback, m.failure())
			} else {
				w.raceCCBill(m, m.failure(), chargeback)
			}
			sub := w.subscription(embedded, m.sub)
			require.Equal(t, "cancelled", sub.Status, "the chargeback stands")
			require.NotNil(t, sub.CancelType)
			require.Equal(t, "chargeback", *sub.CancelType)
			require.False(t, m.c.entitled(m.ent))
		})
	}
}

func TestCCBillExpiryRacingRenewal(t *testing.T) {
	for _, order := range []string{"renewal_first", "expiry_first"} {
		t.Run(order, func(t *testing.T) {
			w := newWorld(t)
			m := importCCBill(t, w)
			w.advance(20*day + time.Hour)
			next := m.paidThrough.Add(monthHours * time.Hour)
			txn := ccbillNumericID()
			renewal := ccbillEvent{"RenewalSuccess", m.renewal(txn, next)}
			expiry := ccbillEvent{"Expiration", map[string]string{"subscriptionId": m.railSub}}
			if order == "renewal_first" {
				w.raceCCBill(m, renewal, expiry)
			} else {
				w.raceCCBill(m, expiry, renewal)
			}
			sub := w.subscription(embedded, m.sub)
			require.Equal(t, "active", sub.Status, "the paid renewal wins")
			require.True(t, sub.CurrentPeriodEndsAt.Equal(endOfDay(next)), "paid through %v", sub.CurrentPeriodEndsAt)
			require.Equal(t, "succeeded", m.payment(txn).Status)
			require.True(t, m.c.entitled(m.ent))
			require.Zero(t, w.engineCharges())
		})
	}
}
