//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// collects maps each of the customer's cards to the currencies it collects,
// as the Client lists them.
func (c *customer) collects(tp topology) map[string][]string {
	c.w.t.Helper()
	page, err := c.w.client[tp].ListPaymentMethods(c.w.t.Context(), c.cid(), billing.PageRequest{Limit: 100})
	require.NoError(c.w.t, err)
	out := map[string][]string{}
	for _, m := range page.Items {
		out[m.ID.String()] = m.CollectionCurrencies
	}
	return out
}

// A customer names, per currency, the card that collects their invoices;
// there is no default card, and naming another card moves it. Stripe cards:
// an NMI card collects only once it carries an unscheduled stored-credential
// agreement.
func TestCollectionPaymentMethod(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			first := c.saveCard("stripe", visa)
			second := c.saveCard("stripe", mastercard)
			require.Equal(t, map[string][]string{first: {}, second: {}}, c.collects(tp), "a saved card collects nothing until named")

			set := c.must(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": second, "currency": "USD"})
			require.Equal(t, map[string]any{"currency": "USD", "payment_method_id": second}, set)
			require.Equal(t, map[string][]string{first: {}, second: {"USD"}}, c.collects(tp))

			c.must(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": first, "currency": "USD"})
			require.Equal(t, map[string][]string{first: {"USD"}, second: {}}, c.collects(tp), "one card collects a currency")

			status, body := c.call(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": second})
			require.Equal(t, http.StatusBadRequest, status, "%v", body)

		})
	}
}
