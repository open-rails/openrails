//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
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
		effects := provider.effects(t)
		require.Len(t, effects, 1, "receipt reads cannot trigger or repeat provider writes")
		require.EqualValues(t, 3000, effects[0].Amount)
	})
}
