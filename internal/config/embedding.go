package config

import (
	"fmt"

	"github.com/open-rails/openrails/internal/billingauth"
)

// Routes selects the HTTP surface Client.Routes returns and the adapters
// mount on the root router, and the Auth that guards it. Processor webhooks
// and GET /v1/capabilities are always mounted.
type Routes struct {
	// Auth is the host's auth middleware. OpenRails stacks it on its own
	// routes by tier: Required on /v1/me (and to show a checkout session's
	// buyer their saved cards); RequirePermission with each staff route's
	// guard, and Sensitive for a user in person moving money, on the merchant
	// API. A selection whose groups need it refuses to mount without it.
	Auth billingauth.Auth
	// Prefix is where the API is mounted: "/billing" serves /billing/v1/*.
	// Empty is the root.
	Prefix string
	// Storefront mounts what a buyer's browser needs without signing in:
	// products, prices, currencies, checkout config, checkout sessions (read
	// and pay), Solana Pay and the captcha.
	Storefront bool
	// Customers mounts signed-in customers' own billing at /v1/me/*, behind
	// Auth.Required. CustomersNone, the zero value, mounts none.
	Customers CustomerHTTPScope
	// Merchant mounts staff work on customers (/v1/merchant/*): payments and
	// refunds, subscriptions, invoices, credits, access, usage, metrics and
	// operations, each route behind Auth.RequirePermission with its guard
	// (StaffReads, StaffWrites or a narrower one), and Auth.Sensitive for a
	// user in person on one that moves money or removes access.
	Merchant bool
	// MerchantConfig mounts the merchant's own configuration: PSPs, settings,
	// catalog edits, billing import and export, the dashboard layout, each
	// route behind its guard (MerchantConfig or a narrower one). Catalog
	// edits are refused while Config.Catalog is the catalog's truth. The Go
	// client configures the merchant either way. Every mount in one process
	// must agree.
	MerchantConfig bool
	// Guards are the host's permissions for the staff routes Merchant and
	// MerchantConfig mount: a level group (StaffReads, StaffWrites,
	// MerchantConfig), a resource group (Refunds) or one route (RefundPayment),
	// the most specific winning. Mounting fails when a mounted staff route has
	// no guard, two guards of one tier cover a route, or a guard covers no
	// mounted route.
	Guards Guards
	// CustomerProfiles mount further customer surfaces: another prefix,
	// another merchant, or their own Auth.
	CustomerProfiles []CustomerRoutes
	// CookieOrigin admits cookie-authenticated requests from this exact origin
	// (https, or http on loopback); unsafe ones must carry it as Origin.
	// Empty strips ambient cookies: credentials are explicit headers.
	CookieOrigin string
	// AdminConsole serves the merchant admin console, the staff dashboard
	// over the merchant API; it needs Merchant. Nil, the default, mounts no
	// console routes at all.
	AdminConsole *AdminConsole
}

// RouteSet names staff routes a guard covers: a level group, a resource group
// or one route (openrails' generated constants).
type RouteSet string

// Guards maps route sets to the host's permissions, such as AuthKit's
// iam.Perm. Mount reads each String() once and passes it to
// Auth.RequirePermission.
type Guards map[RouteSet]fmt.Stringer

// AdminConsole is Routes.AdminConsole. The console is a static app that
// calls the merchant API at Routes.Prefix and AuthKit on its own origin.
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

// CustomerHTTPScope selects the routes of a customer surface.
type CustomerHTTPScope uint8

const (
	// CustomersNone mounts no customer routes.
	CustomersNone CustomerHTTPScope = iota
	// CustomerSelfService is the full customer self-service API.
	CustomerSelfService
	// CustomerSubscriptionManagement is cancellation, resumption, subscription
	// payment-method changes and invoice collection-method selection only.
	CustomerSubscriptionManagement
	// CustomerBillingManagement adds billing history, purchased access, saved
	// methods, payment recovery and paying a checkout session the merchant
	// minted, without starting checkouts or changing plans.
	CustomerBillingManagement
)

// CustomerRoutes is one further customer surface (Routes.CustomerProfiles).
// It never includes merchant administration, provider callbacks or
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
	// Scope selects the surface's routes; it is required.
	Scope CustomerHTTPScope
	// Auth guards this surface's customers (its Required) instead of
	// Routes.Auth.
	Auth billingauth.Auth
}
