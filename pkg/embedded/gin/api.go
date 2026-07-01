package gin

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
)

// Option configures RegisterAPI by mutating the underlying MountOptions.
type Option func(*MountOptions)

// WithGroups selects the mounted billing route groups. Unset uses the embedded
// defaults (EmbeddedDefaultRouteSets).
func WithGroups(groups ...embedded.RouteSet) Option {
	return func(o *MountOptions) { o.RouteSets = append([]embedded.RouteSet(nil), groups...) }
}

// WithAuthenticator protects buyer/checkout and customer routes. Required when
// RouteSetCheckout or RouteSetCustomer is selected.
func WithAuthenticator(a billingauth.Authenticator) Option {
	return func(o *MountOptions) { o.Authenticator = a }
}

// WithGate protects merchant route groups (merchant_admin, catalog,
// payment_providers, merchant_api). Required when any of those is selected.
func WithGate(g billingauth.Gate) Option {
	return func(o *MountOptions) { o.Gate = g }
}

// WithDelegatedAuthenticator protects the customer self-service surface
// (/v1/me, /v1/customers). Required when RouteSetCustomer is selected.
func WithDelegatedAuthenticator(d billingauth.DelegatedAuthenticator) Option {
	return func(o *MountOptions) { o.DelegatedAuthenticator = d }
}

// WithProviderRoutes selects provider-specific public routes for this mount.
func WithProviderRoutes(routes ProviderRoutes) Option {
	return func(o *MountOptions) { o.ProviderRoutes = &routes }
}

// RegisterAPI mounts the selected OpenRails billing surface onto a dedicated gin
// route group, AuthKit-style. The mount prefix is inferred from the group's
// BasePath(), so the host does not repeat it. The group should be dedicated to
// OpenRails: RegisterAPI installs a catch-all under it.
//
// MountHandler remains the lower-level net/http escape hatch.
//
//	api := router.Group("/billing")
//	openrailsgin.RegisterAPI(api, rt.Embedded(),
//	    openrailsgin.WithGroups(embedded.RouteSetCheckout, embedded.RouteSetCustomer, embedded.RouteSetWebhooks),
//	    openrailsgin.WithAuthenticator(hostAuthn),          // checkout/customer
//	    openrailsgin.WithDelegatedAuthenticator(hostDeleg), // /v1/me + /v1/customers
//	    openrailsgin.WithGate(hostGate),                    // merchant groups
//	)
func RegisterAPI(group *gin.RouterGroup, e *embedded.Embedded, opts ...Option) error {
	if group == nil {
		return fmt.Errorf("openrails: RegisterAPI requires a gin route group")
	}
	if e == nil {
		return fmt.Errorf("openrails: RegisterAPI requires an embedded engine")
	}
	var mo MountOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&mo)
		}
	}
	// BasePath() is authoritative — it is exactly where gin mounts, so the
	// combined handler must strip precisely it. Ignore any prefix an option set.
	mo.MountPrefix = group.BasePath()
	h, err := MountHandler(e, mo)
	if err != nil {
		return err
	}
	for _, method := range billingMethods {
		group.Handle(method, "/*openrails", gin.WrapH(h))
	}
	return nil
}

var billingMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
	http.MethodHead,
}
