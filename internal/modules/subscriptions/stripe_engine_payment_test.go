package subscriptions

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/stripemock"
)

func enginePayment(merchant, psp uuid.UUID) StripeEnginePaymentParams {
	return StripeEnginePaymentParams{OneTime: true, MerchantID: merchant, PSPID: psp, CustomerID: uuid.New(), OperationID: uuid.New(),
		Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: models.CustodianPSP, RailCustomerRef: "cus_engine", RailMethodRef: "pm_engine"}, AmountMinor: 1000, Currency: "USD"}
}

// A payment the posture gate refuses sent nothing, so it is not dispatched:
// receipt recovery has no PaymentIntent to look for.
func TestADisarmedEnginePaymentIsNotDispatched(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	merchant, psp := uuid.New(), uuid.New()
	key := "sk_test_acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	service := NewAccountStripeService(&config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, merchant, psp, "acct_engine", key)
	service.StripeClients = stripeapi.NewWiredFactory(stripe)
	_, err := service.CreateEnginePayment(t.Context(), enginePayment(merchant, psp))
	require.ErrorIs(t, err, charge.ErrNotDispatched)
	require.Empty(t, stripe.Submitted("/v1/payment_intents"))
}

// A payment Stripe refuses with 401, its key revoked, did nothing: it is not
// dispatched, and parks until the key is fixed rather than going to receipt
// recovery.
func TestAnEnginePaymentWithARevokedKeyIsNotDispatched(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	merchant, psp := uuid.New(), uuid.New()
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "sk_test_acct_" + name
	service := NewAccountStripeService(&config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, merchant, psp, "acct_"+name, key)
	service.StripeClients = stripeapi.NewWiredFactory(stripe)
	service.StripeClients.RegisterPosture("engine/"+name, merchant, psp, "acct_"+name, key)
	stripe.Intercept(func(r *http.Request) bool { return r.URL.Path == "/v1/payment_intents" }, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Invalid API Key provided"}}`))}, nil
	})
	_, err := service.CreateEnginePayment(t.Context(), enginePayment(merchant, psp))
	require.ErrorIs(t, err, charge.ErrNotDispatched)
	_, err = service.CreateEnginePayment(t.Context(), enginePayment(merchant, psp))
	require.ErrorIs(t, err, charge.ErrNotDispatched)
	require.Len(t, stripe.Submitted("/v1/payment_intents"), 1, "the revoked key's next payment is not sent")
}
