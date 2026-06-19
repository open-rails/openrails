package subscriptions

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateSubscriptionPrice_ModelBUpgradeRequest pins the request the #268
// Stripe upgrade path constructs: switch the item to the new price, reset the
// billing cycle anchor to "now", and invoice the proration immediately so the
// customer is charged now for a fresh period (Model B).
func TestUpdateSubscriptionPrice_ModelBUpgradeRequest(t *testing.T) {
	var gotPath, gotMethod string
	var form url.Values
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = r.ParseForm()
		form = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sub_123"}`))
	})

	svc := &StripeService{Config: testStripeConfig(), Processors: testStripeProcessors()}
	svc.SetBaseURLForTest(srv.URL)

	// These are exactly the args the checkout upgrade branch passes.
	err := svc.UpdateSubscriptionPrice(
		context.Background(),
		"sub_123",
		"si_item_1",
		"price_new",
		"019e5e09-37e6-7ef7-be77-13a9891a13e0",
		"always_invoice",
		"now",
	)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/subscriptions/sub_123", gotPath)
	assert.Equal(t, "si_item_1", form.Get("items[0][id]"))
	assert.Equal(t, "price_new", form.Get("items[0][price]"))
	// Rewrite the subscription metadata to the new local price so the proration
	// invoice and all future renewals resolve the new tier in invoice.paid (#268).
	assert.Equal(t, "019e5e09-37e6-7ef7-be77-13a9891a13e0", form.Get("metadata[internal_price_id]"))
	// Model B: invoice the proration now + reset the cycle anchor to now.
	assert.Equal(t, "always_invoice", form.Get("proration_behavior"))
	assert.Equal(t, "now", form.Get("billing_cycle_anchor"))
}
