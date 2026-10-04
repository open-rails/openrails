package handlers

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

const cardBody = `"card":{"number":"4111 1111 1111 1111","exp_month":10,"exp_year":2027,"cvc":"999"}`

// #1129: the typed card field is refused beside a token, with a card number in
// any other field, and in live posture over plain HTTP.
func TestCardFieldRequestRules(t *testing.T) {
	live := &app.Runtime{Config: &config.Config{TestMode: config.CredentialPostureLive}, TrustedProxies: iputil.ParseTrustedProxies([]string{"10.0.0.0/8"})}
	sandbox := &app.Runtime{Config: &config.Config{TestMode: config.CredentialPostureSandbox}}
	for name, tc := range map[string]struct {
		rt       *app.Runtime
		wire     func(*http.Request)
		hasToken bool
		others   []string
		code     string
	}{
		"sandbox over plain http":             {rt: sandbox},
		"live over tls":                       {rt: live, wire: func(r *http.Request) { r.TLS = &tls.ConnectionState{} }},
		"live behind a trusted https proxy":   {rt: live, wire: func(r *http.Request) { r.RemoteAddr = "10.0.0.5:1234"; r.Header.Set("X-Forwarded-Proto", "https") }},
		"live over plain http":                {rt: live, code: "card_requires_https"},
		"live, header from an untrusted peer": {rt: live, wire: func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }, code: "card_requires_https"},
		"live behind a trusted http proxy":    {rt: live, wire: func(r *http.Request) { r.RemoteAddr = "10.0.0.5:1234"; r.Header.Set("X-Forwarded-Proto", "http") }, code: "card_requires_https"},
		"no posture is live":                  {rt: &app.Runtime{}, code: "card_requires_https"},
		"beside a token":                      {rt: sandbox, hasToken: true, code: "not both"},
		"a number in another field":           {rt: sandbox, others: []string{"Card Holder", "4111-1111-1111-1111"}, code: "only in the card field"},
	} {
		wire := httptest.NewRequest(http.MethodPost, "http://billing.test/v1/me/payment-methods", nil)
		if tc.wire != nil {
			tc.wire(wire)
		}
		rec := httptest.NewRecorder()
		admitted := cardFieldAdmitted(httprequest.NewHTTP(rec, wire, tc.rt), tc.hasToken, tc.others...)
		require.Equal(t, tc.code == "", admitted, name)
		if tc.code != "" {
			require.Equal(t, http.StatusBadRequest, rec.Code, name)
			require.Contains(t, rec.Body.String(), tc.code, name)
			require.NotContains(t, rec.Body.String(), "4111", name)
		}
	}
}

// A card decodes into the request and never back out of it; with neither a
// token nor a card the request is refused exactly as a tokenless one was.
func TestPaymentMethodBodiesTakeACard(t *testing.T) {
	bind := func(body string, into any) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		wire := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		httprequest.NewHTTP(rec, wire, &app.Runtime{}).BindJSON(into)
		return rec
	}
	var create billing.CreatePaymentMethodParams
	require.Empty(t, bind(`{"psp_id":"`+billing.PSPID(uuid.New()).String()+`",`+cardBody+`}`, &create).Body.String())
	require.Equal(t, []string{"visa", "1111", "10/27"}, []string{create.Card.Brand(), create.Card.LastFour(), create.Card.Expiry()})
	var update billing.ReplacePaymentMethodCardParams
	require.Empty(t, bind(`{`+cardBody+`}`, &update).Body.String())
	require.NotNil(t, update.Card)
	var pay checkoutsession.CheckoutSessionPayRequest
	require.Empty(t, bind(`{"option_id":"option_card",`+cardBody+`}`, &pay).Body.String())
	require.NotNil(t, pay.Card)

	// A card number in a field of its own is an unknown field, never echoed.
	rec := bind(`{"psp_id":"`+billing.PSPID(uuid.New()).String()+`","card_number":"4111111111111111"}`, new(billing.CreatePaymentMethodParams))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "unknown_field")
	require.NotContains(t, rec.Body.String(), "4111")
	for body, message := range map[string]string{
		`{"card":{"number":"4111111111111112","exp_month":10,"exp_year":2027,"cvc":"999"}}`: "card number is invalid",
		`{"card":{"number":"4111111111111111","exp_month":10,"exp_year":2027}}`:             "card cvc is invalid",
		`{"card":"[card]"}`: "card was redacted in transit",
	} {
		rec := bind(body, new(billing.CreatePaymentMethodParams))
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
		require.Contains(t, rec.Body.String(), message, body)
		require.NotContains(t, rec.Body.String(), "4111", body)
	}
}
