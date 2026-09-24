package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// The engine talks to Stripe through the stripeapi factory; the fake sits at
// the RoundTripper so the real guard (read-only mode, version header) runs.
type engineWire func(*http.Request) (*http.Response, error)

func (f engineWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func engineResponse(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(b))), Header: make(http.Header)}
}

func engineFixture() (*StripeService, StripeEnginePaymentParams) {
	p := StripeEnginePaymentParams{MerchantID: uuid.New(), PSPID: uuid.New(), CustomerID: uuid.New(), OperationID: uuid.New(), AmountMinor: 1299, Currency: "USD", Initial: true}
	p.Instrument = charge.FrozenInstrument{PSPID: p.PSPID, Custodian: models.CustodianPSP, RailCustomerRef: "cus_fixture", RailMethodRef: "pm_fixture"}
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox}
	return NewAccountStripeService(cfg, p.MerchantID, p.PSPID, "acct_fixture", "sk_test_fixture"), p
}

func enginePI(p StripeEnginePaymentParams) map[string]any {
	return map[string]any{"object": "payment_intent", "id": "pi_fixture", "status": "succeeded", "customer": p.Instrument.RailCustomerRef, "payment_method": p.Instrument.RailMethodRef, "amount": 1299, "amount_received": 1299, "currency": "usd", "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "latest_charge": "ch_fixture", "metadata": p.metadata(), "livemode": false}
}

func engineCharge(p StripeEnginePaymentParams) map[string]any {
	return map[string]any{"id": "ch_fixture", "amount": 1299, "amount_captured": 1299, "currency": "usd", "customer": p.Instrument.RailCustomerRef, "payment_method": p.Instrument.RailMethodRef, "payment_intent": "pi_fixture", "status": "succeeded", "paid": true, "captured": true}
}

// serveEngine answers GETs with pi/ch; post, when non-nil, handles writes.
func serveEngine(t *testing.T, s *StripeService, pi, ch map[string]any, post func(*http.Request, url.Values) (*http.Response, error)) {
	t.Helper()
	s.StripeClients = stripeapi.NewFactory(engineWire(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer sk_test_fixture" || r.Header.Get(stripeapi.VersionHeader) != stripeapi.APIVersion {
			t.Errorf("request not scoped to the account or version-pinned: %s %s", r.Method, r.URL.Path)
		}
		if r.Method == http.MethodPost {
			if post == nil {
				t.Errorf("unexpected write %s", r.URL.Path)
				return nil, errors.New("unexpected write")
			}
			b, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(b))
			return post(r, form)
		}
		if r.URL.Path == "/v1/charges/ch_fixture" {
			return engineResponse(200, ch), nil
		}
		return engineResponse(200, pi), nil
	}))
}

// Every payment kind sends the frozen terms and its own initiation flags under
// the operation's idempotency key, never native Stripe billing; reads replay
// the same payment without moving money again.
func TestStripeEnginePaymentWire(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*StripeEnginePaymentParams)
		offSession string
		setup      bool
		extra      map[string]string
	}{
		{"initial enrollment", func(*StripeEnginePaymentParams) {}, "false", true, nil},
		{"merchant renewal", func(p *StripeEnginePaymentParams) {
			p.Initial, p.Instrument.StoredCredentialRecurringRef = false, "pi_initial"
		}, "true", false, map[string]string{"openrails_agreement": "pi_initial"}},
		{"renewal on replacement card", func(p *StripeEnginePaymentParams) {
			p.Initial, p.Instrument.StoredCredentialRecurringRef = false, "seti_replacement"
		}, "true", false, map[string]string{"openrails_agreement": "seti_replacement"}},
		{"customer retry", func(p *StripeEnginePaymentParams) {
			p.Initial, p.CustomerInitiated, p.Instrument.StoredCredentialRecurringRef = false, true, "pi_original"
		}, "false", false, map[string]string{"openrails_customer_retry": "true", "openrails_agreement": "pi_original"}},
		{"one-time purchase", func(p *StripeEnginePaymentParams) { p.Initial, p.OneTime = false, true }, "false", false, map[string]string{"openrails_one_time": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := engineFixture()
			tc.mutate(&p)
			pi := enginePI(p)
			if !tc.setup {
				delete(pi, "setup_future_usage")
			}
			posts := 0
			serveEngine(t, s, pi, engineCharge(p), func(r *http.Request, v url.Values) (*http.Response, error) {
				posts++
				require.Equal(t, "/v1/payment_intents", r.URL.Path)
				require.Equal(t, "engine:"+p.OperationID.String(), r.Header.Get(stripeapi.IdempotencyKeyHeader))
				for k, want := range map[string]string{"customer": "cus_fixture", "payment_method": "pm_fixture", "amount": "1299", "currency": "usd", "confirm": "true", "capture_method": "automatic", "payment_method_types[]": "card", "off_session": tc.offSession, "metadata[openrails_engine_operation]": p.OperationID.String(), "metadata[openrails_psp]": p.PSPID.String()} {
					require.Equal(t, want, v.Get(k), k)
				}
				require.Equal(t, tc.setup, v.Get("setup_future_usage") == "off_session")
				for k, want := range tc.extra {
					require.Equal(t, want, v.Get("metadata["+k+"]"), k)
				}
				require.False(t, v.Has("price") || v.Has("subscription"), "native billing enrollment")
				return engineResponse(200, pi), nil
			})
			result, err := s.CreateEnginePayment(context.Background(), p)
			require.NoError(t, err)
			require.Equal(t, StripeEngineSucceeded, result.State)
			require.NoError(t, result.Receipt.Matches(p))
			for range 2 {
				again, found, err := s.ReadEnginePayment(context.Background(), p, result.PaymentIntentID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, result.Receipt.ChargeID, again.Receipt.ChargeID)
			}
			require.Equal(t, 1, posts)

			// A receipt proves only the exact instruction it was taken under:
			// a customer retry is not a merchant renewal and vice versa.
			if !p.Initial && !p.OneTime {
				flipped := p
				flipped.CustomerInitiated = !p.CustomerInitiated
				require.Error(t, result.Receipt.Matches(flipped))
			}
		})
	}
}

// Anything that cannot be a valid, armed charge is refused as provably not
// dispatched, before any request leaves the process.
func TestStripeEngineRefusesBeforeWire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*StripeService, *StripeEnginePaymentParams)
	}{
		{"read-only provider mode", func(s *StripeService, _ *StripeEnginePaymentParams) {
			s.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
		}},
		{"operation for another psp", func(_ *StripeService, p *StripeEnginePaymentParams) {
			p.PSPID = uuid.New()
			p.Instrument.PSPID = p.PSPID
		}},
		{"instrument from another psp", func(_ *StripeService, p *StripeEnginePaymentParams) { p.Instrument.PSPID = uuid.New() }},
		{"initial customer retry", func(_ *StripeService, p *StripeEnginePaymentParams) { p.CustomerInitiated = true }},
		{"recurring one-time", func(_ *StripeService, p *StripeEnginePaymentParams) { p.OneTime = true }},
		{"renewal without agreement", func(_ *StripeService, p *StripeEnginePaymentParams) { p.Initial = false }},
		{"zero amount", func(_ *StripeService, p *StripeEnginePaymentParams) { p.AmountMinor = 0 }},
		{"amount over Stripe max", func(_ *StripeService, p *StripeEnginePaymentParams) { p.AmountMinor = 100_000_000 }},
		{"unknown currency", func(_ *StripeService, p *StripeEnginePaymentParams) { p.Currency = "XXQ" }},
		{"malformed method ref", func(_ *StripeService, p *StripeEnginePaymentParams) { p.Instrument.RailMethodRef = "pm_x/../y" }},
		{"third-party custodian", func(_ *StripeService, p *StripeEnginePaymentParams) { p.Instrument.Custodian = "hyperswitch" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := engineFixture()
			tc.mutate(s, &p)
			calls := 0
			s.StripeClients = stripeapi.NewFactory(engineWire(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected wire")
			}))
			_, err := s.CreateEnginePayment(context.Background(), p)
			require.ErrorIs(t, err, charge.ErrNotDispatched)
			require.Zero(t, calls)
		})
	}
}

// A read binds the provider object to every accepted term; any foreign or
// incomplete fact is an error, never a receipt, and reads never write.
func TestStripeEngineReadRejectsForeignEvidence(t *testing.T) {
	meta := func(k string) func(pi, ch map[string]any) {
		return func(pi, _ map[string]any) { pi["metadata"].(map[string]string)[k] = uuid.NewString() }
	}
	for _, tc := range []struct {
		name   string
		mutate func(pi, ch map[string]any)
	}{
		{"operation", meta("openrails_engine_operation")},
		{"merchant", meta("openrails_merchant")},
		{"psp", meta("openrails_psp")},
		{"customer id", meta("openrails_customer")},
		{"amount", func(pi, _ map[string]any) { pi["amount"] = 1300 }},
		{"received", func(pi, _ map[string]any) { pi["amount_received"] = 1 }},
		{"currency", func(pi, _ map[string]any) { pi["currency"] = "eur" }},
		{"customer", func(pi, _ map[string]any) { pi["customer"] = "cus_other" }},
		{"method", func(pi, _ map[string]any) { pi["payment_method"] = "pm_replacement" }},
		{"succeeded without method", func(pi, _ map[string]any) { pi["payment_method"] = nil }},
		{"setup usage", func(pi, _ map[string]any) { pi["setup_future_usage"] = "on_session" }},
		{"manual capture", func(pi, _ map[string]any) { pi["capture_method"] = "manual" }},
		{"live mode", func(pi, _ map[string]any) { pi["livemode"] = true }},
		{"missing environment", func(pi, _ map[string]any) { delete(pi, "livemode") }},
		{"no charge", func(pi, _ map[string]any) { delete(pi, "latest_charge") }},
		{"charge customer", func(_, ch map[string]any) { ch["customer"] = "cus_other" }},
		{"charge method", func(_, ch map[string]any) { ch["payment_method"] = "pm_other" }},
		{"charge pi", func(_, ch map[string]any) { ch["payment_intent"] = "pi_other" }},
		{"charge not captured", func(_, ch map[string]any) { ch["captured"] = false }},
		{"charge amount", func(_, ch map[string]any) { ch["amount_captured"] = 1 }},
		{"refunded flag without full amount", func(_, ch map[string]any) { ch["refunded"] = true }},
		{"over-refunded", func(_, ch map[string]any) { ch["amount_refunded"] = 1300 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := engineFixture()
			pi, ch := enginePI(p), engineCharge(p)
			tc.mutate(pi, ch)
			serveEngine(t, s, pi, ch, nil)
			_, _, err := s.ReadEnginePayment(context.Background(), p, "pi_fixture")
			require.Error(t, err)
		})
	}
}

// A lost create response is an unknown outcome, not a non-dispatch. Recovery
// pages the customer's PaymentIntents and must find exactly one claimant.
func TestStripeEngineLostResponseRecovery(t *testing.T) {
	s, p := engineFixture()
	lists := 0
	s.StripeClients = stripeapi.NewFactory(engineWire(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost:
			return nil, errors.New("timeout after provider accepted")
		case r.URL.Path == "/v1/payment_intents":
			lists++
			require.Equal(t, "cus_fixture", r.URL.Query().Get("customer"))
			if r.URL.Query().Get("starting_after") == "" {
				return engineResponse(200, map[string]any{"data": []any{map[string]any{"id": "pi_other", "metadata": map[string]string{}}}, "has_more": true}), nil
			}
			return engineResponse(200, map[string]any{"data": []any{enginePI(p)}, "has_more": false}), nil
		case r.URL.Path == "/v1/charges/ch_fixture":
			return engineResponse(200, engineCharge(p)), nil
		}
		return engineResponse(200, enginePI(p)), nil
	}))
	_, err := s.CreateEnginePayment(context.Background(), p)
	require.Error(t, err)
	require.NotErrorIs(t, err, charge.ErrNotDispatched)
	r, found, err := s.ReadEnginePayment(context.Background(), p, "")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, StripeEngineSucceeded, r.State)
	require.Equal(t, 2, lists, "recovery must page past unrelated payments")

	for _, tc := range []struct {
		name    string
		data    []any
		wantErr bool
	}{
		{"absent", []any{}, false},
		{"duplicate claimants", []any{enginePI(p), enginePI(p)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveEngine(t, s, map[string]any{"data": tc.data, "has_more": false}, nil, nil)
			_, found, err := s.ReadEnginePayment(context.Background(), p, "")
			require.False(t, found)
			require.Equal(t, tc.wantErr, err != nil)
		})
	}
}

// 3DS: the 402 body carries the same PI; the outcome never persists its
// client secret, and only the owning customer can fetch it for Stripe.js.
func TestStripeEngineAuthenticationSecretIsolation(t *testing.T) {
	s, p := engineFixture()
	pi := enginePI(p)
	pi["status"] = "requires_payment_method"
	pi["payment_method"] = nil
	pi["last_payment_error"] = map[string]any{"code": "authentication_required", "payment_method": map[string]any{"id": "pm_fixture"}}
	pi["client_secret"] = "pi_fixture_secret_sensitive"
	posts := 0
	serveEngine(t, s, pi, engineCharge(p), func(*http.Request, url.Values) (*http.Response, error) {
		posts++
		return engineResponse(http.StatusPaymentRequired, map[string]any{"error": map[string]any{"payment_intent": pi}}), nil
	})
	r, err := s.CreateEnginePayment(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, StripeEngineAuthenticationRequired, r.State)
	require.Equal(t, "pi_fixture", r.PaymentIntentID)
	encoded, _ := json.Marshal(r)
	require.NotContains(t, string(encoded), "secret")

	secret, err := s.EngineAuthenticationSecret(context.Background(), p, r.PaymentIntentID, p.CustomerID)
	require.NoError(t, err)
	require.Equal(t, "pi_fixture_secret_sensitive", secret)
	secret, err = s.EngineAuthenticationSecret(context.Background(), p, r.PaymentIntentID, uuid.New())
	require.Error(t, err)
	require.Empty(t, secret)
	_, err = s.EngineAuthenticationSecret(context.Background(), p, "", p.CustomerID)
	require.Error(t, err)
	require.Equal(t, 1, posts)

	pi["status"] = "succeeded"
	pi["payment_method"] = "pm_fixture"
	_, err = s.EngineAuthenticationSecret(context.Background(), p, r.PaymentIntentID, p.CustomerID)
	require.Error(t, err, "no secret once the payment is not awaiting authentication")
}

// A reversal after capture keeps the original receipt valid and surfaces the
// reversal kind; a partial refund is not a reversal of access.
func TestStripeEngineReversalRetainsCapture(t *testing.T) {
	for _, tc := range []struct {
		kind string
		ch   map[string]any
	}{
		{"refund", map[string]any{"refunded": true, "amount_refunded": 1299}},
		{"refund", map[string]any{"amount_refunded": 1299}},
		{"dispute", map[string]any{"disputed": true}},
		{"", map[string]any{"amount_refunded": 1}},
	} {
		s, p := engineFixture()
		ch := engineCharge(p)
		for k, v := range tc.ch {
			ch[k] = v
		}
		serveEngine(t, s, enginePI(p), ch, nil)
		r, found, err := s.ReadEnginePayment(context.Background(), p, "pi_fixture")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, tc.kind, r.Receipt.ReversalKind())
		require.NoError(t, r.Receipt.Matches(p))
	}
}

// A webhook is a wake-up signal only, bound to the same accepted terms and to
// the routed account's environment.
func TestStripeEngineNotificationBinding(t *testing.T) {
	_, params := engineFixture()
	for _, status := range []string{"succeeded", "requires_payment_method", "requires_action", "canceled"} {
		pi := enginePI(params)
		pi["status"] = status
		if status != "succeeded" {
			pi["payment_method"] = nil
		}
		raw, _ := json.Marshal(pi)
		require.NoError(t, ValidateStripeEnginePaymentNotification(raw, params, "test"), status)
	}
	for _, tc := range []struct {
		name   string
		env    string
		change func(map[string]any)
	}{
		{"object", "test", func(p map[string]any) { p["object"] = "charge" }},
		{"missing object", "test", func(p map[string]any) { delete(p, "object") }},
		{"customer", "test", func(p map[string]any) { p["customer"] = "cus_other" }},
		{"method", "test", func(p map[string]any) { p["payment_method"] = "pm_other" }},
		{"amount", "test", func(p map[string]any) { p["amount"] = 999 }},
		{"currency", "test", func(p map[string]any) { p["currency"] = "eur" }},
		{"live event on test account", "test", func(p map[string]any) { p["livemode"] = true }},
		{"test event on live account", "live", func(map[string]any) {}},
		{"unknown environment", "staging", func(map[string]any) {}},
		{"missing mode", "test", func(p map[string]any) { delete(p, "livemode") }},
		{"operation", "test", func(p map[string]any) {
			p["metadata"].(map[string]string)["openrails_engine_operation"] = uuid.NewString()
		}},
		{"account", "test", func(p map[string]any) { p["metadata"].(map[string]string)["openrails_psp"] = uuid.NewString() }},
	} {
		pi := enginePI(params)
		tc.change(pi)
		raw, _ := json.Marshal(pi)
		require.Error(t, ValidateStripeEnginePaymentNotification(raw, params, tc.env), tc.name)
	}
}

// A declined PI is cancelled (same PI, own idempotency key) before the decline
// is terminal, so a leaked client secret cannot pay later. A lost cancel
// response is recovered by readback; authentication states are never cancelled.
func TestStripeEngineDeclineFinalization(t *testing.T) {
	s, p := engineFixture()
	pi := enginePI(p)
	pi["status"] = "requires_payment_method"
	pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
	cancels := 0
	serveEngine(t, s, pi, nil, func(r *http.Request, _ url.Values) (*http.Response, error) {
		require.Equal(t, "/v1/payment_intents/pi_fixture/cancel", r.URL.Path)
		require.Equal(t, "engine:"+p.OperationID.String()+":cancel", r.Header.Get(stripeapi.IdempotencyKeyHeader))
		cancels++
		pi["status"] = "canceled"
		delete(pi, "last_payment_error")
		return nil, errors.New("lost cancel response")
	})
	_, err := s.FinalizeEngineDecline(context.Background(), p, "pi_fixture")
	require.Error(t, err)
	result, err := s.FinalizeEngineDecline(context.Background(), p, "pi_fixture")
	require.NoError(t, err)
	require.Equal(t, StripeEngineDeclined, result.State)
	require.Equal(t, "canceled", result.FailureCode)
	require.Equal(t, 1, cancels)

	// Cancellation that reads back cleanly keeps the issuer's decline reason.
	s, p = engineFixture()
	pi = enginePI(p)
	pi["status"] = "requires_payment_method"
	pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "stolen_card"}
	serveEngine(t, s, pi, nil, func(*http.Request, url.Values) (*http.Response, error) {
		pi["status"] = "canceled"
		delete(pi, "last_payment_error")
		return engineResponse(200, pi), nil
	})
	result, err = s.FinalizeEngineDecline(context.Background(), p, "pi_fixture")
	require.NoError(t, err)
	require.Equal(t, "stolen_card", result.DeclineCode)

	for _, status := range []string{"requires_action", "succeeded"} {
		s, p = engineFixture()
		pi = enginePI(p)
		pi["status"] = status
		serveEngine(t, s, pi, engineCharge(p), nil)
		_, err = s.FinalizeEngineDecline(context.Background(), p, "pi_fixture")
		require.Error(t, err, status)
	}
}

// Card setup moves no money, is bound to the checkout session and customer,
// and its client secret never serializes.
func TestStripeEngineSetupBinding(t *testing.T) {
	s, payment := engineFixture()
	p := StripeEngineSetupParams{MerchantID: payment.MerchantID, PSPID: payment.PSPID, CustomerID: payment.CustomerID, SessionID: uuid.New(), CustomerRef: "cus_fixture"}
	setup := map[string]any{"id": "seti_fixture", "status": "requires_payment_method", "customer": "cus_fixture", "payment_method": "pm_fixture", "usage": "off_session", "livemode": false, "payment_method_types": []string{"card"}, "metadata": p.metadata(), "client_secret": "seti_fixture_secret_private"}
	posts := 0
	s.StripeClients = stripeapi.NewFactory(engineWire(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer sk_test_fixture", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost:
			posts++
			require.Equal(t, "/v1/setup_intents", r.URL.Path)
			require.Equal(t, "engine-setup:"+p.SessionID.String(), r.Header.Get(stripeapi.IdempotencyKeyHeader))
			b, _ := io.ReadAll(r.Body)
			v, _ := url.ParseQuery(string(b))
			require.Equal(t, "off_session", v.Get("usage"))
			require.Equal(t, "card", v.Get("payment_method_types[]"))
			require.NotContains(t, string(b), "amount")
			return engineResponse(200, setup), nil
		case r.URL.Path == "/v1/setup_intents":
			require.Equal(t, "cus_fixture", r.URL.Query().Get("customer"))
			return engineResponse(200, map[string]any{"data": []any{setup}, "has_more": false}), nil
		case r.URL.Path == "/v1/payment_methods/pm_fixture":
			return engineResponse(200, map[string]any{"id": "pm_fixture", "customer": "cus_fixture", "type": "card", "livemode": false, "card": map[string]any{"last4": "4242", "brand": "visa", "exp_month": 1, "exp_year": 2030}}), nil
		}
		require.Equal(t, "/v1/setup_intents/seti_fixture", r.URL.Path)
		return engineResponse(200, setup), nil
	}))
	action, err := s.CreateEngineSetup(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, "seti_fixture_secret_private", action.ClientSecret)
	raw, _ := json.Marshal(action)
	require.NotContains(t, string(raw), "secret")

	setup["status"] = "succeeded"
	saved, found, err := s.ReadEngineSetup(context.Background(), p, "")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "pm_fixture", saved.MethodRef)
	require.Equal(t, "4242", saved.LastFour)
	require.Empty(t, saved.ClientSecret)
	require.Equal(t, 1, posts)

	for k, v := range map[string]any{"customer": "cus_other", "usage": "on_session", "livemode": true} {
		orig := setup[k]
		setup[k] = v
		_, _, err = s.ReadEngineSetup(context.Background(), p, "seti_fixture")
		require.Error(t, err, k)
		setup[k] = orig
	}
	other := p
	other.SessionID = uuid.New()
	_, _, err = s.ReadEngineSetup(context.Background(), other, "seti_fixture")
	require.Error(t, err, "setup belongs to another session")
}
