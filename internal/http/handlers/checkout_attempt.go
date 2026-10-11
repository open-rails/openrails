package handlers

import (
	"github.com/open-rails/openrails/internal/billingauth"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// checkoutVerifiedPrincipal is the customer route's verified customer as the
// payer the engine checks: interactive only for the customer's own sign-in.
func checkoutVerifiedPrincipal(r *httprequest.Request) billingauth.Payer {
	scope, ok := r.CustomerScope()
	if !ok {
		return billingauth.Payer{}
	}
	c, _ := billingauth.IdentityFromContext(r.Request.Context())
	return scope.Payer(c)
}
