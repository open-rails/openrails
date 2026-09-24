package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func stripeStateServer(t *testing.T, routes map[string]any) (*HTTPStripePaymentStateReader, *[]*http.Request) {
	t.Helper()
	var mu sync.Mutex
	var seen []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(r.Context()))
		mu.Unlock()
		require.Equal(t, "Bearer sk_test_exact", r.Header.Get("Authorization"))
		key := r.URL.Path
		if after := r.URL.Query().Get("starting_after"); after != "" {
			key += "?after=" + after
		}
		body, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)
	reader := NewHTTPStripePaymentStateReader(" sk_test_exact ")
	reader.BaseURL, reader.HTTPClient = server.URL+"/", server.Client()
	return reader, &seen
}

func stripeMethodJSON(id, customer, last4 string) map[string]any {
	return map[string]any{"id": id, "customer": customer, "card": map[string]any{"brand": "visa", "last4": last4, "exp_month": 12, "exp_year": 2030}}
}

// A subscription's own default wins over the customer's; a method owned by another customer is never used.
func TestStripePaymentStateDefaultPrecedence(t *testing.T) {
	t.Parallel()
	reader, seen := stripeStateServer(t, map[string]any{
		"/v1/customers/cus_1": map[string]any{"id": "cus_1", "invoice_settings": map[string]any{"default_payment_method": map[string]any{"id": "pm_customer"}}},
		"/v1/subscriptions":   map[string]any{"has_more": true, "data": []map[string]any{{"id": "sub_own", "default_payment_method": "pm_sub"}}},
		"/v1/subscriptions?after=sub_own": map[string]any{"has_more": false, "data": []map[string]any{
			{"id": "sub_fallback", "default_payment_method": nil},
			{"id": "sub_foreign", "default_payment_method": map[string]any{"id": "pm_foreign"}},
			{"id": "sub_gone", "default_payment_method": "pm_gone"},
		}},
		"/v1/payment_methods/pm_sub":      stripeMethodJSON("pm_sub", "cus_1", "1111"),
		"/v1/payment_methods/pm_customer": stripeMethodJSON("pm_customer", "cus_1", "2222"),
		"/v1/payment_methods/pm_foreign":  stripeMethodJSON("pm_foreign", "cus_other", "3333"),
	})
	reader.PageLimit = 1

	state, err := reader.CustomerPaymentState(context.Background(), " cus_1 ")
	require.NoError(t, err)
	require.Equal(t, "cus_1", state.CustomerID)
	require.Len(t, state.Subscriptions, 4)
	require.Equal(t, "sub_own", state.Subscriptions[0].SubscriptionID)
	require.Equal(t, "1111", state.Subscriptions[0].PaymentMethod.Card.Last4)
	require.Equal(t, "pm_customer", state.Subscriptions[1].PaymentMethod.ID)
	require.Equal(t, "Visa", state.Subscriptions[1].PaymentMethod.Card.Brand)
	require.Nil(t, state.Subscriptions[2].PaymentMethod, "a foreign customer's method is never selected")
	require.Nil(t, state.Subscriptions[3].PaymentMethod, "a deleted method reads as none")

	q := (*seen)[1].URL.Query()
	require.Equal(t, "cus_1", q.Get("customer"))
	require.Equal(t, "all", q.Get("status"))
	require.Equal(t, "1", q.Get("limit"))
	require.Equal(t, []string{"data.default_payment_method"}, q["expand[]"])
}

func TestStripePaymentStateRefusesWrongOrMissingCustomer(t *testing.T) {
	t.Parallel()
	reader, _ := stripeStateServer(t, map[string]any{
		"/v1/customers/cus_deleted": map[string]any{"id": "cus_deleted", "deleted": true},
		"/v1/customers/cus_swap":    map[string]any{"id": "cus_other"},
		"/v1/customers/cus_noid":    map[string]any{},
	})
	ctx := context.Background()
	_, err := reader.CustomerPaymentState(ctx, "cus_deleted")
	require.ErrorIs(t, err, ErrStripeObjectNotFound)
	_, err = reader.CustomerPaymentState(ctx, "cus_swap")
	require.ErrorContains(t, err, "does not match")
	_, err = reader.CustomerPaymentState(ctx, "cus_noid")
	require.ErrorContains(t, err, "missing id")
	_, err = reader.CustomerPaymentState(ctx, "cus_404")
	require.ErrorIs(t, err, ErrStripeObjectNotFound)

	state, err := reader.CustomerPaymentState(ctx, " ")
	require.NoError(t, err)
	require.Nil(t, state)
	method, err := reader.PaymentMethod(ctx, "pm_404")
	require.NoError(t, err)
	require.Nil(t, method)
}

func TestStripeObjectID(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{`"pm_1"`: "pm_1", `{"id":" pm_2 "}`: "pm_2", `null`: "", ``: "", `42`: ""} {
		require.Equal(t, want, stripeObjectID(json.RawMessage(raw)), raw)
	}
}
