//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// An order of one-time lines paid on Stripe's hosted page: the customer goes
// to Checkout for the order's total and comes back to the host. The session's
// completion completes the order once, however often Stripe delivers it. An
// expired page lets the order take another payment; canceling an order
// expires its page.
func TestOrderPaidOnStripeCheckout(t *testing.T) {
	t.Parallel()
	w, _, _ := receiptWorld(t, true)
	ctx := t.Context()
	life := w.lifetime("orders:hosted", 15_000_000)
	c := w.newCustomer()
	hosted := func(key string) orderCall {
		return c.order(http.MethodPost, "/orders", key, map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(15_000_000),
			"payment": map[string]any{"return_url": "https://e2e.test/return", "psp_id": w.psp["stripe"]}})
	}

	preview := c.order(http.MethodPost, "/orders/preview", "", map[string]any{"lines": []any{line(life, 0)}})
	var accepts []any
	for _, o := range preview.body["payment_options"].([]any) {
		if o.(map[string]any)["psp_id"] == w.psp["stripe"].String() {
			accepts = o.(map[string]any)["accepts"].([]any)
		}
	}
	require.True(t, slices.Contains(accepts, any("redirect")), "Stripe takes one-time lines on its hosted page: %v", preview.body)
	refused := c.order(http.MethodPost, "/orders", "evil-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(15_000_000),
		"payment": map[string]any{"return_url": "https://evil.test/return", "psp_id": w.psp["stripe"]}})
	require.Equal(t, "invalid_param", orderError(refused), "%v", refused.body)

	created := hosted("hosted-" + uuid.NewString())
	require.Equal(t, http.StatusCreated, created.status, "%v", created.body)
	require.Equal(t, "requires_action", paymentOf(created)["status"])
	sessions := w.stripe.CheckoutSessions()
	session := sessions[len(sessions)-1]
	action := paymentOf(created)["next_action"].(map[string]any)
	require.Equal(t, "redirect_to_url", action["type"])
	require.Equal(t, session["url"], action["url"], "the customer goes to Stripe's page")
	require.Equal(t, "payment", session["mode"])
	require.EqualValues(t, 1500, session["amount_total"], "the order's total, in cents")
	require.Equal(t, "https://e2e.test/return", session["success_url"])
	require.Equal(t, created.body["id"], session["metadata"].(map[string]any)["order_id"])

	event, err := w.stripe.CompleteCheckoutSession(ctx, session["id"].(string))
	require.NoError(t, err)
	require.NoError(t, w.stripe.Redeliver(ctx, event), "a redelivered completion is accepted")
	w.settle()
	read := c.order(http.MethodGet, "/orders/"+created.body["id"].(string), "", nil)
	require.Equal(t, "complete", read.body["status"], "%v", read.body)
	require.Equal(t, "succeeded", paymentOf(read)["status"])
	require.True(t, c.entitled("orders:hosted"))
	require.Len(t, completed(w.payments(embedded, c.id)), 1, "one payment, however often Stripe delivers")

	// An expired page: the order takes another payment.
	other := w.lifetime("orders:hosted-expired", 5_000_000)
	again := c.order(http.MethodPost, "/orders", "hosted-"+uuid.NewString(), map[string]any{"lines": []any{line(other, 0)}, "expected_total": micros(5_000_000),
		"payment": map[string]any{"return_url": "https://e2e.test/return", "psp_id": w.psp["stripe"]}})
	require.Equal(t, http.StatusCreated, again.status, "%v", again.body)
	sessions = w.stripe.CheckoutSessions()
	expired := sessions[len(sessions)-1]
	expired["status"] = "expired"
	_, err = w.stripe.SendEvent(ctx, "checkout.session.expired", expired)
	require.NoError(t, err)
	read = c.order(http.MethodGet, "/orders/"+again.body["id"].(string), "", nil)
	require.Equal(t, "open", read.body["status"])
	require.Equal(t, "requires_payment_method", paymentOf(read)["status"])
	require.Equal(t, "payment_page_expired", paymentOf(read)["last_payment_error"].(map[string]any)["reason"])

	// Canceling an order awaiting its page expires the page.
	third := w.lifetime("orders:hosted-canceled", 5_000_000)
	pending := c.order(http.MethodPost, "/orders", "hosted-"+uuid.NewString(), map[string]any{"lines": []any{line(third, 0)}, "expected_total": micros(5_000_000),
		"payment": map[string]any{"return_url": "https://e2e.test/return", "psp_id": w.psp["stripe"]}})
	require.Equal(t, http.StatusCreated, pending.status, "%v", pending.body)
	canceled := c.order(http.MethodPost, "/orders/"+pending.body["id"].(string)+"/cancel", "", nil)
	require.Equal(t, http.StatusOK, canceled.status, "%v", canceled.body)
	sessions = w.stripe.CheckoutSessions()
	require.Equal(t, "expired", sessions[len(sessions)-1]["status"], "the canceled order's page is expired at Stripe")
}
