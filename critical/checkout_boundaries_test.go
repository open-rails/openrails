//go:build integration

package critical_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestCheckoutBoundaries(t *testing.T) {
	t.Run("OR-J09-S02/customer-challenge-settles-only-after-confirmation", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "auth")
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "challenged-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var waiting billing.Order
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.Equal(t, billing.OrderPaymentRequiresAction, waiting.Payment.Status)
		require.NotNil(t, waiting.Payment.NextAction)
		require.Empty(t, p.effects(t))
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		calls := p.core.Submitted("/v1/payment_intents")
		require.Len(t, calls, 1)
		// Authenticate is the external bank's challenge completion; only the
		// following public confirmation can admit local access and settlement.
		intent, _, ok := strings.Cut(waiting.Payment.NextAction.Payload["client_secret"], "_secret_")
		require.True(t, ok, "the challenge must carry the provider's client capability")
		require.True(t, p.core.Authenticate(intent))
		var completed billing.Order
		for range 2 {
			status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+waiting.ID.String()+"/confirm", a.aliceToken, map[string]any{})
			require.Equal(t, http.StatusOK, status, string(raw))
			require.NoError(t, json.Unmarshal(raw, &completed))
			require.Equal(t, billing.OrderComplete, completed.Status)
			require.Equal(t, waiting.ID, completed.ID)
		}
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
		payment, err := a.alpha.GetPayment(t.Context(), *completed.Payment.PaymentID)
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		access, err = a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: payment.CustomerID, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["sale:owned"])
	})
	t.Run("OR-J09-S03/definitive-decline-keeps-order-unpaid", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "insufficient_funds")
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "declined-purchase"})
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.NotEqual(t, billing.OrderComplete, orders.Items[0].Status)
		require.NotEqual(t, billing.OrderPaymentProcessing, orders.Items[0].Payment.Status)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		for _, payment := range payments.Items {
			require.NotEqual(t, billing.PaymentSucceeded, payment.Status)
		}
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		require.Empty(t, p.effects(t))
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, facts.Events)
	})
	t.Run("OR-J10-S02/lost-client-response-replays-original-committed-result", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		origin := a.server
		wire := http.DefaultTransport.(*http.Transport).Clone()
		defer wire.CloseIdleConnections()
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forward, err := http.NewRequestWithContext(r.Context(), r.Method, origin.URL+r.URL.RequestURI(), r.Body)
			if err != nil {
				http.Error(w, "fixture forward", 500)
				return
			}
			forward.Header = r.Header.Clone()
			response, err := wire.RoundTrip(forward)
			if err != nil {
				http.Error(w, "fixture upstream", 502)
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
		}))
		defer proxy.Close()
		a.server = proxy
		_, _, err := a.do(t.Context(), http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "lost-client-answer"})
		a.server = origin
		require.Error(t, err, "the client must actually lose the committed answer")
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.Equal(t, billing.OrderComplete, orders.Items[0].Status)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "lost-client-answer"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var replay billing.Order
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, orders.Items[0].ID, replay.ID)
		require.Equal(t, orders.Items[0].Payment.PaymentID, replay.Payment.PaymentID)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
	})
}
