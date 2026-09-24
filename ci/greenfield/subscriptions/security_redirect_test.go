//go:build greenfield && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// SEC-33: checkout return URLs are an open-redirect vector (a phishing page
// reached through the merchant's own checkout). Success and cancel URLs must
// name an exact allowed host origin, on the embedded and the remote Client.
func TestSecurityCheckoutReturnURLsStayOnHost(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	c := w.newCustomer()
	method := c.saveCard("stripe", visa)
	create := func(tp topology, success, cancel string) error {
		_, err := w.client[tp].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
			OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: "content:members", PriceID: price.ID,
			IdempotencyKey: "redirect-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["stripe"], Rail: "stripe", PaymentMethodID: method},
			SuccessURL: success, CancelURL: cancel,
		})
		return err
	}
	for _, tp := range []topology{embedded, remote} {
		for _, bad := range [][2]string{
			{"https://evil.test/return", "https://greenfield.test/return"},
			{"https://greenfield.test/return", "https://greenfield.test.evil.test/return"},
			{"https://evil.test/https://greenfield.test/return", "https://greenfield.test/return"},
			{"http://greenfield.test/return", "https://greenfield.test/return"},
			{"https://greenfield.test:8443/return", "https://greenfield.test/return"},
		} {
			require.Error(t, create(tp, bad[0], bad[1]), "%s %v", tp, bad)
		}
		require.NoError(t, create(tp, "https://greenfield.test/return", "https://greenfield.test/return?canceled=1"), tp)
	}
}
