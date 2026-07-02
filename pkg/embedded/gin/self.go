package gin

import (
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SelfHandler returns the mountable browser-direct SELF-SERVICE surface for an
// embedded host (issues #339/#467). A host-supplied
// billingauth.DelegatedAuthenticator is supplied by the host at mount time.
//
// Routes are served at the CANONICAL embedded paths, alongside the
// NewHTTPHandler surface:
//
//	/billing/v1/me/*                      (RegisterSelfServiceRoutes)
//	/billing/v1/customers/:customer_id/*  (RegisterCustomerTreasuryRoutes)
//
// so a host that mounts NewHTTPHandler under /billing without prefix stripping
// can route this subtree to this handler and everything else to the base
// handler. The same neutral base middleware stack wraps it. Host-supplied
// delegated authenticators own their own browser-origin policy before
// returning a principal.
//
// Since #670 the returned handler is fully gin-free (embedhttp.NewSelfHandler);
// this constructor stays in the gin shim for embedding-host compatibility.
// Errors when the app graph is not initialized or no identity source can be
// derived — the surface is never mounted without authentication.
func SelfHandler(e *embedded.Embedded, authn billingauth.DelegatedAuthenticator) (http.Handler, error) {
	return selfHandler(e, authn, nil)
}

func selfHandler(e *embedded.Embedded, authn billingauth.DelegatedAuthenticator, providerRouteOverride *routesurface.ProviderRoutes) (http.Handler, error) {
	if e == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	if authn == nil {
		return nil, fmt.Errorf("embedded billing: self surface requires MountOptions.DelegatedAuthenticator")
	}
	var configured merchant.ID
	if a.Runtime != nil {
		configured = a.Runtime.ConfiguredMerchant
	}
	return newSelfHandler(a.Runtime, authn, configured, providerRouteOverride), nil
}

// newSelfHandler delegates to the neutral assembly; kept as the unit-testable
// seam (no live app graph required).
func newSelfHandler(rt *app.Runtime, authn billingauth.DelegatedAuthenticator, configured merchant.ID, providerRouteOverride *routesurface.ProviderRoutes) http.Handler {
	return embedhttp.NewSelfHandler(rt, authn, configured, providerRouteOverride)
}
