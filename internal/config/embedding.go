package config

import (
	"fmt"

	"github.com/open-rails/openrails/internal/billingauth"
)

// Routes is the HTTP surface the adapters mount on the root router, and the
// Auth that guards it. The public, customer (/v1/me) and webhook routes are
// always mounted; every other route group is off until RouteGroups turns it
// on.
type Routes struct {
	// Auth is the host's auth middleware. OpenRails stacks it on its own
	// routes: Required on /v1/me and the programmatic routes (and to show a
	// checkout session's buyer their saved cards); RequirePermission with the
	// route's group permission on the staff routes, and Sensitive for a user
	// in person on one that moves money or removes access. Required.
	Auth billingauth.Auth
	// Prefix is where the API is mounted: "/billing" serves /billing/v1/*.
	// Empty is the root.
	Prefix string
	// RouteGroups turns route groups on; each is off by default.
	RouteGroups RouteGroups
	// Permissions are what a caller must hold for each staff group that is
	// on. Mount fails when a staff group is on without its permission, or a
	// permission is given for a group that is off.
	Permissions Permissions
	// AdminConsole serves the staff dashboard at Prefix's /admin, over the
	// staff routes, signing staff in through AuthKit's /api/v1 on the same
	// origin. It has one area per staff group, shown to staff holding its
	// permission. It needs at least one staff group on and
	// Deps.ConsoleAssets.
	AdminConsole bool
}

// RouteGroups are Routes.RouteGroups: the route groups beyond the public,
// customer and webhook routes, each off until turned on.
type RouteGroups struct {
	// Admin is customer support (/v1/admin): customers, their
	// subscriptions, payments, refunds, invoices, credits, access and usage.
	// Callers need Permissions.AdminRead; changes also
	// Permissions.AdminUpdate, without which it is read-only.
	Admin bool
	// Catalog is the catalog: products, prices, meters and their rates,
	// archiving a product, applying a catalog document, moving subscribers
	// between prices. It needs Permissions.Catalog. With Config.Catalog, the
	// file skips what an edit changed.
	Catalog bool
	// MerchantConfig is the merchant's own configuration: PSPs, settings,
	// billing import and export, the dashboard layout. It needs
	// Permissions.MerchantConfig.
	MerchantConfig bool
	// Metrics is the merchant's business metrics, read-only: revenue, sales,
	// the metrics queries and the dashboard. It needs Permissions.Metrics.
	Metrics bool
	// Programmatic is the routes the host's backend calls over HTTP at
	// /v1/app: usage, admissions, provider operations, host events and SCIM
	// provisioning. They take the application Auth admits (its Identity's
	// SubjectKind) and refuse a person; no permission is involved. SCIM also
	// takes the merchant's provisioning token, and is not mounted with
	// Deps.UserInfo, which reads the directory instead. Embedded, the Client
	// calls the same operations in process without it.
	Programmatic bool
}

// Permissions are Routes.Permissions: the host's permissions, such as
// AuthKit's iam.Perm, that OpenRails passes to Auth.RequirePermission, one
// per staff group that is on. Mount reads each String() once.
type Permissions struct {
	// AdminRead is what the admin group's callers hold.
	AdminRead fmt.Stringer
	// AdminUpdate is what its changes also need: cancellations, refunds,
	// credit grants. Optional: without it the admin group is read-only.
	AdminUpdate fmt.Stringer
	// Catalog is what the catalog group's callers hold.
	Catalog fmt.Stringer
	// MerchantConfig is what the merchant-config group's callers hold.
	MerchantConfig fmt.Stringer
	// Metrics is what the metrics group's callers hold. Support staff who
	// help customers need only AdminRead.
	Metrics fmt.Stringer
}

// ConsoleMount is where a standalone server serves the admin console, and
// what it serves it with.
type ConsoleMount struct {
	// Path is where the console is served: an absolute path without a
	// trailing slash, outside the API. Empty is "/admin".
	Path string
	// AuthBaseURL is the AuthKit JSON API staff sign in through; empty is
	// the server's own.
	AuthBaseURL string
	// Extensions is the host's data for the console extensions it builds in
	// (scripts/build-admin-console.sh --extensions), keyed by extension id and
	// served verbatim in config.json. OpenRails never reads it.
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
