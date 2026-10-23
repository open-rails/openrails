//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/testevents"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/writeposture"
)

// renewalOrders are the subscription's renewal orders, as the merchant reads
// them, newest first.
func (e *engineCase) renewalOrders() []billing.Order {
	e.w.t.Helper()
	page, err := e.w.client[e.tp].ListOrders(e.w.t.Context(), billing.OrderListParams{SubscriptionID: e.sub, Origin: billing.OrderOriginSubscription})
	require.NoError(e.w.t, err)
	return page.Items
}

// openRenewal is the subscription's unpaid renewal order, as its customer
// finds it from the subscription.
func (e *engineCase) openRenewal() orderCall {
	e.w.t.Helper()
	recovery, _ := unwrap(e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil))["recovery"].(map[string]any)
	id, _ := recovery["order_id"].(string)
	require.NotEmpty(e.w.t, id, "the subscription names its unpaid renewal: %v", recovery)
	return e.c.order(http.MethodGet, "/orders/"+id, "", nil)
}

// payRenewal pays the subscription's open renewal order with a saved card.
func (e *engineCase) payRenewal(method string) orderCall {
	e.w.t.Helper()
	open := e.openRenewal()
	return e.c.order(http.MethodPost, "/orders/"+open.body["id"].(string)+"/pay", "pay-"+uuid.NewString(),
		map[string]any{"payment": map[string]any{"payment_method_id": method}, "expected_total": open.body["total"]})
}

// Each period of an engine-owned subscription is a renewal order: the due
// pass opens it, its charge is an attempt on it, and paid it is numbered and
// complete with the renewal's payment. One order per period, however often
// the pass runs.
func TestRenewalIsAnOrder(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		e := enroll(t, w, rail, tp)
		for period := 1; period <= 2; period++ {
			end := e.periodEnd()
			e.toFreshPeriodEnd()
			w.runRenewals()
			w.runRenewals()
			renewals := e.renewalOrders()
			require.Len(t, renewals, period, "one renewal order per period")
			o := renewals[0]
			require.Equal(t, billing.OrderOriginSubscription, o.Origin)
			require.Equal(t, billing.OrderComplete, o.Status)
			require.Equal(t, billing.OrderPaymentSucceeded, o.Payment.Status)
			require.NotNil(t, o.Number)
			require.Nil(t, o.ExpiresAt, "a renewal does not expire")
			require.NotNil(t, o.Renewal)
			require.Equal(t, e.sub, o.Renewal.SubscriptionID)
			require.True(t, end.Equal(o.Renewal.PeriodStartsAt) && end.Equal(o.Renewal.DueAt), "due when its period starts: %+v", o.Renewal)
			require.True(t, e.periodEnd().Equal(o.Renewal.PeriodEndsAt), "the order bills the period it paid")
			require.EqualValues(t, 9_990_000, o.Total)
			require.Len(t, o.Lines, 1)
			require.Equal(t, e.price, o.Lines[0].PriceID.String())
			require.Equal(t, e.sub, *o.Lines[0].SubscriptionID)
			paid := completed(w.payments(tp, e.c.id))
			require.Len(t, paid, period+1)
			require.NotNil(t, o.Payment.PaymentID)
			require.True(t, slices.ContainsFunc(paid, func(p billing.Payment) bool { return p.ID == *o.Payment.PaymentID }), "the order holds the renewal's payment")
		}
		mine := e.c.order(http.MethodGet, "/orders?subscription_id="+e.sub.String(), "", nil)
		require.Equal(t, http.StatusOK, mine.status, "%v", mine.body)
		require.Len(t, mine.body["data"], 2, "the customer reads their renewals")
		events, err := testevents.Hosts(t.Context(), w.client[tp], billing.HostEventOrderCompleted)
		require.NoError(t, err)
		renewals, renewed := e.renewalOrders(), 0
		for _, ev := range events.Items {
			if ev.Order != nil && slices.ContainsFunc(renewals, func(o billing.Order) bool { return o.ID == ev.Order.OrderID }) {
				renewed++
			}
		}
		require.Equal(t, 2, renewed, "order.completed for each renewal")
	})
}

// A declined renewal leaves its order open with why, overdue and named by
// the subscription. The customer cannot cancel it; they pay it with a saved
// card: a decline answers 402 and leaves it open, another card pays it and
// becomes the subscription's, which the next renewal charges off-session.
func TestRenewalOrderPaidByCustomer(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, rail, remote)
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			end := e.periodEnd()
			e.toFreshPeriodEnd()
			w.runRenewals()
			require.Equal(t, billing.SubscriptionPastDue, w.subscription(e.tp, e.sub).Status)

			open := e.openRenewal()
			id := open.body["id"].(string)
			require.Equal(t, "open", open.body["status"], "%v", open.body)
			require.Equal(t, "requires_payment_method", paymentOf(open)["status"])
			require.NotEmpty(t, paymentOf(open)["last_payment_error"].(map[string]any)["reason"])
			options := open.body["payment_options"].([]any)
			require.Len(t, options, 1)
			require.Equal(t, []any{"saved_card"}, options[0].(map[string]any)["accepts"], "a renewal is paid with a saved card")
			recovery := unwrap(e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil))["recovery"].(map[string]any)
			require.Equal(t, true, recovery["retryable"])

			overdue, err := w.client[e.tp].ListOrders(t.Context(), billing.OrderListParams{Overdue: true})
			require.NoError(t, err)
			require.True(t, slices.ContainsFunc(overdue.Items, func(o billing.Order) bool { return o.ID.String() == id }), "an open renewal past its due date is overdue")

			canceled := e.c.order(http.MethodPost, "/orders/"+id+"/cancel", "", nil)
			require.Equal(t, http.StatusConflict, canceled.status, "%v", canceled.body)
			require.Equal(t, "order_not_cancelable", orderError(canceled))
			token := e.c.order(http.MethodPost, "/orders/"+id+"/pay", "pay-"+uuid.NewString(), map[string]any{"payment": map[string]any{"token": "tok_new"}, "expected_total": open.body["total"]})
			require.Equal(t, http.StatusBadRequest, token.status, "%v", token.body)

			declined := e.payRenewal(e.method)
			require.Equal(t, http.StatusPaymentRequired, declined.status, "%v", declined.body)
			require.Equal(t, "card_declined", orderError(declined))
			require.Equal(t, "open", e.openRenewal().body["status"])
			require.Equal(t, billing.SubscriptionPastDue, w.subscription(e.tp, e.sub).Status)

			fresh := e.c.saveCard(rail, mastercard)
			paid := e.payRenewal(fresh)
			require.Equal(t, http.StatusOK, paid.status, "%v", paid.body)
			require.Equal(t, "complete", paid.body["status"], "%v", paid.body)
			require.Equal(t, fresh, paymentOf(paid)["payment_method_id"])
			sub := w.subscription(e.tp, e.sub)
			require.Equal(t, billing.SubscriptionActive, sub.Status)
			require.True(t, end.Add(monthHours*time.Hour).Equal(*sub.CurrentPeriodEndsAt), "the customer pays the period that was due")
			require.Equal(t, fresh, sub.PaymentMethodID.String(), "the card that paid is the subscription's")
			require.Equal(t, mastercard.Last4, e.lastChargedCard())
			require.Len(t, e.providerLedger(), 2)
			again := e.c.order(http.MethodPost, "/orders/"+id+"/pay", "pay-"+uuid.NewString(), map[string]any{"payment": map[string]any{"payment_method_id": fresh}, "expected_total": open.body["total"]})
			require.Equal(t, http.StatusConflict, again.status, "%v", again.body)
			require.Equal(t, "order_not_payable", orderError(again))

			e.toFreshPeriodEnd()
			w.runRenewals()
			require.Equal(t, billing.SubscriptionActive, w.subscription(e.tp, e.sub).Status)
			require.Len(t, e.providerLedger(), 3, "the next renewal charges the new card off-session")
			require.Equal(t, mastercard.Last4, e.lastChargedCard())
			require.Len(t, e.renewalOrders(), 2)
		})
	}
}

// The member pays a declined renewal themselves: a customer retry of the
// same cycle.
func TestEngineRebillCustomerRetry(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	e.refreshBeforePeriodEnd()
	e.setDecline(visa.Last4, "", "202")
	e.toPeriodEnd()
	w.runRenewals()
	require.Equal(t, billing.SubscriptionPastDue, w.subscription(e.tp, e.sub).Status)
	e.setDecline(visa.Last4, "", "")
	paid := e.payRenewal(e.method)
	require.Equal(t, http.StatusOK, paid.status, "%v", paid.body)
	w.settle()
	require.Equal(t, billing.SubscriptionActive, w.subscription(e.tp, e.sub).Status)
	rows := w.cycleAttempts(e.sub)
	require.Len(t, rows, 2)
	require.Equal(t, []string{"rebill", "customer_retry"}, []string{rows[0].Kind, rows[1].Kind})
	require.Equal(t, "approved", rows[1].Category)
	require.Equal(t, rows[0].Cycle, rows[1].Cycle)
	require.Equal(t, []string{"customer", "used"}, credentialFields(w.nmi.LastSale())[:2], "the customer pays in person, citing the card's agreement")
}

// A renewal the issuer challenges is an order awaiting the member: they are
// told, authenticate on the order's next action and confirm it.
func TestRenewalOrderAuthentication(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.setDecline(visa.Last4, "auth", "")
	end := e.periodEnd()
	e.toFreshPeriodEnd()
	w.runRenewals()
	open := e.openRenewal()
	require.Equal(t, "open", open.body["status"], "%v", open.body)
	require.Equal(t, "requires_action", paymentOf(open)["status"])
	next := paymentOf(open)["next_action"].(map[string]any)
	require.Equal(t, "authenticate", next["type"])
	require.Equal(t, 1, e.c.notificationCount("payment_authentication_required"), "the member is told to authenticate")
	recovery := unwrap(e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil))["recovery"].(map[string]any)
	require.Equal(t, "authentication_required", recovery["blocked_reason"])
	busy := e.payRenewal(e.method)
	require.Equal(t, http.StatusConflict, busy.status, "%v", busy.body)
	require.Equal(t, "order_payment_in_progress", orderError(busy))

	setRecoveryPosture(t, w, writeposture.ReadOnly)
	held := unwrap(e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil))["recovery"].(map[string]any)
	require.Equal(t, recovery, held, "a write hold must not erase the authentication requirement")
	setRecoveryPosture(t, w, writeposture.Full)

	require.True(t, w.stripe.Authenticate(next["payload"].(map[string]any)["payment_intent_id"].(string)))
	confirmed := e.c.order(http.MethodPost, "/orders/"+open.body["id"].(string)+"/confirm", "", nil)
	require.Equal(t, http.StatusOK, confirmed.status, "%v", confirmed.body)
	require.Equal(t, "complete", confirmed.body["status"], "%v", confirmed.body)
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.True(t, end.Add(monthHours*time.Hour).Equal(*sub.CurrentPeriodEndsAt))
	require.Len(t, e.providerLedger(), 2)
	w.runRenewals()
	require.Equal(t, 1, e.c.notificationCount("payment_authentication_required"))
	require.Len(t, e.providerLedger(), 2)
}

// Ending a subscription voids its unpaid renewal: order.voided, and nothing
// left to pay.
func TestRenewalOrderVoidedWithItsSubscription(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.setDecline(visa.Last4, "insufficient_funds", "")
	e.toFreshPeriodEnd()
	w.runRenewals()
	open := e.openRenewal()
	id := open.body["id"].(string)
	_, err := w.client[embedded].CancelSubscription(t.Context(), e.sub, billing.CancelSubscriptionParams{Reason: "the member left", RevokeAccess: true})
	require.NoError(t, err)
	read := e.c.order(http.MethodGet, "/orders/"+id, "", nil)
	require.Equal(t, "void", read.body["status"], "%v", read.body)
	require.NotNil(t, read.body["voided_at"])
	require.Empty(t, read.body["payment_options"])
	paid := e.c.order(http.MethodPost, "/orders/"+id+"/pay", "pay-"+uuid.NewString(), map[string]any{"payment": map[string]any{"payment_method_id": e.method}, "expected_total": open.body["total"]})
	require.Equal(t, http.StatusConflict, paid.status, "%v", paid.body)
	require.Equal(t, "order_not_payable", orderError(paid))
	events, err := testevents.Hosts(t.Context(), w.client[embedded], billing.HostEventOrderVoided)
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(events.Items, func(ev billing.HostEvent) bool { return ev.Order != nil && ev.Order.OrderID.String() == id }), "order.voided")
	overdue, err := w.client[embedded].ListOrders(t.Context(), billing.OrderListParams{Overdue: true, SubscriptionID: e.sub})
	require.NoError(t, err)
	require.Empty(t, overdue.Items, "a void renewal is not owed")
}

// A membership bought on an order renews from that order: a declined first
// renewal retries, beside its renewal order's own line, and the next period
// renews from the renewal.
func TestRenewalOfAnOrderedMembership(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			price := w.membership("orders:renewing", 9_990_000)
			c := w.newCustomer()
			method := c.saveCard(rail, visa)
			bought := c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(price, 0)},
				"expected_total": micros(9_990_000), "payment": map[string]any{"payment_method_id": method}})
			require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
			sub, err := billing.ParseSubscriptionID(orderLines(bought)[0]["subscription_id"].(string))
			require.NoError(t, err)
			e := &engineCase{w: w, rail: rail, tp: embedded, c: c, price: price.ID.String(), sub: sub, method: method, ent: "orders:renewing"}
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			end := e.periodEnd()
			e.toFreshPeriodEnd()
			w.runRenewals()
			pastDue := w.subscription(embedded, sub)
			require.Equal(t, billing.SubscriptionPastDue, pastDue.Status)
			e.setDecline(visa.Last4, "", "")
			w.advanceHealthyTo(nextRetry(pastDue).Add(time.Second))
			w.runRenewals()
			renewed := w.subscription(embedded, sub)
			require.Equal(t, billing.SubscriptionActive, renewed.Status, "the dunning retry renews from the order's agreement")
			require.True(t, end.Add(monthHours*time.Hour).Equal(*renewed.CurrentPeriodEndsAt))
			e.toFreshPeriodEnd()
			w.runRenewals()
			require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, sub).Status)
			require.Len(t, e.renewalOrders(), 2)
		})
	}
}
