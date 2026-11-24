package subscriptions

import (
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

// A payment the posture gate refuses sent nothing, so it is not dispatched:
// receipt recovery has no PaymentIntent to look for.
func TestADisarmedEnginePaymentIsNotDispatched(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	merchant, psp := uuid.New(), uuid.New()
	key := "sk_test_acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	service := NewAccountStripeService(&config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, merchant, psp, "acct_engine", key)
	service.StripeClients = stripeapi.NewWiredFactory(stripe)
	_, err := service.CreateEnginePayment(t.Context(), StripeEnginePaymentParams{OneTime: true, MerchantID: merchant, PSPID: psp, CustomerID: uuid.New(), OperationID: uuid.New(),
		Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: models.CustodianPSP, RailCustomerRef: "cus_engine", RailMethodRef: "pm_engine"}, AmountMinor: 1000, Currency: "USD"})
	require.ErrorIs(t, err, charge.ErrNotDispatched)
	require.Empty(t, stripe.Submitted("/v1/payment_intents"))
}
