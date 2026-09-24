package nmiproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	tokenID  = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	intentID = "9b2b0d6c-0000-4000-8000-000000000001"
	ntID     = "7c1f3a10-0000-4000-8000-000000000002"
)

var gw = GatewayConfig{SecurityKey: "sk_test_123"}

func saleRequest(ctx charge.Context) charge.Request {
	return charge.Request{AmountMinor: 199, Currency: "USD", Description: "Renewal", OrderRef: "order-1", Context: ctx}
}

// Money wall: the exact urlencoded body BT forwards to NMI, expressions byte-exact.
func TestSaleFormWire(t *testing.T) {
	t.Parallel()
	form, err := SaleForm(saleRequest(charge.RecurringMIT("9001")), Source{TokenID: tokenID}, gw, nil)
	require.NoError(t, err)
	require.Equal(t, url.Values{
		"type": {"sale"}, "security_key": {"sk_test_123"}, "amount": {"1.99"}, "currency": {"USD"},
		"order_description": {"Renewal"}, "orderid": {"order-1"},
		"ccnumber":     {`{{ token: ` + tokenID + ` | json: "$.data.number" }}`},
		"ccexp":        {`{{ token: ` + tokenID + ` | json: "$.data" | card_exp: "MMYY" }}`},
		"initiated_by": {"merchant"}, "stored_credential_indicator": {"used"}, "initial_transaction_id": {"9001"}, "billing_method": {"recurring"},
	}.Encode(), form.Encode(), "MITs are CVC-less")

	for _, tc := range []struct {
		name string
		ctx  charge.Context
		src  Source
		cg   *basistheory.Cryptogram
		want map[string]string
		none []string
	}{
		{"initial recurring CIT from intent", charge.InitialRecurring(), Source{TokenIntentID: intentID}, nil, map[string]string{
			"ccnumber":     `{{ token_intent: ` + intentID + ` | json: "$.data.number" }}`,
			"ccexp":        `{{ token_intent: ` + intentID + ` | json: "$.data" | card_exp: "MMYY" }}`,
			"cvv":          `{{ token_intent: ` + intentID + ` | json: "$.data.cvc" }}`,
			"initiated_by": "customer", "stored_credential_indicator": "stored", "billing_method": "recurring",
		}, []string{"initial_transaction_id"}},
		{"initial unscheduled CIT", charge.InitialOneTime(), Source{TokenIntentID: intentID}, nil,
			map[string]string{"stored_credential_indicator": "stored", "initiated_by": "customer"}, []string{"billing_method"}},
		{"unscheduled MIT", charge.UnscheduledMIT("7777"), Source{TokenID: tokenID}, nil,
			map[string]string{"initiated_by": "merchant", "stored_credential_indicator": "used", "initial_transaction_id": "7777"}, []string{"billing_method", "cvv"}},
		{"network token CIT", charge.InitialRecurring(), Source{TokenID: tokenID, Via: ViaNetworkToken, NetworkTokenID: ntID}, &basistheory.Cryptogram{Cryptogram: "2z8pd6WGPUi/BBesvjJcyw==", ECI: "07"}, map[string]string{
			"ccnumber": `{{ network_token: ` + ntID + ` | json: "$.data.number" }}`,
			"ccexp":    `{{ network_token: ` + ntID + ` | json: "$.data" | card_exp: "MMYY" }}`,
			"cavv":     "2z8pd6WGPUi/BBesvjJcyw==", "eci": "07",
		}, nil},
		{"network token MIT rides the anchor", charge.RecurringMIT("9001"), Source{Via: ViaNetworkToken, NetworkTokenID: ntID}, nil,
			map[string]string{"initial_transaction_id": "9001"}, []string{"cavv", "eci"}},
	} {
		form, err := SaleForm(saleRequest(tc.ctx), tc.src, gw, tc.cg)
		require.NoError(t, err, tc.name)
		for k, v := range tc.want {
			require.Equal(t, v, form.Get(k), "%s: %s", tc.name, k)
		}
		for _, k := range tc.none {
			require.False(t, form.Has(k), "%s: %s", tc.name, k)
		}
	}

	for cents, want := range map[moneyutil.Cents]string{199: "1.99", 100: "1.00", 1: "0.01", 123456: "1234.56"} {
		form, err := SaleForm(charge.Request{AmountMinor: cents, Currency: "USD", Context: charge.InitialOneTime()}, Source{TokenID: tokenID}, gw, nil)
		require.NoError(t, err)
		require.Equal(t, want, form.Get("amount"))
	}
}

// No silent defaults: every missing or contradictory term is a loud error before any I/O.
func TestSaleFormRefusals(t *testing.T) {
	t.Parallel()
	req := func(mut func(*charge.Request)) charge.Request {
		r := saleRequest(charge.InitialOneTime())
		mut(&r)
		return r
	}
	for name, tc := range map[string]struct {
		req charge.Request
		src Source
		gw  GatewayConfig
	}{
		"no security key":            {saleRequest(charge.InitialOneTime()), Source{TokenID: tokenID}, GatewayConfig{}},
		"no currency":                {req(func(r *charge.Request) { r.Currency = " " }), Source{TokenID: tokenID}, gw},
		"zero amount":                {req(func(r *charge.Request) { r.AmountMinor = 0 }), Source{TokenID: tokenID}, gw},
		"no source":                  {saleRequest(charge.InitialOneTime()), Source{}, gw},
		"MIT from token intent":      {saleRequest(charge.RecurringMIT("1")), Source{TokenIntentID: intentID}, gw},
		"reference-less recurring":   {saleRequest(charge.RecurringMIT("")), Source{TokenID: tokenID}, gw},
		"reference-less unscheduled": {saleRequest(charge.UnscheduledMIT("")), Source{TokenID: tokenID}, gw},
		"reference-less reuse CIT":   {saleRequest(charge.OneTimeReuse("")), Source{TokenID: tokenID}, gw},
		"NT CIT without cryptogram":  {saleRequest(charge.InitialRecurring()), Source{Via: ViaNetworkToken, NetworkTokenID: ntID}, gw},
		"NT without id":              {saleRequest(charge.RecurringMIT("1")), Source{Via: ViaNetworkToken}, gw},
		"unknown via":                {saleRequest(charge.RecurringMIT("1")), Source{TokenID: tokenID, Via: "carrier_pigeon"}, gw},
		"51-char order id":           {req(func(r *charge.Request) { r.OrderRef = strings.Repeat("x", 51) }), Source{TokenID: tokenID}, gw},
		"unregistered currency":      {req(func(r *charge.Request) { r.Currency = "XYZ" }), Source{TokenID: tokenID}, gw},
	} {
		_, err := SaleForm(tc.req, tc.src, tc.gw, nil)
		require.Error(t, err, name)
	}
}

type fakeProxy struct {
	calls int
	serve func(w http.ResponseWriter, r *http.Request)
}

func newProxyCharger(t *testing.T, p *fakeProxy) *Charger {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls++
		p.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	bt, err := basistheory.New(basistheory.Config{APIKey: "key_test", BaseURL: srv.URL})
	require.NoError(t, err)
	posture, err := nmi.ProxyPostureClient(uuid.New(), uuid.New(), "gateway-account", &config.NMIProviderSettings{SecurityKey: "sk"}, "https://gw.example/transact.php", false)
	require.NoError(t, err)
	return New(bt, GatewayConfig{SecurityKey: "sk", DirectPostURL: "https://gw.example/transact.php", Posture: posture})
}

func gatewayReply(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(basistheory.ProxyDestinationStatusHeader, "200")
		_, _ = w.Write([]byte(body))
	}
}

// Three-way outcome: approval, parsed hard decline (a Result), or error for the verify machinery.
func TestChargerOutcomes(t *testing.T) {
	t.Parallel()
	mit := charge.Request{Instrument: charge.Instrument{Rail: Rail, MethodRef: tokenID}, AmountMinor: 199, Currency: "USD", OrderRef: "ord-1", Context: charge.RecurringMIT("9001")}
	initial := mit
	initial.Context = charge.InitialRecurring()

	p := &fakeProxy{serve: gatewayReply("response=1&responsetext=SUCCESS&transactionid=424242&response_code=100")}
	res, err := newProxyCharger(t, p).Charge(context.Background(), mit)
	require.NoError(t, err)
	require.Equal(t, charge.Result{TransactionID: "424242", TokenType: charge.TokenTypePANViaProxy}, res, "a prior-ref charge never re-anchors")

	p = &fakeProxy{serve: gatewayReply("response=1&transactionid=555")}
	res, err = newProxyCharger(t, p).Charge(context.Background(), initial)
	require.NoError(t, err)
	require.Equal(t, "555", res.CapturedRef)

	p = &fakeProxy{serve: gatewayReply("response=2&responsetext=DECLINE&transactionid=99&response_code=202")}
	res, err = newProxyCharger(t, p).Charge(context.Background(), mit)
	require.NoError(t, err)
	require.True(t, res.Declined)
	require.Equal(t, "insufficient_funds", *res.FailureCode)

	for _, body := range []string{
		"response=3&responsetext=Duplicate&response_code=430",
		"response=3&response_code=400&responsetext=RAW_PROVIDER_SENTINEL",
		"response=2&response=1&response_code=202&response_code=100",
		"response=2&response_code=100",
		"response=2",
	} {
		p := &fakeProxy{serve: gatewayReply(body)}
		res, err := newProxyCharger(t, p).Charge(context.Background(), mit)
		require.Error(t, err, body)
		require.False(t, res.Declined, body)
		require.Equal(t, 1, p.calls, body)
		require.NotContains(t, err.Error(), "RAW_PROVIDER_SENTINEL")
		if !strings.Contains(body, "430") {
			require.True(t, nmi.RequiresVerification(err), body)
		}
	}

	invalid := mit
	invalid.Context = charge.RecurringMIT("")
	p = &fakeProxy{serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }}
	_, err = newProxyCharger(t, p).Charge(context.Background(), invalid)
	require.Error(t, err)
	require.Zero(t, p.calls, "an invalid MIT never reaches the proxy")

	p = &fakeProxy{serve: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"proxy_error":{"title":"Unauthorized","status":401}}`))
	}}
	_, err = newProxyCharger(t, p).Charge(context.Background(), mit)
	_, isProxyErr := basistheory.IsBTProxyError(err)
	require.True(t, isProxyErr, "pre-forward failure is a BT error, never a decline")
	require.False(t, basistheory.IsTransportAmbiguous(err))

	p = &fakeProxy{serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusRequestTimeout) }}
	_, err = newProxyCharger(t, p).Charge(context.Background(), mit)
	require.True(t, basistheory.IsTransportAmbiguous(err), "408 may have forwarded: verify, never decline")
}

// A network-token failure before the destination answered falls back to pan_proxy in the same attempt.
func TestNetworkTokenFallsBackToPAN(t *testing.T) {
	t.Parallel()
	p := &fakeProxy{}
	p.serve = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cryptogram") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"title":"suspended","status":400}`))
			return
		}
		_ = r.ParseForm()
		if strings.Contains(r.PostForm.Get("ccnumber"), "network_token:") {
			t.Error("fallback must use the PAN token expression")
		}
		gatewayReply("response=1&transactionid=606")(w, r)
	}
	req := charge.Request{Instrument: charge.Instrument{Rail: Rail}, AmountMinor: 199, Currency: "USD", OrderRef: "ord-1", Context: charge.InitialRecurring()}
	res, err := newProxyCharger(t, p).WithSource(Source{TokenID: tokenID, Via: ViaNetworkToken, NetworkTokenID: ntID}).Charge(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "606", res.TransactionID)
	require.Equal(t, charge.TokenTypePANViaProxy, res.TokenType)
	require.Equal(t, 2, p.calls, "cryptogram call, then one PAN proxy call")
	require.Nil(t, (*Charger)(nil).WithSource(Source{}))
}
