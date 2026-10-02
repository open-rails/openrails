//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// #1102: when OpenRails ends a provider-billed membership, the provider cancel
// is a durable intent queued in the same transaction, on every rail.

// providerCancels counts the provider-cancel intents queued for a subscription.
func (w *world) providerCancels(intentType string, sub uuid.UUID) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT count(*) FROM openrails.rail_intents
		WHERE intent_type = $1 AND subscription_id = $2`), intentType, sub).Scan(&n))
	return n
}

// A refund, void or chargeback CCBill reports ends access at once, and CCBill
// is told to stop rebilling: exactly one cancel, however often CCBill
// redelivers the postback.
func TestCCBillReversalStopsCCBillBilling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, event string
		fields      func(*ccbillMember) map[string]string
	}{
		{"refund", "Refund", func(m *ccbillMember) map[string]string { return m.reversal(ccbillNumericID(), "9.99") }},
		{"void", "Void", func(m *ccbillMember) map[string]string { return m.reversal(m.saleTxn, "9.99") }},
		{"chargeback", "Chargeback", func(m *ccbillMember) map[string]string { return m.reversal(ccbillNumericID(), "9.99") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dl := newDataLinkFake(t)
			w := newDataLinkWorld(t, dl)
			m := importCCBill(t, w)
			w.armDestructive()
			fields := tc.fields(m)
			w.deliverCCBill(tc.event, fields)
			require.Equal(t, "cancelled", w.subscription(embedded, m.sub).Status)
			require.False(t, m.c.entitled(m.ent), "a reversed payment ends access")

			w.until(func() bool { return dl.cancelled(m.railSub) > 0 }, "the queued cancel reaches CCBill")
			w.deliverCCBill(tc.event, fields)
			w.wake()
			require.Equal(t, 1, dl.cancelled(m.railSub), "one cancel for a redelivered postback")
			require.Equal(t, 1, w.providerCancels("ccbill_cancel_subscription", m.sub.UUID()))
			require.Zero(t, w.engineCharges())
		})
	}
}

// CCBill's own cancellation of a failed rebill needs no cancel from OpenRails.
func TestCCBillFailedRebillCancelQueuesNothing(t *testing.T) {
	t.Parallel()
	dl := newDataLinkFake(t)
	w := newDataLinkWorld(t, dl)
	m := importCCBill(t, w)
	w.armDestructive()
	w.deliverCCBill("Cancellation", map[string]string{"subscriptionId": m.railSub, "reason": "Transaction Declined", "source": "failedRB"})
	require.Equal(t, "cancelled", w.subscription(embedded, m.sub).Status)
	w.wake()
	require.Zero(t, w.providerCancels("ccbill_cancel_subscription", m.sub.UUID()))
	require.Zero(t, dl.cancelled(m.railSub))
}

// A merchant cancel of a Stripe membership never calls Stripe inline: the
// cancel commits with its intent, and the intent reaches Stripe once Stripe
// is back.
func TestStripeMerchantCancelIsDurable(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.stripe.subscriptionWritesDown(true)
	cancel := map[string]any{"reason": "merchant ended the membership"}
	status, body := w.staffJSON(http.MethodPost, "/v1/merchant/subscriptions/"+l.sub.String()+"/cancel", cancel)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status, "the cancel never waits on Stripe")
	require.True(t, l.c.entitled(l.ent), "the paid period is kept")
	require.Equal(t, false, w.stripe.subscriptionObject(l.railSub)["cancel_at_period_end"])

	w.stripe.subscriptionWritesDown(false)
	w.until(func() bool { return w.stripe.subscriptionObject(l.railSub)["cancel_at_period_end"] == true }, "the queued cancel reaches Stripe")
	require.Len(t, l.stripeSubWrites(), 1)

	status, _ = w.staffJSON(http.MethodPost, "/v1/merchant/subscriptions/"+l.sub.String()+"/cancel", cancel)
	require.NotEqual(t, http.StatusOK, status, "a cancelled membership is not cancelled again")
	w.wake()
	require.Len(t, l.stripeSubWrites(), 1)
	require.Equal(t, 1, w.providerCancels("stripe_cancel_subscription", l.sub.UUID()))
	require.Zero(t, l.engineCharges())
}
