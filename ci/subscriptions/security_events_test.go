//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// SEC-33: the provider's notice of a refund OpenRails issued can arrive before
// OpenRails finishes recording it. The refund is counted once, and the notice
// never applies a second decision: a partial refund with revoke_access=false
// keeps the membership and its access.
func TestSecurityRefundNoticeDuringOwnRefundCountsOnce(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		e := enroll(t, w, rail, tp)
		payment := completed(w.payments(tp, e.c.id))[0]
		g := w.refundGateServed(rail)
		done := make(chan error, 1)
		go func() {
			_, err := w.client[tp].RefundPayment(context.WithoutCancel(t.Context()), payment.ID, openrails.RefundPaymentParams{Amount: 4_000_000, Reason: "requested_by_customer", IdempotencyKey: "partial-" + payment.ID.String()})
			done <- err
		}()
		select {
		case <-g.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("the refund never reached the provider")
		}
		require.NotEqual(t, http.StatusOK, w.deliverNow(rail, w.refundNotice(rail)), "the early notice is deferred for redelivery")
		close(g.release)
		require.NoError(t, <-done)
		w.stripe.unhold()
		w.nmi.unhold()
		w.settle()
		require.Equal(t, http.StatusOK, w.deliver(rail, w.refundNotice(rail)), "the redelivered notice is the refund already recorded")

		var refunded int64
		for _, entry := range e.providerLedger() {
			refunded += entry.Refunded
		}
		require.EqualValues(t, 400, refunded)
		for _, view := range []topology{embedded, remote} {
			got, err := w.client[view].GetPayment(t.Context(), payment.ID)
			require.NoError(t, err)
			require.EqualValues(t, 4_000_000, got.AmountRefunded, "one refund recorded (%s)", view)
		}
		require.Equal(t, "active", w.subscription(tp, e.sub).Status)
		require.True(t, e.c.entitled(e.ent), "revoke_access=false keeps access")
	})
}

// SEC-33: an NMI chargeback of a one-time purchase revokes what it bought and
// records the reversal once, as a Stripe dispute of a one-off does.
func TestSecurityNMIChargebackRevokesOneTimePurchase(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.finitePass("content:pass")
	c := w.newCustomer()
	method := c.saveCard("nmi", card{Brand: "visa", Last4: "5100"})
	c.buyWith("nmi", method, price, openrails.OfferFinite, "content:pass")
	paid := completed(w.payments(embedded, c.id))
	require.Len(t, paid, 1)
	require.True(t, c.entitled("content:pass"))
	notice := func() obj {
		return nmiEvent("chargeback.batch.complete", obj{"batch": obj{"count": 1, "total_amount": "4.99"}, "count": 1, "chargebacks": []obj{{
			"id": "cb-" + paid[0].ID.String()[:8], "date": w.clock.Now().UTC().Format("2006-01-02"), "customer_name": "Greenfield Payer",
			"cc_number": "4xxxxxxxxxxx5100", "amount": "4.99", "reason_code": "10", "reason": "Fraud",
		}}})
	}
	for range 2 {
		require.Equal(t, http.StatusOK, w.deliver("nmi", notice()), "a redelivered batch changes nothing more")
		require.False(t, c.entitled("content:pass"), "the charged-back purchase grants nothing")
		for _, view := range []topology{embedded, remote} {
			got, err := w.client[view].GetPayment(t.Context(), paid[0].ID)
			require.NoError(t, err)
			require.Equal(t, got.Amount, got.AmountRefunded, "the chargeback is recorded once (%s)", view)
		}
	}
}

func stripeDisputeEvent(kind, id, status string, p openrails.Payment) obj {
	return stripeEvent(kind, obj{"object": "dispute", "id": id, "charge": p.TransactionID, "payment_intent": p.TransactionID,
		"amount": p.Amount / 10_000, "currency": "usd", "status": status, "reason": "fraudulent"})
}

// SEC-33: Stripe dispute notices arrive out of order. A dispute already won
// never reverses when its "created" notice arrives late, and winning a
// dispute restores only the cancellation that dispute caused.
func TestSecurityStripeDisputeOrdering(t *testing.T) {
	t.Parallel()
	t.Run("created_after_won/membership", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		p := completed(w.payments(embedded, l.c.id))[0]
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.closed", "dp_won1", "won", p)))
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_won1", "needs_response", p)))
		require.Equal(t, "active", w.subscription(embedded, l.sub).Status)
		require.True(t, l.c.entitled(l.ent))
		for _, view := range []topology{embedded, remote} {
			got, err := w.client[view].GetPayment(t.Context(), p.ID)
			require.NoError(t, err)
			require.Zero(t, got.AmountRefunded, "a won dispute records no chargeback (%s)", view)
		}
	})
	t.Run("created_after_won/one_off", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.finitePass("content:pass")
		c := w.newCustomer()
		method := c.saveCard("stripe", visa)
		c.buyWith("stripe", method, price, openrails.OfferFinite, "content:pass")
		p := completed(w.payments(embedded, c.id))[0]
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.closed", "dp_won2", "won", p)))
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_won2", "needs_response", p)))
		require.True(t, c.entitled("content:pass"))
	})
	t.Run("won_keeps_prior_cancellation", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		p := completed(w.payments(embedded, l.c.id))[0]
		require.Equal(t, http.StatusOK, w.deliver("stripe", l.providerCancelNotice()))
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_prior", "needs_response", p)))
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.closed", "dp_prior", "won", p)))
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status, "a won dispute never revives an earlier cancellation")
		require.Equal(t, "cancelled", w.subscription(remote, l.sub).Status)
	})
	t.Run("won_restores_its_own_cancellation", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		p := completed(w.payments(embedded, l.c.id))[0]
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_own", "needs_response", p)))
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.False(t, l.c.entitled(l.ent))
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.closed", "dp_own", "won", p)))
		require.Equal(t, "active", w.subscription(embedded, l.sub).Status, "winning the dispute restores the membership it cancelled")
	})
}

// SEC-33: CCBill signs nothing, so its posted dates are bounded. A renewal
// buys at most one billing cycle (plus CCBill's 72h grace) past the paid
// period, and a reactivation, which carries no payment, never extends it.
func TestSecurityCCBillPeriodEndsAreBounded(t *testing.T) {
	t.Parallel()
	t.Run("renewal", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		m := importCCBill(t, w)
		w.advance(20 * day)
		w.deliverCCBill("RenewalSuccess", m.renewal(ccbillNumericID(), m.paidThrough.Add(50*365*day)))
		limit := m.paidThrough.Add(monthHours*time.Hour + 72*time.Hour)
		for _, view := range []topology{embedded, remote} {
			sub := w.subscription(view, m.sub)
			require.NotNil(t, sub.CurrentPeriodEndsAt)
			require.False(t, sub.CurrentPeriodEndsAt.After(limit), "one renewal buys one cycle, not %v (%s)", sub.CurrentPeriodEndsAt, view)
		}
	})
	t.Run("billing_date_change", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		m := importCCBill(t, w)
		w.deliverCCBill("BillingDateChange", map[string]string{"subscriptionId": m.railSub, "nextRenewalDate": ccbillDate(m.paidThrough.Add(50 * 365 * day))})
		sub := w.subscription(embedded, m.sub)
		require.NotNil(t, sub.CurrentPeriodEndsAt)
		require.False(t, sub.CurrentPeriodEndsAt.After(m.paidThrough.Add(monthHours*time.Hour+72*time.Hour)))
	})
	t.Run("reactivation", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		m := importCCBill(t, w)
		w.deliverCCBill("UserReactivation", map[string]string{
			"subscriptionId": m.railSub, "transactionId": ccbillNumericID(), "price": "$9.99(USD) for 30 days then $9.99(USD) recurring every 30 days",
			"email": "payer@greenfield.test", "nextRenewalDate": ccbillDate(w.clock.Now().Add(365 * day)),
		})
		for _, view := range []topology{embedded, remote} {
			sub := w.subscription(view, m.sub)
			require.NotNil(t, sub.CurrentPeriodEndsAt)
			require.False(t, sub.CurrentPeriodEndsAt.After(m.paidThrough), "a reactivation keeps the paid period (%s)", view)
		}
	})
}
