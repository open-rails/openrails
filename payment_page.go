package openrails

import (
	"io/fs"
	"net/http"
)

// PaymentPage configures an embedded engine's payment page on its own
// origin, before Start or serving HTTP, and answers that origin canonically
// and the page's handler. Serve the handler only on the origin:
// openrailsgin.PaymentOrigin does. assets is the built payment page
// (web/pay); it must contain index.html. Nil assets configures pay URL
// issuance only and returns a nil handler, for another host serving the
// payment origin against the same billing database. The origin must be
// separate from declared merchant issuers, PublicBillingBaseURL and
// DashboardBaseURL.
//
// Orders get their pay URL from CreateOrderParams.PayURL or a customer's
// CreateMyOrderParams.PayURL. Only an order's pay URL secret opens this
// surface; host sessions and staff credentials do not. Merchant settings
// payment_page.embed_origins control framing. This enables no customer
// network or sign-in service.
func (e *Engine) PaymentPage(origin string, assets fs.FS) (string, http.Handler, error) {
	return e.engine.PaymentPage(origin, assets)
}
