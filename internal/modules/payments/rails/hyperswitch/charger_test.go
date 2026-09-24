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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	provider "github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	destination = "https://secure.nmi.com/api/transact.php"
	gatewayKey  = "synthetic-psp-key"
	approved    = `{"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":"txn_initial","authcode":"auth_1"},"status_code":200,"response_headers":{}}`
	declined    = `{"response":{"response":"2","response_code":"200","responsetext":"Declined"},"status_code":200,"response_headers":{}}`
	gateway430  = `{"response":{"response":"3","response_code":"430","responsetext":"Gateway error"},"status_code":200,"response_headers":{}}`
)

type entry int

const (
	invoice entry = iota
	enrollment
	renewal
)

// fakeHyperSwitch is the vendor at its HTTP boundary: method lookup, proxy preflight, proxied NMI post.
func fakeHyperSwitch(t *testing.T, mode, body string, status int, req charge.Request, amount string, posts *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "api-key=synthetic-custodian-key", r.Header.Get("Authorization"))
		require.Equal(t, "profile_A", r.Header.Get("x-profile-id"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/payment-methods/method_A" {
			require.Equal(t, "false", r.URL.Query().Get("fetch_raw_detail"))
			merchant, customer, method := "merchant_A", "customer_A", "method_A"
			switch mode {
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
			if mode == "missing" {
				w.WriteHeader(404)
				return
			}
			contract := "openrails-nmi-form-v2"
			if mode == "stock" {
				contract = "openrails-nmi-form-v1"
			}
			_, _ = fmt.Fprintf(w, `{"contract":%q,"strict":%t,"max_response_bytes":65536,"routes":[{"destination_url":%q,"method":"POST","response_profile":"nmi_classic"}]}`, contract, mode != "disabled", destination)
			return
		}
		posts.Add(1)
		var raw map[string]json.RawMessage
		var form map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
		require.NoError(t, json.Unmarshal(raw["request_body"], &form))
		for key, want := range map[string]string{"token": "method_A", "token_type": "payment_method_id", "destination_url": destination, "request_encoding": "form_urlencoded", "response_encoding": "form_urlencoded", "method": "POST"} {
			var got string
			require.NoError(t, json.Unmarshal(raw[key], &got))
			require.Equal(t, want, got, key)
		}
		indicator, billing := "used", ""
		if req.Context.FirstUse {
			indicator = "stored"
		}
		if req.Context.Agreement == charge.AgreementRecurring {
			billing = "recurring"
		}
		for key, want := range map[string]string{
			"ccnumber": "{{$card_number}}", "ccexp": "{{$card_expiry_mmyy}}", "security_key": gatewayKey,
			"order_description": req.Description, "orderid": req.OrderRef, "currency": req.Currency,
			"initiated_by": string(req.Context.Initiator), "stored_credential_indicator": indicator,
			"billing_method": billing, "initial_transaction_id": req.Context.PriorRef,
			// The custodian charge never creates or addresses a provider schedule or vault.
			"recurring": "", "subscription_id": "", "plan_id": "", "start_date": "", "customer_vault_id": "", "billing_id": "",
		} {
			require.Equal(t, want, form[key], key)
		}
		if amount != "" {
			require.Equal(t, amount, form["amount"])
		}
		if mode == "lost" {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		if status != 0 {
			w.Header().Set("Location", destination)
			w.WriteHeader(status)
		}
		reply := body
		if reply == "" {
			reply = approved
		}
		_, _ = w.Write([]byte(reply))
	}))
}

// Refusal before send proves non-execution; anything after the POST is a decline only when the gateway says so cleanly.
func TestHyperSwitchNMIChargeBoundary(t *testing.T) {
	t.Parallel()
	base := charge.Request{
		Instrument:  charge.Instrument{Rail: "nmi", CustomerRef: "customer_A", MethodRef: "method_A"},
		AmountMinor: 1234, Currency: "USD", OrderRef: "operation_A", Description: "A&B=+50% café",
		Context: charge.InitialOneTime(),
	}
	jpy := func(r *charge.Request) { r.Currency, r.AmountMinor = "JPY", 100 }
	ctx := func(c charge.Context) func(*charge.Request) { return func(r *charge.Request) { r.Context = c } }
	for _, tc := range []struct {
		name     string
		entry    entry
		request  func(*charge.Request)
		mode     string
		body     string
		status   int
		wantErr  error
		declined bool
		posted   bool
		amount   string
	}{
		{name: "initial USD", amount: "12.34", posted: true},
		{name: "JPY native units", request: jpy, amount: "100.00", posted: true},
		{name: "full int64 exact", request: func(r *charge.Request) { r.AmountMinor = moneyutil.Cents(math.MaxInt64) }, amount: "92233720368547758.07", posted: true},
		{name: "subsequent customer", request: ctx(charge.OneTimeReuse("txn_initial")), posted: true},
		{name: "unscheduled merchant", request: ctx(charge.UnscheduledMIT("txn_initial")), posted: true},
		{name: "declined", body: declined, declined: true, posted: true},
		{name: "430 is uncertain", body: gateway430, wantErr: provider.ErrUnknown, posted: true},
		{name: "unqualified error cannot decline", body: `{"response":{"response":"3","response_code":"400","responsetext":"RAW_PROVIDER_SENTINEL"},"status_code":200,"response_headers":{}}`, wantErr: provider.ErrUnknown, posted: true},
		{name: "missing refusal code", body: `{"response":{"response":"2"},"status_code":200,"response_headers":{}}`, wantErr: provider.ErrUnknown, posted: true},
		{name: "contradictory refusal", body: `{"response":{"response":"2","response_code":"100"},"status_code":200,"response_headers":{}}`, wantErr: provider.ErrUnknown, posted: true},
		{name: "lost reply", mode: "lost", wantErr: provider.ErrUnknown, posted: true},
		{name: "malformed response", body: `{`, wantErr: provider.ErrUnknown, posted: true},
		{name: "oversized response", body: strings.Repeat(" ", 65537), wantErr: provider.ErrUnknown, posted: true},
		{name: "approval without receipt", body: strings.Replace(approved, `"txn_initial"`, `""`, 1), wantErr: provider.ErrUnknown, posted: true},
		{name: "contradicted code", body: strings.Replace(approved, `"100"`, `"200"`, 1), wantErr: provider.ErrUnknown, posted: true},
		{name: "hostile retained key", body: strings.Replace(approved, "txn_initial", gatewayKey, 1), wantErr: provider.ErrUnknown, posted: true},
		{name: "hostile card in retained id", body: strings.Replace(approved, "txn_initial", "4111111111111111", 1), wantErr: provider.ErrUnknown, posted: true},
		{name: "raw provider headers", body: strings.Replace(approved, `"response_headers":{}`, `"response_headers":{"x-secret":"must-not-surface"}`, 1), wantErr: provider.ErrUnknown, posted: true},
		{name: "redirect after dispatch", status: 307, wantErr: provider.ErrUnknown, posted: true},
		{name: "proxy denial after attempted write", status: 403, wantErr: provider.ErrUnknown, posted: true},
		{name: "missing preflight", mode: "missing", wantErr: provider.ErrUnavailable},
		{name: "stock preflight", mode: "stock", wantErr: provider.ErrUnavailable},
		{name: "disabled route", mode: "disabled", wantErr: provider.ErrUnavailable},
		{name: "foreign customer", mode: "foreign customer", wantErr: provider.ErrBinding},
		{name: "foreign merchant", mode: "foreign merchant", wantErr: provider.ErrBinding},
		{name: "retargeted method", mode: "retargeted method", wantErr: provider.ErrBinding},
		{name: "readonly", mode: "readonly", wantErr: provider.ErrReadOnly},
		{name: "invoice rejects recurring initial", request: ctx(charge.InitialRecurring()), wantErr: charge.ErrNotDispatched},
		{name: "invoice rejects recurring merchant", request: ctx(charge.RecurringMIT("recurring")), wantErr: charge.ErrNotDispatched},

		{name: "enrollment USD", entry: enrollment, amount: "12.34", posted: true},
		{name: "enrollment JPY", entry: enrollment, request: jpy, amount: "100.00", posted: true},
		{name: "enrollment reuse", entry: enrollment, request: ctx(charge.RecurringReuse("original_recurring")), posted: true},
		{name: "enrollment decline", entry: enrollment, body: declined, declined: true, posted: true},
		{name: "enrollment 430", entry: enrollment, body: gateway430, wantErr: provider.ErrUnknown, posted: true},
		{name: "enrollment lost reply", entry: enrollment, mode: "lost", wantErr: provider.ErrUnknown, posted: true},
		{name: "enrollment foreign customer", entry: enrollment, mode: "foreign customer", wantErr: provider.ErrBinding},
		{name: "enrollment readonly", entry: enrollment, mode: "readonly", wantErr: provider.ErrReadOnly},
		{name: "enrollment wrong rail", entry: enrollment, request: func(r *charge.Request) { r.Instrument.Rail = "stripe" }, wantErr: charge.ErrNotDispatched},
		{name: "enrollment rejects unscheduled initial", entry: enrollment, request: ctx(charge.InitialOneTime()), wantErr: charge.ErrNotDispatched},
		{name: "enrollment rejects unscheduled reuse", entry: enrollment, request: ctx(charge.OneTimeReuse("unscheduled")), wantErr: charge.ErrNotDispatched},
		{name: "enrollment rejects merchant", entry: enrollment, request: ctx(charge.RecurringMIT("recurring")), wantErr: charge.ErrNotDispatched},
		{name: "enrollment rejects missing anchor", entry: enrollment, request: ctx(charge.RecurringReuse("")), wantErr: charge.ErrNotDispatched},
		{name: "enrollment rejects padded anchor", entry: enrollment, request: ctx(charge.RecurringReuse(" padded ")), wantErr: charge.ErrNotDispatched},
		{name: "enrollment rejects first-use anchor", entry: enrollment, request: func(r *charge.Request) { r.Context.PriorRef = "contradiction" }, wantErr: charge.ErrNotDispatched},

		{name: "renewal USD", entry: renewal, amount: "12.34", posted: true},
		{name: "renewal JPY", entry: renewal, request: jpy, amount: "100.00", posted: true},
		{name: "renewal decline", entry: renewal, body: declined, declined: true, posted: true},
		{name: "renewal lost reply", entry: renewal, mode: "lost", wantErr: provider.ErrUnknown, posted: true},
		{name: "renewal rejects customer initial", entry: renewal, request: ctx(charge.InitialRecurring()), wantErr: charge.ErrNotDispatched},
		{name: "renewal rejects customer reuse", entry: renewal, request: ctx(charge.RecurringReuse("original_recurring")), wantErr: charge.ErrNotDispatched},
		{name: "renewal rejects unscheduled merchant", entry: renewal, request: ctx(charge.UnscheduledMIT("unscheduled")), wantErr: charge.ErrNotDispatched},
		{name: "renewal rejects missing anchor", entry: renewal, request: ctx(charge.RecurringMIT("")), wantErr: charge.ErrNotDispatched},
		{name: "renewal rejects padded anchor", entry: renewal, request: ctx(charge.RecurringMIT(" padded ")), wantErr: charge.ErrNotDispatched},
		{name: "renewal rejects first use", entry: renewal, request: func(r *charge.Request) { r.Context.FirstUse = true }, wantErr: charge.ErrNotDispatched},
		{name: "renewal wrong rail", entry: renewal, request: func(r *charge.Request) { r.Instrument.Rail = "stripe" }, wantErr: charge.ErrNotDispatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := base
			switch tc.entry {
			case enrollment:
				req.Context = charge.InitialRecurring()
			case renewal:
				req.Context = charge.RecurringMIT("original_recurring")
			}
			if tc.request != nil {
				tc.request(&req)
			}
			var posts atomic.Int32
			server := fakeHyperSwitch(t, tc.mode, tc.body, tc.status, req, tc.amount, &posts)
			defer server.Close()
			client, err := provider.New(provider.Config{BaseURL: server.URL, MerchantID: "merchant_A", ProfileID: "profile_A", APIKey: "synthetic-custodian-key", ReadOnly: tc.mode == "readonly"})
			require.NoError(t, err)
			posture, err := nmi.ProxyPostureClient(uuid.New(), uuid.New(), "gateway-account", &config.NMIProviderSettings{SecurityKey: gatewayKey}, destination, false)
			require.NoError(t, err)
			charger := &Charger{Client: client, Destination: destination, SecurityKey: gatewayKey, Posture: posture}
			require.NotContains(t, fmt.Sprintf("%+v %#v", charger, charger), gatewayKey)

			var result charge.Result
			var refusal *nmi.CustomerVaultError
			switch tc.entry {
			case enrollment:
				result, refusal, err = charger.ChargeInitialRecurring(t.Context(), req)
			case renewal:
				result, refusal, err = charger.ChargeRecurringMIT(t.Context(), req)
			default:
				result, err = charger.Charge(t.Context(), req)
			}
			wantPosts := int32(0)
			if tc.posted {
				wantPosts = 1
			}
			require.Equal(t, wantPosts, posts.Load(), "no lost-response retry or preflight bypass")
			if tc.entry != invoice && tc.declined {
				require.NotNil(t, refusal)
				require.Equal(t, 200, refusal.ResponseCode)
				require.NotEmpty(t, refusal.LocalizationID)
				require.Contains(t, refusal.RawResponse, "response_code=200")
				require.Contains(t, refusal.RawResponse, "response=2")
				require.NotContains(t, fmt.Sprintf("%+v", refusal), gatewayKey)
			} else {
				require.Nil(t, refusal, "uncertainty or preflight failure cannot become decline custody")
			}
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Equal(t, !tc.posted, errors.Is(err, charge.ErrNotDispatched), "only errors before POST prove non-execution")
				require.Empty(t, result)
				return
			}
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
		})
	}

	for name, mutate := range map[string]func(*charge.Request){
		"reference-less MIT": func(r *charge.Request) { r.Context = charge.UnscheduledMIT("") },
		"no initiator":       func(r *charge.Request) { r.Context.Initiator = "" },
		"merchant first use": func(r *charge.Request) { r.Context.Initiator = charge.InitiatorMerchant },
		"zero amount":        func(r *charge.Request) { r.AmountMinor = 0 },
		"no operation":       func(r *charge.Request) { r.OrderRef = "" },
		"no currency":        func(r *charge.Request) { r.Currency = "" },
	} {
		req := base
		mutate(&req)
		_, err := saleForm(req, gatewayKey)
		require.Error(t, err, name)
	}
}
