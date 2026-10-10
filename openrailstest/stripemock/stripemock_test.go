package stripemock_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/shared/sigverify"
	"github.com/open-rails/openrails/openrailstest/stripemock"
)

// Paying a Checkout Session charges it and sends checkout.session.completed,
// signed and in the pinned API version; Redeliver resends it and a host's
// refusal is the caller's error.
func TestCheckoutSessionCompletesWithASignedWebhook(t *testing.T) {
	stripe := stripemock.New(stripemock.Options{})
	t.Cleanup(stripe.Close)
	var mu sync.Mutex
	var received [][]byte
	refuse := false
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if err := sigverify.VerifyStripe("whsec_test", r.Header.Get("Stripe-Signature"), body, 5*time.Minute); err != nil || refuse {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		require.Equal(t, "/billing/v1/webhooks/stripe/acct_test", r.URL.Path)
		received = append(received, body)
	}))
	t.Cleanup(host.Close)
	events := func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), received...)
	}
	stripe.SendWebhooksTo(host.URL+"/billing/v1/webhooks/stripe/acct_test", "whsec_test")

	form := url.Values{"mode": {"payment"}, "success_url": {"https://host.test/done"}, "cancel_url": {"https://host.test/done"},
		"line_items[0][price_data][currency]": {"usd"}, "line_items[0][price_data][unit_amount]": {"1299"},
		"line_items[0][price_data][product_data][name]": {"API credit"}, "line_items[0][quantity]": {"1"},
		"metadata[checkout_attempt_id]": {"chk_test"}, "metadata[user_id]": {"user-1"}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, stripe.URL()+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var opened struct{ ID, URL string }
	require.NoError(t, json.NewDecoder(res.Body).Decode(&opened))
	require.NoError(t, res.Body.Close())
	session := stripe.LastCheckoutSession()
	require.NotNil(t, session)
	require.Equal(t, opened.ID, session.ID)
	require.Equal(t, opened.URL, session.URL)
	require.Equal(t, "open", session.Status)
	require.EqualValues(t, 1299, session.AmountTotal)

	event, err := stripe.CompleteCheckoutSession(t.Context(), session.ID)
	require.NoError(t, err)
	require.Len(t, events(), 1)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(events()[0], &raw))
	require.Equal(t, event, raw["id"])
	require.Equal(t, "checkout.session.completed", raw["type"])
	require.Equal(t, stripeapi.APIVersion, raw["api_version"])
	object := raw["data"].(map[string]any)["object"].(map[string]any)
	require.Equal(t, "complete", object["status"])
	require.Equal(t, "paid", object["payment_status"])
	require.EqualValues(t, 1299, object["amount_total"])
	require.Equal(t, "chk_test", object["metadata"].(map[string]any)["checkout_attempt_id"])
	paid := stripe.LastCheckoutSession()
	require.Equal(t, object["payment_intent"], paid.PaymentIntent)
	ledger := stripe.Ledger("")
	require.Len(t, ledger, 1)
	require.Equal(t, paid.PaymentIntent, ledger[0].PaymentIntent)
	require.EqualValues(t, 1299, ledger[0].Amount, "Stripe charged the session")

	require.NoError(t, stripe.Redeliver(t.Context(), event))
	require.Len(t, events(), 2)
	require.Equal(t, events()[0], events()[1], "a redelivery is the same event")
	_, err = stripe.CompleteCheckoutSession(t.Context(), session.ID)
	require.Error(t, err, "a complete session is not paid twice")
	mu.Lock()
	refuse = true
	mu.Unlock()
	require.Error(t, stripe.Redeliver(t.Context(), event), "the host's refusal is the caller's")
	require.Empty(t, stripe.Unexpected())
}
