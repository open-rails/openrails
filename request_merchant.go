package openrails

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

// RequestMerchant is the merchant OpenRails bound a request to before asking
// the host's Authenticator who it is: the route's merchant or, on a server's
// customer surface without one, the merchant the request selected by its
// OpenRails-Merchant header or merchant API host, as OpenRails resolved it. A
// host's Authenticate checks the customer relationship against it rather
// than resolving the selector itself. ok is false where OpenRails bound no
// merchant.
func RequestMerchant(ctx context.Context) (billing.MerchantID, bool) {
	return billingauth.BoundMerchant(ctx)
}
