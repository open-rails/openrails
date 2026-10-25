//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		require.Equal(t, billing.OrderOpen, order.Status)
		require.Equal(t, billing.OrderPaymentRequiresPaymentMethod, order.Payment.Status, "an untouched cart has no admitted payment")
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
		afterDecline, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderOpen, afterDecline.Status)
		require.Equal(t, billing.OrderPaymentRequiresPaymentMethod, afterDecline.Payment.Status, "a definitive decline clears the admitted processing state")
		require.NotNil(t, afterDecline.Payment.LastPaymentError)
		require.Empty(t, provider.effects(t))
		token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		pay := billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: token}, ExpectedTotal: 30_000_000, Reusable: ptr(true)}
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		provider.mu.Lock()
		provider.responseGate = gate
		provider.mu.Unlock()
		defer gate.unblock()
		type answer struct {
			status int
			raw    []byte
			err    error
		}
		finished := make(chan answer, 1)
		go func() {
			status, raw, err := a.do(t.Context(), http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, pay, key)
			finished <- answer{status, raw, err}
		}()
		select {
		case <-gate.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("payment did not reach the held provider response")
		}
		// Independent public requests must see the committed admission while
		// the provider outcome is still unavailable to the paying handler.
		// A process exit here cannot leave this payment looking like a cart.
		pending, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderProcessing, pending.Status)
		require.Equal(t, billing.OrderPaymentProcessing, pending.Payment.Status)
		require.Nil(t, pending.Payment.PaymentID)
		status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil, key)
		require.Equal(t, http.StatusOK, status, string(raw))
		var pendingReceipt billing.OrderPaymentAttempt
		require.NoError(t, json.Unmarshal(raw, &pendingReceipt))
		require.Equal(t, "created", pendingReceipt.State)
		require.Nil(t, pendingReceipt.PaymentID)
		require.Len(t, provider.effects(t), 1, "the external journal already has the debit whose reply is held")
		gate.unblock()
		paid := <-finished
		require.NoError(t, paid.err)
		status, raw = paid.status, paid.raw
		require.Equal(t, http.StatusOK, status, string(raw))
		var completed billing.Order
		require.NoError(t, json.Unmarshal(raw, &completed))
		require.Equal(t, billing.OrderComplete, completed.Status)
		require.NotNil(t, completed.Payment.PaymentID)
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, pay, key)
		require.Equal(t, http.StatusOK, status, string(raw))
		var replay billing.Order
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, completed.Payment.PaymentID, replay.Payment.PaymentID)
		require.Len(t, provider.effects(t), 1, "replaying the admitted key cannot add a debit")
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
