package openrails

import (
	"io/fs"
	"net/http"
)

// HostedCheckout configures an embedded engine's hosted order checkout on its
// own origin, before Start or serving HTTP. Mount the returned handler only on
// the returned host. assets is the built checkout page (web/checkout); it must
// contain index.html. Nil assets configures URL issuance only and returns a
// nil handler, for another host serving checkout against the same billing
// database. The origin must be separate from declared merchant
// issuers, PublicBillingBaseURL and DashboardBaseURL.
//
// The host creates orders with CreateOrderParams.Checkout after authorizing
// its customer. Only the order's checkout credential opens this surface; host
// sessions and staff credentials do not. Merchant checkout.embed_origins still
// controls framing. This enables no customer network or sign-in service.
func (e *Engine) HostedCheckout(origin string, assets fs.FS) (string, http.Handler, error) {
	return e.engine.HostedCheckout(origin, assets)
}
