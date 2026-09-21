package hyperswitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	provider "github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

// The real adapter speaks HTTP throughout. The later actual-vendor invoice
// workflow proves vault interpolation and authoritative receipt settlement;
// this boundary proof distinguishes refusal before send from uncertainty after it.
func TestHyperSwitchNMIChargeBoundary(t *testing.T) {
	const destination = "https://secure.nmi.com/api/transact.php"
	const gatewayKey = "synthetic-psp-key"
	approved := `{"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":"txn_initial","authcode":"auth_1"},"status_code":200,"response_headers":{}}`
	base := charge.Request{
		Instrument:  charge.Instrument{Rail: "nmi", CustomerRef: "customer_A", MethodRef: "method_A"},
		AmountMinor: 1234, Currency: "USD", OrderRef: "operation_A", Description: "A&B=+50% café",
		Context: charge.InitialOneTime(),
	}
	for _, tc := range []struct {
		name      string
		request   func(*charge.Request)
		mode      string
		body      string
		status    int
		wantError error
		declined  bool
		wantPosts int32
		amount    string
		recurring bool
	}{
		{name: "initial USD", amount: "12.34", wantPosts: 1},
		{name: "JPY native units", request: func(r *charge.Request) { r.Currency = "JPY"; r.AmountMinor = 100 }, amount: "100.00", wantPosts: 1},
		{name: "full int64 exact", request: func(r *charge.Request) { r.AmountMinor = moneyutil.Cents(math.MaxInt64) }, amount: "92233720368547758.07", wantPosts: 1},
		{name: "subsequent customer", request: func(r *charge.Request) { r.Context = charge.OneTimeReuse("txn_initial") }, wantPosts: 1},
		{name: "unscheduled merchant", request: func(r *charge.Request) { r.Context = charge.UnscheduledMIT("txn_initial") }, wantPosts: 1},
		{name: "declined", body: `{"response":{"response":"2","response_code":"200","responsetext":"Declined"},"status_code":200,"response_headers":{}}`, declined: true, wantPosts: 1},
		{name: "duplicate refusal uncertain", body: `{"response":{"response":"3","response_code":"430","responsetext":"Gateway error"},"status_code":200,"response_headers":{}}`, wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "lost reply", mode: "lost", wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "malformed response", body: `{`, wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "oversized response", body: strings.Repeat(" ", 65537), wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "approval without receipt", body: strings.Replace(approved, `"txn_initial"`, `""`, 1), wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "contradicted code", body: strings.Replace(approved, `"100"`, `"200"`, 1), wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "hostile retained key", body: strings.Replace(approved, "txn_initial", gatewayKey, 1), wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "hostile card in retained id", body: strings.Replace(approved, "txn_initial", "4111111111111111", 1), wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "raw provider headers", body: strings.Replace(approved, `"response_headers":{}`, `"response_headers":{"x-secret":"must-not-surface"}`, 1), wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "redirect after dispatch", status: 307, wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "proxy denial after attempted write", status: 403, wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "missing preflight", mode: "missing", wantError: provider.ErrUnavailable},
		{name: "stock preflight", mode: "stock", wantError: provider.ErrUnavailable},
		{name: "disabled route", mode: "disabled", wantError: provider.ErrUnavailable},
		{name: "foreign customer", mode: "foreign customer", wantError: provider.ErrBinding},
		{name: "foreign merchant", mode: "foreign merchant", wantError: provider.ErrBinding},
		{name: "retargeted method", mode: "retargeted method", wantError: provider.ErrBinding},
		{name: "readonly", mode: "readonly", wantError: provider.ErrReadOnly},
		{name: "recurring initial USD", recurring: true, amount: "12.34", wantPosts: 1},
		{name: "recurring initial JPY", recurring: true, request: func(r *charge.Request) { r.Currency = "JPY"; r.AmountMinor = 100 }, amount: "100.00", wantPosts: 1},
		{name: "recurring customer reuse", recurring: true, request: func(r *charge.Request) { r.Context = charge.RecurringReuse("original_recurring") }, amount: "12.34", wantPosts: 1},
		{name: "recurring structured decline", recurring: true, body: `{"response":{"response":"2","response_code":"200","responsetext":"Declined"},"status_code":200,"response_headers":{}}`, declined: true, wantPosts: 1},
		{name: "recurring gateway uncertainty", recurring: true, body: `{"response":{"response":"3","response_code":"430","responsetext":"Gateway error"},"status_code":200,"response_headers":{}}`, wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "recurring lost reply", recurring: true, mode: "lost", wantError: provider.ErrUnknown, wantPosts: 1},
		{name: "recurring missing preflight", recurring: true, mode: "missing", wantError: provider.ErrUnavailable},
		{name: "recurring foreign customer", recurring: true, mode: "foreign customer", wantError: provider.ErrBinding},
		{name: "recurring readonly", recurring: true, mode: "readonly", wantError: provider.ErrReadOnly},
		{name: "recurring wrong rail", recurring: true, request: func(r *charge.Request) { r.Instrument.Rail = "stripe" }, wantError: charge.ErrNotDispatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			req := base
			if tc.recurring {
				req.Context = charge.InitialRecurring()
			}
			if tc.request != nil {
				tc.request(&req)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "api-key=synthetic-custodian-key", r.Header.Get("Authorization"))
				require.Equal(t, "profile_A", r.Header.Get("x-profile-id"))
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v2/payment-methods/method_A" {
					require.Equal(t, "false", r.URL.Query().Get("fetch_raw_detail"))
					merchant, customer, method := "merchant_A", "customer_A", "method_A"
					switch tc.mode {
					case "foreign customer":
						customer = "customer_B"
					case "foreign merchant":
						merchant = "merchant_B"
					case "retargeted method":
						method = "method_B"
					}
					_, _ = fmt.Fprintf(w, `{"id":%q,"merchant_id":%q,"customer_id":%q,"storage_type":"persistent","payment_method_data":{"card":{"last4_digits":"1111","expiry_month":"12","expiry_year":"2030"}}}`, method, merchant, customer)
					return
				}
				require.Equal(t, "/v2/proxy", r.URL.Path)
				if r.Method == http.MethodGet {
					if tc.mode == "missing" {
						w.WriteHeader(404)
						return
					}
					contract := "openrails-nmi-form-v2"
					if tc.mode == "stock" {
						contract = "openrails-nmi-form-v1"
					}
					_, _ = fmt.Fprintf(w, `{"contract":%q,"strict":%t,"max_response_bytes":65536,"routes":[{"destination_url":%q,"method":"POST","response_profile":"nmi_classic"}]}`, contract, tc.mode != "disabled", destination)
					return
				}
				posts.Add(1)
				var body map[string]string
				// Explicit wire names ensure the same public vendor bytes are tested.
				var raw map[string]json.RawMessage
				require.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
				require.NoError(t, json.Unmarshal(raw["request_body"], &body))
				for key, expected := range map[string]string{"token": "method_A", "token_type": "payment_method_id", "destination_url": destination, "request_encoding": "form_urlencoded", "response_encoding": "form_urlencoded", "method": "POST"} {
					var got string
					require.NoError(t, json.Unmarshal(raw[key], &got))
					require.Equal(t, expected, got, key)
				}
				require.Equal(t, "{{$card_number}}", body["ccnumber"])
				require.Equal(t, "{{$card_expiry_mmyy}}", body["ccexp"])
				require.Equal(t, gatewayKey, body["security_key"])
				require.Equal(t, req.Description, body["order_description"])
				require.Equal(t, req.OrderRef, body["orderid"])
				require.Equal(t, req.Currency, body["currency"])
				require.Equal(t, string(req.Context.Initiator), body["initiated_by"])
				if tc.recurring {
					require.Equal(t, "recurring", body["billing_method"])
				} else {
					require.Empty(t, body["billing_method"], "invoice anchor is never recurring")
				}
				indicator := "used"
				if req.Context.FirstUse {
					indicator = "stored"
				}
				require.Equal(t, indicator, body["stored_credential_indicator"])
				for _, key := range []string{"recurring", "subscription_id", "plan_id", "start_date", "customer_vault_id", "billing_id"} {
					require.Empty(t, body[key], "the custodian charge never creates or addresses a provider schedule/vault")
				}
				require.Equal(t, req.Context.PriorRef, body["initial_transaction_id"])
				if tc.amount != "" {
					require.Equal(t, tc.amount, body["amount"])
				}
				if tc.mode == "lost" {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
					return
				}
				if tc.status != 0 {
					w.Header().Set("Location", destination)
					w.WriteHeader(tc.status)
				}
				bodyText := tc.body
				if bodyText == "" {
					bodyText = approved
				}
				_, _ = w.Write([]byte(bodyText))
			}))
			defer server.Close()
			client, err := provider.New(provider.Config{BaseURL: server.URL, MerchantID: "merchant_A", ProfileID: "profile_A", APIKey: "synthetic-custodian-key", ReadOnly: tc.mode == "readonly"})
			require.NoError(t, err)
			charger := &Charger{Client: client, Destination: destination, SecurityKey: gatewayKey}
			require.NotContains(t, fmt.Sprintf("%+v %#v", charger, charger), gatewayKey)
			var result charge.Result
			var refusal *nmi.CustomerVaultError
			if tc.recurring {
				result, refusal, err = charger.ChargeInitialRecurring(t.Context(), req)
			} else {
				result, err = charger.Charge(t.Context(), req)
			}
			if tc.recurring && tc.declined {
				require.NotNil(t, refusal)
				require.Equal(t, 200, refusal.ResponseCode)
				require.NotEmpty(t, refusal.LocalizationID)
				require.Contains(t, refusal.RawResponse, "response_code=200")
				require.NotContains(t, fmt.Sprintf("%+v", refusal), gatewayKey)
			} else {
				require.Nil(t, refusal, "uncertainty or preflight failure cannot become provider decline custody")
			}
			if tc.wantError != nil {
				require.ErrorIs(t, err, tc.wantError)
				require.Equal(t, tc.wantPosts == 0, errors.Is(err, charge.ErrNotDispatched), "only errors before POST prove nonexecution")
				require.Empty(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.declined, result.Declined)
				if tc.declined {
					require.Equal(t, "transaction_was_declined_by_processor", *result.FailureCode)
				}
				if !tc.declined && req.Context.FirstUse {
					require.Equal(t, "txn_initial", result.CapturedRef)
				} else {
					require.Empty(t, result.CapturedRef)
				}
			}
			require.Equal(t, tc.wantPosts, posts.Load(), "no lost-response retry or preflight bypass")
		})
	}
	for _, mutate := range []func(*charge.Request){
		func(r *charge.Request) { r.Context = charge.RecurringMIT("initial") },
		func(r *charge.Request) { r.Context = charge.UnscheduledMIT("") },
		func(r *charge.Request) { r.Context.Initiator = "" },
		func(r *charge.Request) { r.Context.Initiator = charge.InitiatorMerchant },
		func(r *charge.Request) { r.AmountMinor = 0 },
		func(r *charge.Request) { r.OrderRef = "" },
		func(r *charge.Request) { r.Currency = "" },
	} {
		request := base
		mutate(&request)
		_, err := saleForm(request, gatewayKey)
		require.Error(t, err)
	}
	for _, posture := range []charge.Context{
		charge.InitialOneTime(), charge.OneTimeReuse("unscheduled"), charge.RecurringMIT("recurring"),
		charge.RecurringReuse(""), charge.RecurringReuse(" padded "),
		{Agreement: charge.AgreementRecurring, Initiator: charge.InitiatorCustomer, FirstUse: true, PriorRef: "contradiction"},
	} {
		request := base
		request.Context = posture
		result, refusal, err := (*Charger)(nil).ChargeInitialRecurring(t.Context(), request)
		require.ErrorIs(t, err, charge.ErrNotDispatched)
		require.Empty(t, result)
		require.Nil(t, refusal)
	}
	request := base
	request.Context = charge.InitialRecurring()
	_, err := (*Charger)(nil).Charge(t.Context(), request)
	require.ErrorIs(t, err, charge.ErrNotDispatched, "existing invoice entry does not admit recurring charges")
}
