//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// SEC-33: checkout return URLs are an open-redirect vector (a phishing page
// reached through the merchant's own checkout). A session's return URL must
// name an exact allowed host origin, minted through the embedded and the
// remote Client.
func TestSecurityCheckoutReturnURLsStayOnHost(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	for _, tp := range []topology{embedded, remote} {
		c := w.newCustomer()
		method := c.saveCard("stripe", visa)
		for _, bad := range []string{
			"https://evil.test/return",
			"https://e2e.test.evil.test/return",
			"https://evil.test/https://e2e.test/return",
			"http://e2e.test/return",
			"https://e2e.test:8443/return",
		} {
			_, err := c.sell(tp, order{price: price.ID, successURL: bad})
			require.Error(t, err, "%s %s", tp, bad)
		}
		c.mustCheckout(tp, order{price: price.ID, successURL: "https://e2e.test/return", rail: "stripe", method: method})
	}
}
