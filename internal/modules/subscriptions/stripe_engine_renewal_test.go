package subscriptions

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

func renewalFixture() (*StripeService, StripeEnginePaymentParams) {
	s, p := engineFixture()
	p.Initial = false
	p.Instrument.StoredCredentialRecurringRef = "pi_original"
	p.Renewal = &StripeRenewal{Obligation: uuid.NewString(), Attempt: 1, TermsSHA256: strings.Repeat("a", 64)}
	p.OperationID = renewalOperationID(p.MerchantID, p.PSPID, p.Renewal.Obligation, p.Renewal.Attempt)
	return s, p
}

func TestStripeRenewalReadsAllAttemptsBeforeCreating(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		attempt     int
		mutate      func(map[string]any)
		wantFound   bool
		wantError   bool
	}{
		{"same attempt paid", "succeeded", 1, nil, true, false},
		{"same attempt authenticating", "requires_action", 1, nil, true, false},
		{"earlier canceled", "canceled", 0, nil, false, false},
		{"earlier paid", "succeeded", 0, nil, false, true},
		{"earlier still executable", "requires_payment_method", 0, nil, false, true},
		{"later canceled", "canceled", 2, nil, false, true},
		{"same attempt different period", "succeeded", 1, func(pi map[string]any) {
			pi["metadata"].(map[string]string)["openrails_renewal_terms"] = strings.Repeat("b", 64)
		}, false, true},
		{"same attempt customer vs worker", "succeeded", 1, func(pi map[string]any) { pi["metadata"].(map[string]string)["openrails_customer_retry"] = "true" }, false, true},
		{"foreign account", "canceled", 0, func(pi map[string]any) { pi["metadata"].(map[string]string)["openrails_psp"] = uuid.NewString() }, false, true},
		{"wrong environment", "canceled", 0, func(pi map[string]any) { pi["livemode"] = true }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := renewalFixture()
			other := p
			renewal := *p.Renewal
			renewal.Attempt = tc.attempt
			other.Renewal = &renewal
			other.OperationID = renewalOperationID(p.MerchantID, p.PSPID, renewal.Obligation, tc.attempt)
			pi := enginePI(other)
			pi["status"] = tc.state
			if tc.state != "succeeded" {
				pi["amount_received"] = 0
			}
			if tc.mutate != nil {
				tc.mutate(pi)
			}
			calls := 0
			s.StripeClients = stripeapi.NewFactory(engineWire(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, http.MethodGet, r.Method, "preflight never mutates another attempt")
				switch r.URL.Path {
				case "/v1/payment_intents":
					require.Equal(t, p.Instrument.RailCustomerRef, r.URL.Query().Get("customer"))
					return engineResponse(200, map[string]any{"data": []any{pi}, "has_more": false}), nil
				case "/v1/payment_intents/pi_fixture":
					return engineResponse(200, pi), nil
				case "/v1/charges/ch_fixture":
					return engineResponse(200, engineCharge(p)), nil
				default:
					t.Fatalf("unexpected request %s", r.URL.Path)
					return nil, nil
				}
			}))
			result, found, err := s.ReadEngineRenewal(context.Background(), p)
			require.Equal(t, tc.wantError, err != nil, "%v", err)
			require.Equal(t, tc.wantFound, found)
			if tc.wantFound {
				require.Greater(t, calls, 1, "the list entry must be read by exact ID")
				if result.Receipt != nil {
					require.NoError(t, result.Receipt.Matches(p))
					p.Renewal.TermsSHA256 = strings.Repeat("b", 64)
					require.Error(t, result.Receipt.Matches(p), "retained receipt binds coverage")
				}
			}
		})
	}
}

func TestStripeRenewalRefusesIncompleteProviderLookup(t *testing.T) {
	for _, tc := range []struct {
		name string
		page map[string]any
		code int
	}{
		{"provider down", map[string]any{}, 503},
		{"null response", nil, 200},
		{"missing data", map[string]any{"has_more": false}, 200},
		{"missing pagination flag", map[string]any{"data": []any{}}, 200},
		{"null data", map[string]any{"data": nil, "has_more": false}, 200},
		{"record without identity", map[string]any{"data": []any{nil}, "has_more": false}, 200},
		{"empty incomplete page", map[string]any{"data": []any{}, "has_more": true}, 200},
		{"repeated page", map[string]any{"data": []any{map[string]any{"id": "pi_unrelated"}}, "has_more": true}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := renewalFixture()
			calls := 0
			s.StripeClients = stripeapi.NewFactory(engineWire(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, http.MethodGet, r.Method)
				return engineResponse(tc.code, tc.page), nil
			}))
			_, found, err := s.ReadEngineRenewal(context.Background(), p)
			require.Error(t, err)
			require.False(t, found)
			require.LessOrEqual(t, calls, 2, "incomplete pagination must not loop forever")
		})
	}
}

func TestStripeRenewalKeepsListedPaymentWhenDetailDisappears(t *testing.T) {
	s, p := renewalFixture()
	s.StripeClients = stripeapi.NewFactory(engineWire(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, r.Method)
		if r.URL.Path == "/v1/payment_intents" {
			return engineResponse(200, map[string]any{"data": []any{enginePI(p)}, "has_more": false}), nil
		}
		require.Equal(t, "/v1/payment_intents/pi_fixture", r.URL.Path)
		return engineResponse(404, map[string]any{}), nil
	}))
	result, found, err := s.ReadEngineRenewal(context.Background(), p)
	require.ErrorContains(t, err, "exact readback")
	require.False(t, found)
	require.Equal(t, "pi_fixture", result.PaymentIntentID, "retain the observed identity instead of treating a detail 404 as an empty history")
}
