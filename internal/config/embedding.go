package config

import "github.com/open-rails/openrails/internal/billingauth"

// Routes is the HTTP surface the adapters mount on the root router, and the
// host's auth that says who its requests are. The public, customer (/v1/me)
// and webhook routes are always mounted; every other route group is off
// until RouteGroups turns it on.
type Routes struct {
	// Auth says who a request is; OpenRails decides what each route admits
	// and answers refusals itself. It asks Authenticate once per request,
	// then the Verified's Can for a staff or programmatic route's permission
	// and, for a person on a route that moves money or removes access,
	// CheckRecentSignIn. Required.
	//
	// A request acts on the one merchant its credential is bound to, and
	// nothing in the request names it: the merchant whose scope the
	// Verified's BoundScope is (authkit/auth Bound: {Authority: RootScope's,
	// ID: the merchant's id}), else the merchant that declares the
	// Identity's Issuer (MerchantDeclaration.Issuer). A credential bound to
	// neither is refused 403 merchant_unresolved.
	//
	// With a route group on, Auth is a authkit/auth Root, as AuthKit's
	// ak.Authenticator() is. A route acting on merchant M admits its
	// merchant: permission held in M's scope ({Authority: RootScope's, ID:
	// M's id}) or its root: twin held in RootScope. The names are
	// billing.Permissions: declare them in your auth and grant them in its
	// roles. Mount fails for a route neither of whose names Auth knows
	// (authkit/auth PermissionCatalog); a name it does not know is never
	// asked.
	Auth billingauth.Authenticator
	// Prefix is where the API is mounted: "/billing" serves /billing/v1/*.
	// Empty is the root.
	Prefix string
	// RouteGroups turns route groups on; each is off by default.
	RouteGroups RouteGroups
	// AdminConsole serves the staff dashboard at Prefix's /admin, over the
	// staff routes, signing staff in through the host's AuthKit JSON API on
	// the same origin (AdminConsoleAuthBase). It has one area per staff
	// group, shown to staff holding its permission. It needs at least one
	// staff group on and Deps.ConsoleAssets.
	AdminConsole bool
	// AdminConsoleAuthBase is the path the host's AuthKit JSON API is served
	// at on the console's origin, such as AuthKit's APIBase() ("/auth/v1"
	// with APIPath "/auth"); only the console reads it. An absolute path
	// without a trailing slash; empty is AuthKit's default, "/api/v1".
	AdminConsoleAuthBase string
}

// RouteGroups are Routes.RouteGroups: the route groups beyond the public,
// customer and webhook routes, each off until turned on.
type RouteGroups struct {
	// Admin is customer support (/v1/admin): customers, their
	// subscriptions, payments, refunds, orders, credits, access and usage.
	// Its reads need billing.MerchantCustomerRead or RootCustomerRead, its
	// changes MerchantCustomerManage or RootCustomerManage.
	Admin bool
	// Catalog is the catalog: products, prices, meters and their rates,
	// archiving a product, applying a catalog document, moving subscribers
	// between prices. It needs billing.MerchantCatalogManage or
	// RootCatalogManage. A declared merchant's Catalog skips what an edit
	// changed.
	Catalog bool
	// MerchantConfig is the merchant's own configuration: PSPs, settings,
	// billing import and export, the dashboard layout. It needs
	// billing.MerchantConfigManage or RootConfigManage.
	MerchantConfig bool
	// Metrics is the merchant's business metrics, read-only: revenue, sales,
	// the metrics queries and the dashboard. It needs
	// billing.MerchantMetricsRead or RootMetricsRead.
	Metrics bool
	// Programmatic is the routes the host's backend calls over HTTP at
	// /v1/app: entitlement checks, offers, usage and admissions, provider
	// operations, host events and SCIM provisioning. They take an
	// application Auth says a request is (its Identity's SubjectKind) and
	// refuse a person. Each route needs its own permission (entitlements,
	// offers, usage, costs, events, orders, customers), so a program's role
	// grants only what its task needs. SCIM takes any application at the
	// merchant or the merchant's provisioning token, and is not mounted with
	// Deps.UserInfo, which reads the directory instead. Embedded, the Client
	// calls the same operations in process without it.
	Programmatic bool
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
