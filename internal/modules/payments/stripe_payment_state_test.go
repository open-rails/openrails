package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPStripePaymentStateReaderAppliesSubscriptionDefaultPrecedence(t *testing.T) {
	t.Parallel()

	requests := make([]*http.Request, 0, 6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Clone(r.Context()))
		require.Equal(t, "Bearer sk_test_exact", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/customers/cus_1":
			writeStripeTestJSON(t, w, map[string]any{
				"id":               "cus_1",
				"invoice_settings": map[string]any{"default_payment_method": map[string]any{"id": "pm_customer"}},
			})
		case "/v1/subscriptions":
			if r.URL.Query().Get("starting_after") == "" {
				writeStripeTestJSON(t, w, map[string]any{
					"has_more": true,
					"data":     []map[string]any{{"id": "sub_specific", "default_payment_method": "pm_subscription"}},
				})
				return
			}
			require.Equal(t, "sub_specific", r.URL.Query().Get("starting_after"))
			writeStripeTestJSON(t, w, map[string]any{
				"has_more": false,
				"data":     []map[string]any{{"id": "sub_fallback", "default_payment_method": nil}},
			})
		case "/v1/payment_methods/pm_subscription":
			writeStripeTestJSON(t, w, stripePaymentMethodTestPayload("pm_subscription", "cus_1", "1111"))
		case "/v1/payment_methods/pm_customer":
			writeStripeTestJSON(t, w, stripePaymentMethodTestPayload("pm_customer", "cus_1", "2222"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	reader := NewHTTPStripePaymentStateReader("sk_test_exact")
	reader.BaseURL = server.URL
	reader.HTTPClient = server.Client()
	reader.PageLimit = 1

	state, err := reader.CustomerPaymentState(context.Background(), "cus_1")
	require.NoError(t, err)
	require.Equal(t, "cus_1", state.CustomerID)
	require.Len(t, state.Subscriptions, 2)
	require.Equal(t, "pm_subscription", state.Subscriptions[0].PaymentMethod.ID)
	require.Equal(t, "1111", state.Subscriptions[0].PaymentMethod.Card.Last4)
	require.Equal(t, "pm_customer", state.Subscriptions[1].PaymentMethod.ID)
	require.Equal(t, "2222", state.Subscriptions[1].PaymentMethod.Card.Last4)

	require.Len(t, requests, 5)
	query := requests[1].URL.Query()
	require.Equal(t, "cus_1", query.Get("customer"))
	require.Equal(t, "all", query.Get("status"))
	require.Equal(t, "1", query.Get("limit"))
	require.Equal(t, []string{"data.default_payment_method"}, query["expand[]"])
}

func TestHTTPStripePaymentStateReaderIgnoresForeignDefaultMethod(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/customers/cus_1":
			writeStripeTestJSON(t, w, map[string]any{
				"id": "cus_1", "invoice_settings": map[string]any{"default_payment_method": "pm_foreign"},
			})
		case "/v1/subscriptions":
			writeStripeTestJSON(t, w, map[string]any{
				"has_more": false, "data": []map[string]any{{"id": "sub_1"}},
			})
		case "/v1/payment_methods/pm_foreign":
			writeStripeTestJSON(t, w, stripePaymentMethodTestPayload("pm_foreign", "cus_other", "3333"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	reader := NewHTTPStripePaymentStateReader("sk_test_exact")
	reader.BaseURL = server.URL
	reader.HTTPClient = server.Client()
	state, err := reader.CustomerPaymentState(context.Background(), "cus_1")
	require.NoError(t, err)
	require.Len(t, state.Subscriptions, 1)
	require.Nil(t, state.Subscriptions[0].PaymentMethod)
}

func stripePaymentMethodTestPayload(id, customerID, last4 string) map[string]any {
	return map[string]any{
		"id":       id,
		"customer": customerID,
		"card":     map[string]any{"brand": "visa", "last4": last4, "exp_month": 12, "exp_year": 2030},
	}
}

func writeStripeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(value))
}

func TestStripeObjectID(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"string": `"pm_string"`,
		"object": `{"id":"pm_object"}`,
		"null":   `null`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := stripeObjectID(json.RawMessage(raw))
			want := map[string]string{"string": "pm_string", "object": "pm_object", "null": ""}[name]
			require.Equal(t, want, got)
		})
	}
}
