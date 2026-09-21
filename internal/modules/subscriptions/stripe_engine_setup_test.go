package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestStripeEngineSetupBoundReadAndSecretIsolation(t *testing.T) {
	s, payment := engineFixture()
	p := StripeEngineSetupParams{payment.MerchantID, payment.PSPID, payment.CustomerID, uuid.New(), "cus_fixture"}
	setup := map[string]any{"id": "seti_fixture", "status": "requires_payment_method", "customer": "cus_fixture", "payment_method": "pm_fixture", "usage": "off_session", "livemode": false, "payment_method_types": []string{"card"}, "metadata": p.metadata(), "client_secret": "seti_fixture_secret_private"}
	count := 0
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer sk_test_fixture", r.Header.Get("Authorization"))
		if r.Method == "POST" {
			count++
			require.Equal(t, "/v1/setup_intents", r.URL.Path)
			b, _ := io.ReadAll(r.Body)
			v, _ := url.ParseQuery(string(b))
			require.Equal(t, "off_session", v.Get("usage"))
			require.Equal(t, "card", v.Get("payment_method_types[]"))
			require.NotContains(t, string(b), "amount")
			return engineResponse(200, setup), nil
		}
		if r.URL.Path == "/v1/setup_intents" {
			require.Equal(t, "cus_fixture", r.URL.Query().Get("customer"))
			return engineResponse(200, map[string]any{"data": []any{setup}, "has_more": false}), nil
		}
		if r.URL.Path == "/v1/payment_methods/pm_fixture" {
			return engineResponse(200, map[string]any{"id": "pm_fixture", "customer": "cus_fixture", "type": "card", "livemode": false, "card": map[string]any{"last4": "4242", "brand": "visa", "exp_month": 1, "exp_year": 2030}}), nil
		}
		require.Equal(t, "/v1/setup_intents/seti_fixture", r.URL.Path)
		return engineResponse(200, setup), nil
	})
	action, err := s.CreateEngineSetup(context.Background(), p)
	require.NoError(t, err)
	require.NotEmpty(t, action.ClientSecret)
	raw, _ := json.Marshal(action)
	require.NotContains(t, string(raw), "secret")
	setup["status"] = "succeeded"
	saved, found, err := s.ReadEngineSetup(context.Background(), p, "")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "pm_fixture", saved.MethodRef)
	require.Empty(t, saved.ClientSecret)
	require.Equal(t, 1, count)
	setup["customer"] = "cus_other"
	_, _, err = s.ReadEngineSetup(context.Background(), p, "seti_fixture")
	require.Error(t, err)
}
func TestStripeEngineDeclineCancelsOriginalBeforeTerminal(t *testing.T) {
	s, p := engineFixture()
	pi := enginePI(p)
	pi["status"] = "requires_payment_method"
	pi["last_payment_error"] = map[string]any{"code": "card_declined"}
	cancels := 0
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			require.Equal(t, "/v1/payment_intents/pi_fixture/cancel", r.URL.Path)
			require.Equal(t, "engine:"+p.OperationID.String()+":cancel", r.Header.Get("Idempotency-Key"))
			cancels++
			pi["status"] = "canceled"
			return nil, errors.New("lost cancel response")
		}
		return engineResponse(200, pi), nil
	})
	_, err := s.FinalizeEngineDecline(context.Background(), p, "pi_fixture")
	require.Error(t, err)
	result, err := s.FinalizeEngineDecline(context.Background(), p, "pi_fixture")
	require.NoError(t, err)
	require.Equal(t, "canceled", result.FailureCode)
	require.Equal(t, 1, cancels)
	pi["status"] = "requires_action"
	_, err = s.FinalizeEngineDecline(context.Background(), p, "pi_fixture")
	require.Error(t, err)
	require.Equal(t, 1, cancels)
}
