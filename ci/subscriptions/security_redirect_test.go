//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Return URLs are an open-redirect vector: an order paid on Stripe's hosted
// page must return to an exact allowed host origin, whichever Client created
// the order.
func TestSecurityCheckoutReturnURLsStayOnHost(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.permanent("content:post")
	for _, tp := range []topology{embedded, remote} {
		c := w.newCustomer()
		session, err := c.sell(tp, order{price: price.ID})
		require.NoError(t, err)
		opened := len(w.stripe.CheckoutSessions())
		for _, bad := range []string{
			"https://evil.test/return",
			"https://e2e.test.evil.test/return",
			"https://evil.test/https://e2e.test/return",
			"http://e2e.test/return",
			"https://e2e.test:8443/return",
		} {
			_, err := session.buy(c, order{rail: "stripe", returnURL: bad})
			require.Error(t, err, "%s %s", tp, bad)
		}
		require.Len(t, w.stripe.CheckoutSessions(), opened, "a refused return URL opens no hosted page")
		paid, err := session.buy(c, order{rail: "stripe", returnURL: "https://e2e.test/return"})
		require.NoError(t, err)
		require.Equal(t, "requires_action", paid.Status, "%+v", paid.Order)
		require.Equal(t, "redirect_to_url", paid.NextAction.Type)
	}
}
