//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestOrderPaymentReceipts(t *testing.T) {
	t.Run("OR-J10-S05/exact-payment-key-cannot-claim-another-attempt", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		input := saleInput(t, a, provider)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		order, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: input.Lines, IdempotencyKey: "receipt-order"})
		require.NoError(t, err)
		path := "/v1/me/orders/" + order.ID.String() + "/payment-attempt"
		key := map[string]string{"Idempotency-Key": "accepted-payment"}
		status, raw := a.request(t, http.MethodGet, path, a.aliceToken, nil, key)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		declinedKey := map[string]string{"Idempotency-Key": "declined-payment"}
		declinedToken := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4002", Decline: "insufficient_funds"})
		declined := billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: declinedToken}, ExpectedTotal: 30_000_000, Reusable: ptr(true)}
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, declined, declinedKey)
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))
		status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil, declinedKey)
		require.Equal(t, http.StatusOK, status, string(raw))
		var declinedReceipt billing.OrderPaymentAttempt
		require.NoError(t, json.Unmarshal(raw, &declinedReceipt))
		require.Equal(t, "failed", declinedReceipt.State)
		require.Nil(t, declinedReceipt.PaymentID)
		require.Nil(t, declinedReceipt.PaymentMethodID, "an intended method UUID is not evidence of a retained card after decline")
		token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		pay := billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: token}, ExpectedTotal: 30_000_000, Reusable: ptr(true)}
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, pay, key)
		require.Equal(t, http.StatusOK, status, string(raw))
		var completed billing.Order
		require.NoError(t, json.Unmarshal(raw, &completed))
		require.Equal(t, billing.OrderComplete, completed.Status)
		require.NotNil(t, completed.Payment.PaymentID)
		status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil, key)
		require.Equal(t, http.StatusOK, status, string(raw))
		var receipt struct {
			OrderID         billing.OrderID          `json:"order_id"`
			CustomerID      billing.CustomerID       `json:"customer_id"`
			AttemptID       billing.OrderAttemptID   `json:"attempt_id"`
			State           string                   `json:"state"`
			PaymentID       *billing.PaymentID       `json:"payment_id"`
			PaymentMethodID *billing.PaymentMethodID `json:"payment_method_id"`
			PSPID           billing.PSPID            `json:"psp_id"`
			Amount          int64                    `json:"amount,string"`
			Currency        string                   `json:"currency"`
		}
		require.NoError(t, json.Unmarshal(raw, &receipt))
		require.Equal(t, order.ID, receipt.OrderID)
		require.Equal(t, customer, receipt.CustomerID)
		require.False(t, receipt.AttemptID.IsZero())
		require.Equal(t, "succeeded", receipt.State)
		require.Equal(t, completed.Payment.PaymentID, receipt.PaymentID)
		require.NotNil(t, receipt.PaymentMethodID)
		require.False(t, receipt.PSPID.IsZero())
		require.EqualValues(t, 30_000_000, receipt.Amount)
		require.Equal(t, "USD", receipt.Currency)
		for _, private := range []string{token, "sk_test_", "request_fingerprint", "rail_state", "payload"} {
			require.NotContains(t, string(raw), private)
		}
		original := append([]byte(nil), raw...)
		embedded, err := a.alpha.GetOrderPaymentAttempt(t.Context(), order.ID, "accepted-payment")
		require.NoError(t, err)
		remote, err := a.alphaRemote.GetOrderPaymentAttempt(t.Context(), order.ID, "accepted-payment")
		require.NoError(t, err)
		require.Equal(t, embedded, remote, "embedded and remote merchant clients return the same receipt")
		require.Equal(t, receipt.AttemptID, remote.AttemptID)
		_, err = a.betaRemote.GetOrderPaymentAttempt(t.Context(), order.ID, "accepted-payment")
		require.Error(t, err, "another merchant cannot read this receipt")
		status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil, declinedKey)
		require.Equal(t, http.StatusOK, status, string(raw))
		var retainedDecline billing.OrderPaymentAttempt
		require.NoError(t, json.Unmarshal(raw, &retainedDecline))
		require.Equal(t, declinedReceipt, retainedDecline, "later payment must not replace the original decline receipt")
		for range 2 {
			status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil, key)
			require.Equal(t, http.StatusOK, status, string(raw))
			require.JSONEq(t, string(original), string(raw))
		}
		status, raw = a.request(t, http.MethodGet, path, a.bobToken, nil, key)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil, map[string]string{"Idempotency-Key": "another-payment"})
		require.Equal(t, http.StatusNotFound, status, string(raw), "another successful route must not manufacture this attempt's receipt")
		status, raw = a.request(t, http.MethodGet, path, "", nil, key)
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
		status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil)
		require.Equal(t, http.StatusBadRequest, status, string(raw), "the original payment key is required")
		routes, err := a.engine.CustomerRoutes(openrails.CustomerRoutes{Prefix: "/receipt-map", Auth: a.auth.Authenticator(), Resolve: func(r *http.Request, actor auth.Verified) (openrails.CustomerBinding, error) {
			who, err := a.auth.ResolveTenantActor(r.Context(), actor)
			if err != nil {
				return openrails.CustomerBinding{}, err
			}
			if who.AccountID != a.alice.ID || who.TenantID != a.tenant {
				return openrails.CustomerBinding{}, billing.ErrNotFound
			}
			return openrails.CustomerBinding{MerchantID: a.alpha.MerchantID(), CustomerID: customer}, nil
		}})
		require.NoError(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			mux.Handle(route.Method+" "+route.Path, route.Handler)
		}
		mapped := httptest.NewServer(mux)
		defer mapped.Close()
		a.server = mapped
		mappedPath := "/receipt-map/orders/" + order.ID.String() + "/payment-attempt"
		status, raw = a.request(t, http.MethodGet, mappedPath, a.aliceToken, nil, key)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.JSONEq(t, string(original), string(raw))
		status, raw = a.request(t, http.MethodGet, mappedPath, a.bobToken, nil, key)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		effects := provider.effects(t)
		require.Len(t, effects, 1, "receipt reads cannot trigger or repeat provider writes")
		require.EqualValues(t, 3000, effects[0].Amount)
	})
}
