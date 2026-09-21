package hyperswitch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// One token-only workflow drives real HTTP calls, including a lost customer
// create response, exact session association and masked method readback.
func TestCaptureClientBindsTokenOnlyWorkflow(t *testing.T) {
	var created atomic.Bool
	var writes atomic.Int32
	var raw, foreign atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "api-key=fixture-api-key", r.Header.Get("Authorization"))
		require.Equal(t, "profile-A", r.Header.Get("x-profile-id"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/customers/reference/customer-A":
			if !created.Load() {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write([]byte(`{"id":"vendor-customer-A","merchant_reference_id":"customer-A"}`))
		case "/v2/customers":
			var request map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, "customer-A", request["merchant_reference_id"])
			writes.Add(1)
			created.Store(true)
			connection, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = connection.Close() // accepted create, lost reply
		case "/v2/payment-method-sessions":
			var request map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, "vendor-customer-A", request["customer_id"])
			require.Equal(t, "persistent", request["storage_type"])
			_, _ = w.Write([]byte(`{"id":"session-A","customer_id":"vendor-customer-A","storage_type":"persistent","expires_at":"2030-01-01T00:00:00","client_secret":"private-session-secret","sdk_authorization":"private-sdk-authorization"}`))
		case "/v2/payment-method-sessions/session-A":
			_, _ = w.Write([]byte(`{"id":"session-A","customer_id":"vendor-customer-A","storage_type":"persistent","expires_at":"2030-01-01T00:00:00","client_secret":"CLIENT_SECRET_REDACTED","associated_payment_methods":[{"payment_method_token":{"type":"payment_method_session_token","data":"session-token-A"}}]}`))
		case "/v2/payment-methods/session-token-A":
			require.Equal(t, "false", r.URL.Query().Get("fetch_raw_detail"))
			require.Equal(t, "false", r.URL.Query().Get("force_sync"))
			merchant := "merchant-A"
			if foreign.Load() {
				merchant = "merchant-B"
			}
			extra := ""
			if raw.Load() {
				extra = `,"raw_payment_method_data":{"card":{"card_number":"must-not-consume-raw-data"}}`
			}
			_, _ = fmt.Fprintf(w, `{"id":"permanent-method-A","merchant_id":%q,"customer_id":"vendor-customer-A","storage_type":"persistent","payment_method_data":{"card":{"last4_digits":"1111","expiry_month":"12","expiry_year":"2030","card_network":"Visa"}}%s}`, merchant, extra)
		default:
			t.Errorf("unexpected vendor path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, MerchantID: "merchant-A", ProfileID: "profile-A", APIKey: "fixture-api-key"})
	require.NoError(t, err)
	customer, err := client.EnsureCustomer(context.Background(), "customer-A", "Fixture", "fixture@example.test")
	require.NoError(t, err)
	require.Equal(t, "vendor-customer-A", customer.ID)
	require.EqualValues(t, 1, writes.Load())
	_, err = client.EnsureCustomer(context.Background(), "customer-A", "Fixture", "fixture@example.test")
	require.NoError(t, err)
	require.EqualValues(t, 1, writes.Load())
	session, err := client.CreateSession(t.Context(), customer.ID)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprintf("%+v %#v", session, session), "private-")
	retrieved, err := client.GetSession(t.Context(), session.ID, customer.ID)
	require.NoError(t, err)
	require.Empty(t, retrieved.ClientSecret)
	require.True(t, retrieved.OwnsToken("session-token-A"))
	require.False(t, retrieved.OwnsToken("other-token"))
	_, err = client.GetSession(t.Context(), session.ID, "another-customer")
	require.ErrorIs(t, err, ErrBinding)
	method, err := client.GetMethod(t.Context(), "session-token-A", customer.ID)
	require.NoError(t, err)
	require.Equal(t, "permanent-method-A", method.ID)
	require.Equal(t, "1111", method.Data.Card.Last4)
	foreign.Store(true)
	_, err = client.GetMethod(t.Context(), "session-token-A", customer.ID)
	require.ErrorIs(t, err, ErrBinding)
	foreign.Store(false)
	raw.Store(true)
	_, err = client.GetMethod(t.Context(), "session-token-A", customer.ID)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "must-not-consume")
}

func TestProxyPreflightRefusesUnsupportedDeploymentWithoutDispatch(t *testing.T) {
	const destination = "https://secure.nmi.com/api/transact.php"
	good := `{"contract":"openrails-nmi-form-v2","strict":true,"max_response_bytes":65536,"routes":[{"destination_url":"https://secure.nmi.com/api/transact.php","method":"POST","response_profile":"nmi_classic"}]}`
	cases := []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"qualified", good, 200, true},
		{"stock health", "health is good", 200, false},
		{"missing endpoint", "", 404, false},
		{"wrong contract", strings.Replace(good, "openrails-nmi-form-v2", "unqualified", 1), 200, false},
		{"superseded v1 exposes SDK authority", strings.Replace(good, "openrails-nmi-form-v2", "openrails-nmi-form-v1", 1), 200, false},
		{"disabled policy", strings.Replace(good, `"strict":true`, `"strict":false`, 1), 200, false},
		{"unbounded", strings.Replace(good, "65536", "0", 1), 200, false},
		{"different destination", strings.Replace(good, "secure.nmi.com", "other.example", 1), 200, false},
		{"wrong profile", strings.Replace(good, "nmi_classic", "arbitrary_json", 1), 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				require.Equal(t, "/v2/proxy", r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, MerchantID: "merchant-A", ProfileID: "profile-A", APIKey: "fixture-key"})
			require.NoError(t, err)
			err = client.CheckProxyContract(t.Context(), destination)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Zero(t, writes.Load())
		})
	}
}
