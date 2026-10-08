package subscriptions

import (
	"errors"
	"net/http"
	"testing"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/stretchr/testify/require"
)

func TestTokenCurrencyNeverReachesStripeCards(t *testing.T) {
	svc, p := engineFixture()
	calls := 0
	svc.StripeClients = stripeapi.NewFactory(engineWire(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected provider call")
	}))
	for _, currency := range []string{"SOL", "USDC"} {
		p.Currency = currency
		_, err := svc.CreateEnginePayment(t.Context(), p)
		require.ErrorContains(t, err, "not supported by card payment rails")
		_, err = svc.CollectInvoice(t.Context(), StripeInvoiceCollectionParams{CustomerID: "cus_1", PaymentMethodID: "pm_1", AmountCents: 1_000_000, Currency: currency, IdempotencyKey: "invoice"})
		require.ErrorContains(t, err, "not supported by card payment rails")
	}
	require.Zero(t, calls)
}
