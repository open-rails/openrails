package config

import (
	"fmt"

	"github.com/open-rails/openrails/internal/billingauth"
)

// Routes is the HTTP surface the adapters mount on the root router, and the
// Auth that guards it. The public, customer (/v1/me) and webhook routes are
// always mounted; the admin and merchant-config bundles follow Permissions.
type Routes struct {
	// Auth is the host's auth middleware. OpenRails stacks it on its own
	// routes: Required on /v1/me (and to show a checkout session's buyer
	// their saved cards); RequirePermission with the route's bundle
	// permission on the admin and merchant-config routes, and Sensitive for a
	// user in person on one that moves money or removes access. Required.
	Auth billingauth.Auth
	// Prefix is where the API is mounted: "/billing" serves /billing/v1/*.
	// Empty is the root.
	Prefix string
	// Permissions are the host's permissions for the admin and
	// merchant-config bundles; a bundle is mounted only with its permission.
	Permissions Permissions
	// CustomerProfiles mount further customer surfaces: another prefix,
	// another merchant, or their own Auth.
	CustomerProfiles []CustomerRoutes
	// CookieOrigin admits cookie-authenticated requests from this exact origin
	// (https, or http on loopback); unsafe ones must carry it as Origin.
	// Empty strips ambient cookies: credentials are explicit headers.
	CookieOrigin string
	// AdminConsole serves the staff dashboard over the admin routes; it needs
	// Permissions.AdminRead. Nil, the default, mounts no console routes.
	AdminConsole *AdminConsole
}

// Permissions are Routes.Permissions: the host's permissions, such as
// AuthKit's iam.Perm, that OpenRails passes to Auth.RequirePermission. Mount
// reads each String() once.
type Permissions struct {
	// AdminRead mounts the admin routes that read (/v1/admin), each behind it.
	AdminRead fmt.Stringer
	// AdminWrite mounts the admin routes that write, each behind it. It needs
	// AdminRead.
	AdminWrite fmt.Stringer
	// CatalogWrite mounts the catalog edits: products, prices, meters and
	// their rates, archiving a product. They are refused while Config.Catalog
	// is the catalog's truth, and every mount in one process must agree on
	// whether it is given. It needs AdminRead, which serves catalog reads.
	CatalogWrite fmt.Stringer
	// MerchantConfig mounts the merchant's own configuration: PSPs,
	// settings, billing import and export, the dashboard layout, reads and
	// writes alike.
	MerchantConfig fmt.Stringer
}

// AdminConsole is Routes.AdminConsole. The console is a static app that
// calls the admin API at Routes.Prefix and AuthKit on its own origin.
type AdminConsole struct {
	// Path is where the console is served: an absolute path without a
	// trailing slash, outside Prefix's API. Empty is "/admin".
	Path string
	// AuthBaseURL is the AuthKit JSON API staff sign in through. Required
	// when embedded; the standalone server defaults it to its own AuthKit.
	AuthBaseURL string
	// Extensions is the host's data for the console extensions it builds in
	// (scripts/build-admin-console.sh --extensions), keyed by extension id and
	// served verbatim in config.json. OpenRails never reads it; a plain
	// console build leaves it empty.
	Extensions map[string]any
}

// CheckoutConfig is Config.Checkout: the shared payment page. The zero value
// is the single-site case: the app renders billing-ui's <Checkout> itself
// against the session routes, with no payment page and no frame.
type CheckoutConfig struct {
	// PageURL is where the shared payment page (billing-ui's <CheckoutPage>)
	// is served; a minted session's URL is PageURL#<id>. Every app selling
	// through the page sets it.
	PageURL string
	// EmbedOrigins are the sites (scheme://host[:port]) allowed to frame the
	// page this host serves. Only the payment host sets it.
	EmbedOrigins []string
}

// CustomerRoutes is one further customer surface (Routes.CustomerProfiles):
// every customer route, never administration, provider callbacks or
// credential management.
type CustomerRoutes struct {
	// Prefix is the surface's base relative to the mount; default /v1/me.
	// Whole-segment {parameters} reach the profile's Auth through
	// Request.PathValue.
	Prefix string
	// Merchant is the merchant slug the surface's customers buy from;
	// default the configured merchant (Config.Merchant) or, on a standalone
	// server, the merchant each request selects (openrails.RequestMerchant).
	Merchant string
	// Auth guards this surface's customers (its Required) instead of
	// Routes.Auth.
	Auth billingauth.Auth
}
