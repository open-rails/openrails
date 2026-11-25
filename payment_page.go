package openrails

import (
	"net/http"
)

// CheckoutPage is the payment page Config.CheckoutOrigin and
// Deps.CheckoutAssets declare: its origin, canonical, and its handler. Serve
// the handler only on the origin: openrailsgin.CheckoutOrigin does. Without
// Deps.CheckoutAssets the handler is nil and orders still get pay URLs, for
// another host serving the checkout origin against the same billing
// database. "" and nil when none is declared, or the origin was refused (it
// must be its own, apart from PublicURL and every declared
// Issuer; Ready reports why).
//
// Orders get their pay URL from CreateOrderParams.PayURL or a customer's
// CreateMyOrderParams.PayURL. Only an order's pay URL secret opens this
// surface; host sessions and staff credentials do not. Merchant settings
// checkout.embed_origins control framing. This enables no customer
// network or sign-in service.
func (e *Engine) CheckoutPage() (origin string, handler http.Handler) {
	return e.engine.CheckoutPage()
}
