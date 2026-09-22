package openrails

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCheckoutPurchaseAndActionsHaveDistinctRequests(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.NotContains(t, input, "mode", "the operation is selected by the endpoint or canonical price")
		require.NotEmpty(t, r.Header.Get("Idempotency-Key"))
		switch r.URL.Path {
		case "/v1/merchant/checkout-sessions":
			require.Contains(t, input, "price_id")
			require.NotContains(t, input, "subscription_id")
			require.NotContains(t, input, "new_price_id")
		case "/v1/merchant/payment-method-sessions":
			require.NotContains(t, input, "price_id")
			require.NotContains(t, input, "subscription_id")
		case "/v1/merchant/solana-cancel-sessions":
			require.Contains(t, input, "subscription_id")
			require.NotContains(t, input, "price_id")
			require.NotContains(t, input, "new_price_id")
		case "/v1/merchant/solana-tier-change-sessions":
			require.Contains(t, input, "subscription_id")
			require.Contains(t, input, "new_price_id")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c, err := NewRemote(server.URL, WithAPIKey("test"), WithDefaultMerchant("fixture"))
	require.NoError(t, err)
	customer := CheckoutCustomerIdentity{ID: CustomerID(uuid.New()).String()}
	key := "operation-key"
	_, err = c.CreateCheckoutSession(t.Context(), CreateCheckoutSessionRequest{Customer: customer, PriceID: PriceID(uuid.New()).String(), IdempotencyKey: key})
	require.NoError(t, err)
	_, err = c.CreatePaymentMethodSession(t.Context(), CreatePaymentMethodSessionRequest{Customer: customer, IdempotencyKey: key})
	require.NoError(t, err)
	_, err = c.CreateSolanaCancelSession(t.Context(), CreateSolanaCancelSessionRequest{Customer: customer, SubscriptionID: SubscriptionID(uuid.New()).String(), IdempotencyKey: key})
	require.NoError(t, err)
	_, err = c.CreateSolanaTierChangeSession(t.Context(), CreateSolanaTierChangeSessionRequest{Customer: customer, SubscriptionID: SubscriptionID(uuid.New()).String(), NewPriceID: PriceID(uuid.New()).String(), IdempotencyKey: key})
	require.NoError(t, err)
	require.Len(t, paths, 4)
}
