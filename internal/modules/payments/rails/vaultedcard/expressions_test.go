package vaultedcard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const testTokenID = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
const testIntentID = "9b2b0d6c-0000-4000-8000-000000000001"
const testNTID = "7c1f3a10-0000-4000-8000-000000000002"

var testGW = GatewayConfig{SecurityKey: "sk_test_123"}

// TestSaleFormWirePins pins the EXACT urlencoded wire body per the money-wall
// rule: known micros => Cents 199 => amount=1.99; expressions byte-exact;
// MIT/CIT stored-credential field combinations pinned.
func TestSaleFormWirePins(t *testing.T) {
	baseReq := func(ctx charge.Context) charge.Request {
		return charge.Request{
			// 1_990_000 micros = 199 cents (converted upstream of the seam).
			AmountMinor: moneyutil.Cents(199),
			Currency:    "usd",
			Description: "Renewal",
			OrderRef:    "order-1",
			Context:     ctx,
		}
	}

	t.Run("MIT recurring on stored token: exact wire body", func(t *testing.T) {
		form, err := SaleForm(baseReq(charge.RecurringMIT("9001")), Source{TokenID: testTokenID}, testGW, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := url.Values{
			"type":                        {"sale"},
			"security_key":                {"sk_test_123"},
			"amount":                      {"1.99"},
			"currency":                    {"usd"},
			"order_description":           {"Renewal"},
			"orderid":                     {"order-1"},
			"ccnumber":                    {`{{ token: 3fa85f64-5717-4562-b3fc-2c963f66afa6 | json: "$.data.number" }}`},
			"ccexp":                       {`{{ token: 3fa85f64-5717-4562-b3fc-2c963f66afa6 | json: "$.data" | card_exp: "MMYY" }}`},
			"initiated_by":                {"merchant"},
			"stored_credential_indicator": {"used"},
			"initial_transaction_id":      {"9001"},
			"billing_method":              {"recurring"},
		}
		if got := form.Encode(); got != want.Encode() {
			t.Fatalf("wire body mismatch:\n got %s\nwant %s", got, want.Encode())
		}
		if form.Has("cvv") {
			t.Fatal("MIT must be CVC-less")
		}
	})

	t.Run("CIT initial recurring from intent: indicator=stored + cvv from intent", func(t *testing.T) {
		form, err := SaleForm(baseReq(charge.InitialRecurring()), Source{TokenIntentID: testIntentID}, testGW, nil)
		if err != nil {
			t.Fatal(err)
		}
		pins := map[string]string{
			"ccnumber":                    `{{ token_intent: 9b2b0d6c-0000-4000-8000-000000000001 | json: "$.data.number" }}`,
			"ccexp":                       `{{ token_intent: 9b2b0d6c-0000-4000-8000-000000000001 | json: "$.data" | card_exp: "MMYY" }}`,
			"cvv":                         `{{ token_intent: 9b2b0d6c-0000-4000-8000-000000000001 | json: "$.data.cvc" }}`,
			"initiated_by":                "customer",
			"stored_credential_indicator": "stored",
			"billing_method":              "recurring",
			"amount":                      "1.99",
		}
		for k, v := range pins {
			if got := form.Get(k); got != v {
				t.Fatalf("%s:\n got %q\nwant %q", k, got, v)
			}
		}
		if form.Has("initial_transaction_id") {
			t.Fatal("initial CIT must not carry initial_transaction_id")
		}
	})

	t.Run("CIT unscheduled initial from intent: no billing_method", func(t *testing.T) {
		form, err := SaleForm(baseReq(charge.InitialOneTime()), Source{TokenIntentID: testIntentID}, testGW, nil)
		if err != nil {
			t.Fatal(err)
		}
		if form.Has("billing_method") {
			t.Fatal("unscheduled CoF sends no billing_method")
		}
		if form.Get("stored_credential_indicator") != "stored" || form.Get("initiated_by") != "customer" {
			t.Fatalf("unexpected stored-credential fields: %v", form)
		}
	})

	t.Run("unscheduled MIT on stored token", func(t *testing.T) {
		form, err := SaleForm(baseReq(charge.UnscheduledMIT("7777")), Source{TokenID: testTokenID}, testGW, nil)
		if err != nil {
			t.Fatal(err)
		}
		if form.Get("initiated_by") != "merchant" || form.Get("stored_credential_indicator") != "used" ||
			form.Get("initial_transaction_id") != "7777" || form.Has("billing_method") {
			t.Fatalf("unexpected fields: %v", form)
		}
	})

	t.Run("network token CIT: NT expressions + cavv/eci", func(t *testing.T) {
		form, err := SaleForm(baseReq(charge.InitialRecurring()),
			Source{TokenID: testTokenID, Via: ViaNetworkToken, NetworkTokenID: testNTID}, testGW,
			&basistheory.Cryptogram{Cryptogram: "2z8pd6WGPUi/BBesvjJcyw==", ECI: "07"})
		if err != nil {
			t.Fatal(err)
		}
		if got := form.Get("ccnumber"); got != `{{ network_token: 7c1f3a10-0000-4000-8000-000000000002 | json: "$.data.number" }}` {
			t.Fatalf("ccnumber: %q", got)
		}
		if got := form.Get("ccexp"); got != `{{ network_token: 7c1f3a10-0000-4000-8000-000000000002 | json: "$.data" | card_exp: "MMYY" }}` {
			t.Fatalf("ccexp: %q", got)
		}
		if form.Get("cavv") != "2z8pd6WGPUi/BBesvjJcyw==" || form.Get("eci") != "07" {
			t.Fatalf("cryptogram fields: %v", form)
		}
	})

	t.Run("network token MIT: no cryptogram, rides initial_transaction_id", func(t *testing.T) {
		form, err := SaleForm(baseReq(charge.RecurringMIT("9001")),
			Source{TokenID: testTokenID, Via: ViaNetworkToken, NetworkTokenID: testNTID}, testGW, nil)
		if err != nil {
			t.Fatal(err)
		}
		if form.Has("cavv") || form.Has("eci") {
			t.Fatal("NT MIT must not send cryptogram fields")
		}
		if form.Get("initial_transaction_id") != "9001" {
			t.Fatalf("NT MIT must ride the stored-credential anchor: %v", form)
		}
	})

	t.Run("micros boundary pins", func(t *testing.T) {
		for cents, want := range map[moneyutil.Cents]string{
			199:    "1.99",
			100:    "1.00",
			1:      "0.01",
			123456: "1234.56",
		} {
			form, err := SaleForm(charge.Request{AmountMinor: cents, Currency: "usd", Context: charge.InitialOneTime()},
				Source{TokenID: testTokenID}, testGW, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := form.Get("amount"); got != want {
				t.Fatalf("cents %d: got %q want %q", cents, got, want)
			}
		}
	})

	t.Run("loud errors: no silent defaults", func(t *testing.T) {
		if _, err := SaleForm(charge.Request{AmountMinor: 199, Context: charge.InitialOneTime()}, Source{TokenID: testTokenID}, testGW, nil); err == nil {
			t.Fatal("missing currency must error")
		}
		if _, err := SaleForm(charge.Request{AmountMinor: 0, Currency: "usd", Context: charge.InitialOneTime()}, Source{TokenID: testTokenID}, testGW, nil); err == nil {
			t.Fatal("zero amount must error")
		}
		if _, err := SaleForm(charge.Request{AmountMinor: 199, Currency: "usd", Context: charge.InitialOneTime()}, Source{}, testGW, nil); err == nil {
			t.Fatal("missing source must error")
		}
		if _, err := SaleForm(charge.Request{AmountMinor: 199, Currency: "usd", Context: charge.RecurringMIT("1")}, Source{TokenIntentID: testIntentID}, testGW, nil); err == nil {
			t.Fatal("MIT from a token intent must error")
		}
		if _, err := SaleForm(charge.Request{AmountMinor: 199, Currency: "usd", Context: charge.InitialRecurring()},
			Source{Via: ViaNetworkToken, NetworkTokenID: testNTID}, testGW, nil); err == nil {
			t.Fatal("NT CIT without cryptogram must error")
		}
		if _, err := SaleForm(charge.Request{AmountMinor: 199, Currency: "usd", OrderRef: strings.Repeat("x", 51), Context: charge.InitialOneTime()}, Source{TokenID: testTokenID}, testGW, nil); err == nil {
			t.Fatal("51-char order id must error, never truncate")
		}
	})
}

// TestChargerOutcomes drives the Charger against a fake BT proxy to pin the
// three-way outcome classification end to end.
func TestChargerOutcomes(t *testing.T) {
	newCharger := func(handler http.HandlerFunc) (*Charger, *httptest.Server) {
		srv := httptest.NewServer(handler)
		bt, err := basistheory.New(basistheory.Config{APIKey: "key_test", BaseURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		return New(bt, GatewayConfig{SecurityKey: "sk", DirectPostURL: "https://gw.example/transact.php"}), srv
	}
	req := charge.Request{
		Instrument:  charge.Instrument{Rail: Rail, MethodRef: testTokenID},
		AmountMinor: 199, Currency: "usd", OrderRef: "ord-1",
		Context: charge.RecurringMIT("9001"),
	}

	t.Run("approval", func(t *testing.T) {
		c, srv := newCharger(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(basistheory.ProxyDestinationStatusHeader, "200")
			_, _ = w.Write([]byte("response=1&responsetext=SUCCESS&authcode=123&transactionid=424242&response_code=100"))
		})
		defer srv.Close()
		res, err := c.Charge(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.TransactionID != "424242" || res.Declined {
			t.Fatalf("unexpected result: %+v", res)
		}
		if res.TokenType != charge.TokenTypePANViaVault {
			t.Fatalf("token type: %q", res.TokenType)
		}
		if res.CapturedRef != "" {
			t.Fatal("charge with a prior ref must not re-anchor")
		}
	})

	t.Run("reference-less charge anchors the sequence", func(t *testing.T) {
		c, srv := newCharger(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(basistheory.ProxyDestinationStatusHeader, "200")
			_, _ = w.Write([]byte("response=1&transactionid=555"))
		})
		defer srv.Close()
		anchorReq := req
		anchorReq.Context = charge.InitialRecurring()
		res, err := c.Charge(context.Background(), anchorReq)
		if err != nil {
			t.Fatal(err)
		}
		if res.CapturedRef != "555" {
			t.Fatalf("captured ref: %q", res.CapturedRef)
		}
	})

	t.Run("hard decline: verbatim code, Result not error", func(t *testing.T) {
		c, srv := newCharger(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(basistheory.ProxyDestinationStatusHeader, "200")
			_, _ = w.Write([]byte("response=2&responsetext=DECLINE&transactionid=99&response_code=202"))
		})
		defer srv.Close()
		res, err := c.Charge(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Declined || res.FailureCode == nil || *res.FailureCode != "insufficient_funds" {
			t.Fatalf("unexpected decline result: %+v, code %v", res, res.FailureCode)
		}
	})

	t.Run("duplicate detection (430) is transient, not a decline", func(t *testing.T) {
		c, srv := newCharger(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(basistheory.ProxyDestinationStatusHeader, "200")
			_, _ = w.Write([]byte("response=3&responsetext=Duplicate&response_code=430"))
		})
		defer srv.Close()
		if _, err := c.Charge(context.Background(), req); err == nil {
			t.Fatal("430 must surface as an error for retry machinery")
		}
	})

	t.Run("BT pre-forward failure is an error, never a decline", func(t *testing.T) {
		c, srv := newCharger(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"proxy_error":{"title":"Unauthorized","status":401}}`))
		})
		defer srv.Close()
		_, err := c.Charge(context.Background(), req)
		if _, ok := basistheory.IsBTProxyError(err); !ok {
			t.Fatalf("want BT proxy error, got %v", err)
		}
		if basistheory.IsTransportAmbiguous(err) {
			t.Fatal("pre-forward failure must not be ambiguous")
		}
	})

	t.Run("408 is ambiguous: verify-not-decline", func(t *testing.T) {
		c, srv := newCharger(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusRequestTimeout)
		})
		defer srv.Close()
		_, err := c.Charge(context.Background(), req)
		if !basistheory.IsTransportAmbiguous(err) {
			t.Fatalf("want ambiguous, got %v", err)
		}
	})

	t.Run("NT pre-forward failure falls back to pan_proxy in the same attempt", func(t *testing.T) {
		var calls int
		c, srv := newCharger(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if strings.HasSuffix(r.URL.Path, "/cryptogram") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"title":"suspended","status":400}`))
				return
			}
			_ = r.ParseForm()
			if strings.Contains(r.PostForm.Get("ccnumber"), "network_token:") {
				t.Fatal("fallback attempt must use the PAN token expression")
			}
			w.Header().Set(basistheory.ProxyDestinationStatusHeader, "200")
			_, _ = w.Write([]byte("response=1&transactionid=606"))
		})
		defer srv.Close()
		ntReq := req
		ntReq.Context = charge.InitialRecurring()
		res, err := c.WithSource(Source{TokenID: testTokenID, Via: ViaNetworkToken, NetworkTokenID: testNTID}).Charge(context.Background(), ntReq)
		if err != nil {
			t.Fatal(err)
		}
		if res.TransactionID != "606" || res.TokenType != charge.TokenTypePANViaVault {
			t.Fatalf("fallback result: %+v", res)
		}
		if calls != 2 {
			t.Fatalf("expected cryptogram + fallback proxy calls, got %d", calls)
		}
	})
}
