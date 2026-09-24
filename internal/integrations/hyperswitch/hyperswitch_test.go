package hyperswitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	nmiDest      = "https://secure.nmi.com/api/transact.php"
	goodContract = `{"contract":"openrails-nmi-form-v2","native_vault_delete_contract":"openrails-native-vault-delete-v1","strict":true,"max_response_bytes":65536,"routes":[{"destination_url":"https://secure.nmi.com/api/transact.php","method":"POST","response_profile":"nmi_classic"}]}`
)

func newClient(t *testing.T, readOnly bool, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "api-key=fixture-key" || r.Header.Get("x-profile-id") != "profile-A" {
			t.Errorf("request lost its account authority: %v", r.Header)
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, MerchantID: "merchant-A", ProfileID: "profile-A", APIKey: "fixture-key", ReadOnly: readOnly})
	require.NoError(t, err)
	return c
}

func hijack(w http.ResponseWriter) {
	conn, _, _ := w.(http.Hijacker).Hijack()
	_ = conn.Close()
}

func TestNewRefusesUnboundConfigAndRedactsSecrets(t *testing.T) {
	good := Config{BaseURL: "https://hs.example", MerchantID: "m", ProfileID: "p", APIKey: "k-secret"}
	for name, mut := range map[string]func(*Config){
		"no merchant": func(c *Config) { c.MerchantID = "" },
		"no profile":  func(c *Config) { c.ProfileID = "" },
		"no key":      func(c *Config) { c.APIKey = "" },
		"userinfo":    func(c *Config) { c.BaseURL = "https://u:p@hs.example" },
		"scheme":      func(c *Config) { c.BaseURL = "ftp://hs.example" },
	} {
		cfg := good
		mut(&cfg)
		_, err := New(cfg)
		require.ErrorIs(t, err, ErrBinding, name)
	}
	c, err := New(good)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprintf("%v %+v %#v %s", c, good, good, good.APIKey), "k-secret")
}

// One token-only workflow over real HTTP: a lost customer-create reply recovers by
// exact reference, sessions bind to the customer, method readback is masked-only.
func TestCaptureWorkflowBindsTokenOnlyResources(t *testing.T) {
	var created, raw, foreign atomic.Bool
	var writes atomic.Int32
	c := newClient(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/customers/reference/customer-A":
			if !created.Load() {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write([]byte(`{"id":"vendor-customer-A","merchant_reference_id":"customer-A"}`))
		case "/v2/customers":
			var req map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Equal(t, "customer-A", req["merchant_reference_id"])
			writes.Add(1)
			created.Store(true)
			hijack(w)
		case "/v2/payment-method-sessions":
			var req map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Equal(t, "vendor-customer-A", req["customer_id"])
			require.Equal(t, "persistent", req["storage_type"])
			_, _ = w.Write([]byte(`{"id":"session-A","customer_id":"vendor-customer-A","storage_type":"persistent","expires_at":"2030-01-01T00:00:00","client_secret":"private-session-secret","sdk_authorization":"private-sdk-authorization"}`))
		case "/v2/payment-method-sessions/session-A":
			_, _ = w.Write([]byte(`{"id":"session-A","customer_id":"vendor-customer-A","storage_type":"persistent","expires_at":"2030-01-01T00:00:00Z","client_secret":"CLIENT_SECRET_REDACTED","associated_payment_methods":[{"payment_method_token":{"type":"payment_method_session_token","data":"session-token-A"}}]}`))
		case "/v2/payment-methods/session-token-A":
			require.Equal(t, "false", r.URL.Query().Get("fetch_raw_detail"))
			require.Equal(t, "false", r.URL.Query().Get("force_sync"))
			merchant, extra := "merchant-A", ""
			if foreign.Load() {
				merchant = "merchant-B"
			}
			if raw.Load() {
				extra = `,"raw_payment_method_data":{"card":{"card_number":"must-not-consume-raw-data"}}`
			}
			_, _ = fmt.Fprintf(w, `{"id":"permanent-method-A","merchant_id":%q,"customer_id":"vendor-customer-A","storage_type":"persistent","payment_method_data":{"card":{"last4_digits":"1111","expiry_month":"3","expiry_year":"2030","card_network":"Visa"}}%s}`, merchant, extra)
		default:
			t.Errorf("unexpected vendor path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	for range 2 {
		customer, err := c.EnsureCustomer(ctx, "customer-A", "Fixture", "fixture@example.test")
		require.NoError(t, err)
		require.Equal(t, "vendor-customer-A", customer.ID)
		require.EqualValues(t, 1, writes.Load())
	}

	session, err := c.CreateSession(ctx, "vendor-customer-A")
	require.NoError(t, err)
	require.EqualValues(t, "private-session-secret", session.ClientSecret)
	require.NotContains(t, fmt.Sprintf("%v %+v %#v", session, session, session), "private-")

	got, err := c.GetSession(ctx, "session-A", "vendor-customer-A")
	require.NoError(t, err)
	require.Empty(t, got.ClientSecret, "GET's redacted marker must never pass as a credential")
	require.True(t, got.OwnsToken("session-token-A"))
	require.False(t, got.OwnsToken("other-token"))
	require.False(t, got.OwnsToken(""))
	_, err = c.GetSession(ctx, "session-A", "another-customer")
	require.ErrorIs(t, err, ErrBinding)

	method, err := c.GetMethod(ctx, "session-token-A", "vendor-customer-A")
	require.NoError(t, err)
	require.Equal(t, "permanent-method-A", method.ID)
	require.Equal(t, "1111", method.Data.Card.Last4)
	require.Equal(t, "03/30", method.MaskedExpiry())
	_, err = c.GetMethod(ctx, "session-token-A", "another-customer")
	require.ErrorIs(t, err, ErrBinding)
	foreign.Store(true)
	_, err = c.GetMethod(ctx, "session-token-A", "vendor-customer-A")
	require.ErrorIs(t, err, ErrBinding)
	foreign.Store(false)
	raw.Store(true)
	_, err = c.GetMethod(ctx, "session-token-A", "vendor-customer-A")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "must-not-consume")
}

// Stock or loosened deployments must be refused with a GET only, before any money route.
func TestProxyContractPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"qualified", goodContract, 200, true},
		{"stock health", "health is good", 200, false},
		{"missing endpoint", "", 404, false},
		{"superseded v1", strings.Replace(goodContract, "form-v2", "form-v1", 1), 200, false},
		{"not strict", strings.Replace(goodContract, `"strict":true`, `"strict":false`, 1), 200, false},
		{"unbounded", strings.Replace(goodContract, "65536", "0", 1), 200, false},
		{"other destination", strings.Replace(goodContract, "secure.nmi.com", "other.example", 1), 200, false},
		{"wrong profile", strings.Replace(goodContract, "nmi_classic", "arbitrary_json", 1), 200, false},
		{"GET route", strings.Replace(goodContract, `"POST"`, `"GET"`, 1), 200, false},
		{"no routes", `{"contract":"openrails-nmi-form-v2","strict":true,"max_response_bytes":65536,"routes":[]}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, false, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/v2/proxy", r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			err := c.CheckProxyContract(context.Background(), nmiDest)
			require.Equal(t, tc.ok, err == nil, "err=%v", err)
		})
	}
}

// Only a receipt naming exactly the deleted method completes an erasure; the
// capability, not the money-route list, gates it.
func TestDeleteMethodRequiresCapabilityAndExactReceipt(t *testing.T) {
	const capability = "openrails-native-vault-delete-v1"
	for _, tc := range []struct {
		name, capability, response string
		status                     int
		readonly, lose             bool
		want                       error
		writes                     int32
	}{
		{name: "completion with money routes disabled", capability: capability, response: `{"id":"pm_owned"}`, status: 200, writes: 1},
		{name: "old deployment", status: 200, want: ErrUnavailable},
		{name: "unknown capability", capability: "future-version", status: 200, want: ErrUnavailable},
		{name: "read only", capability: capability, readonly: true, want: ErrReadOnly},
		{name: "404 is not erasure", capability: capability, status: 404, want: ErrUnknown, writes: 1},
		{name: "refusal is not erasure", capability: capability, status: 400, want: ErrUnknown, writes: 1},
		{name: "vault 5xx", capability: capability, status: 500, want: ErrUnknown, writes: 1},
		{name: "another method", capability: capability, response: `{"id":"pm_other"}`, status: 200, want: ErrUnknown, writes: 1},
		{name: "lost success", capability: capability, lose: true, want: ErrUnknown, writes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			c := newClient(t, tc.readonly, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v2/proxy" {
					_ = json.NewEncoder(w).Encode(map[string]any{"contract": "openrails-nmi-form-v2", "native_vault_delete_contract": tc.capability, "strict": false, "routes": []string{}})
					return
				}
				require.Equal(t, http.MethodDelete+" /v2/payment-methods/pm_owned", r.Method+" "+r.URL.Path)
				writes.Add(1)
				if tc.lose {
					hijack(w)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			})
			require.Equal(t, tc.want, c.DeleteMethod(context.Background(), "pm_owned"))
			require.Equal(t, tc.writes, writes.Load())
		})
	}
	c := newClient(t, false, func(http.ResponseWriter, *http.Request) { t.Error("unsafe id reached the network") })
	require.ErrorIs(t, c.DeleteMethod(context.Background(), "4111111111111111"), ErrBinding)
}

// Once the POST is attempted nothing but a strictly-shaped NMI answer is accepted;
// everything before it is provably ErrNotDispatched.
func TestProxyNMIOutcomes(t *testing.T) {
	form := map[string]Secret{"security_key": "psp-secret-key", "type": Secret("sale")}
	ok := `{"status_code":200,"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":"tx_1","authcode":"A1"}}`
	for _, tc := range []struct {
		name, contract, reply string
		readonly              bool
		notDispatched         bool
		wantUnknown           bool
	}{
		{name: "approved", contract: goodContract, reply: ok},
		{name: "declined is a parsed decline", contract: goodContract, reply: `{"status_code":200,"response":{"response":"2","response_code":"200","responsetext":"Declined"}}`},
		{name: "read only", contract: goodContract, readonly: true, notDispatched: true},
		{name: "unqualified deployment", contract: `{}`, notDispatched: true},
		{name: "unexpected field", contract: goodContract, reply: strings.Replace(ok, `"authcode"`, `"cc_number"`, 1), wantUnknown: true},
		{name: "echoed provider key", contract: goodContract, reply: strings.Replace(ok, "tx_1", "psp-secret-key", 1), wantUnknown: true},
		{name: "PAN in transaction id", contract: goodContract, reply: strings.Replace(ok, "tx_1", "4111111111111111", 1), wantUnknown: true},
		{name: "non-200 destination", contract: goodContract, reply: strings.Replace(ok, "200", "502", 1), wantUnknown: true},
		{name: "response headers leak", contract: goodContract, reply: strings.Replace(ok, `"status_code":200`, `"status_code":200,"response_headers":{"x":"y"}`, 1), wantUnknown: true},
		{name: "approval text mismatch", contract: goodContract, reply: strings.Replace(ok, "Approved", "SUCCESS", 1), wantUnknown: true},
		{name: "decline code out of band", contract: goodContract, reply: `{"status_code":200,"response":{"response":"2","response_code":"300","responsetext":"Declined"}}`, wantUnknown: true},
		{name: "proxy 4xx after attempt", contract: goodContract, reply: "", wantUnknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			c := newClient(t, tc.readonly, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(tc.contract))
					return
				}
				posts.Add(1)
				var req map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				require.Equal(t, "pm_owned", req["token"])
				require.Equal(t, "payment_method_id", req["token_type"])
				require.Equal(t, nmiDest, req["destination_url"])
				if tc.reply == "" {
					w.WriteHeader(400)
					return
				}
				_, _ = w.Write([]byte(tc.reply))
			})
			res, err := c.ProxyNMI(context.Background(), nmiDest, "pm_owned", form)
			switch {
			case tc.notDispatched:
				require.ErrorIs(t, err, ErrNotDispatched)
				require.Zero(t, posts.Load())
			case tc.wantUnknown:
				require.Equal(t, ErrUnknown, err)
				require.EqualValues(t, 1, posts.Load())
			case tc.name == "approved":
				require.NoError(t, err)
				require.Equal(t, "tx_1", res.TransactionID)
			default:
				require.Error(t, err)
				require.False(t, errors.Is(err, ErrUnknown) || errors.Is(err, ErrNotDispatched), "decline must be classified, got %v", err)
			}
		})
	}
}
