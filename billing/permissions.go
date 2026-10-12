package billing

import "strings"

// The OAuth scopes of a trusted issuer's access tokens: ScopeMerchant reaches
// the admin API, ScopeSelf a customer's own billing (/v1/me).
const (
	ScopeMerchant = "openrails:merchant"
	ScopeSelf     = "openrails:self"
)

// Permission is a permission OpenRails checks, named persona:resource:action.
// The persona is where it is held: merchant: in one merchant's own scope
// (its staff group), root: in the Authenticator's root scope, for every
// merchant. A host declares them in its own auth from these constants
// (Permissions), so a renamed one fails its build.
type Permission string

// Persona is where the permission is held: "merchant" or "root".
func (p Permission) Persona() string { return p.part(0) }

// Resource is what the permission is over, such as "customer".
func (p Permission) Resource() string { return p.part(1) }

// Action is what the permission allows, such as "read" or "manage".
func (p Permission) Action() string { return p.part(2) }

func (p Permission) part(i int) string {
	parts := strings.Split(string(p), ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[i]
}

// The permissions of the staff and programmatic routes. A route acting on a
// merchant admits a caller holding its merchant: name in that merchant's
// scope, or its root: name in the root scope.
const (
	// The admin group's reads: customers, their subscriptions and payments.
	MerchantCustomerRead Permission = "merchant:customer:read"
	RootCustomerRead     Permission = "root:customer:read"
	// The admin group's changes: refunds, cancellations, credits.
	MerchantCustomerManage Permission = "merchant:customer:manage"
	RootCustomerManage     Permission = "root:customer:manage"
	// The catalog group.
	MerchantCatalogManage Permission = "merchant:catalog:manage"
	RootCatalogManage     Permission = "root:catalog:manage"
	// The merchant-config group: PSPs, settings.
	MerchantConfigRead   Permission = "merchant:config:read"
	RootConfigRead       Permission = "root:config:read"
	MerchantConfigManage Permission = "merchant:config:manage"
	RootConfigManage     Permission = "root:config:manage"
	// The metrics group: revenue, sales, the dashboard.
	MerchantMetricsRead Permission = "merchant:metrics:read"
	RootMetricsRead     Permission = "root:metrics:read"

	// The programmatic routes' (/v1/app). The content gate.
	MerchantEntitlementsRead Permission = "merchant:entitlements:read"
	RootEntitlementsRead     Permission = "root:entitlements:read"
	// Offers: what a customer may buy.
	MerchantCatalogRead Permission = "merchant:catalog:read"
	RootCatalogRead     Permission = "root:catalog:read"
	// Read usage and running charges without recording or collecting them.
	MerchantUsageRead Permission = "merchant:usage:read"
	RootUsageRead     Permission = "root:usage:read"
	// Admissions and usage events, which charge customers.
	MerchantUsageManage Permission = "merchant:usage:manage"
	RootUsageManage     Permission = "root:usage:manage"
	// Host events: reading and acknowledging them.
	MerchantEventsRead Permission = "merchant:events:read"
	RootEventsRead     Permission = "root:events:read"
	// Merchant orders: previewed, created and withdrawn.
	MerchantOrdersManage Permission = "merchant:orders:manage"
	RootOrdersManage     Permission = "root:orders:manage"
	// A customer by openrails.dev account.
	MerchantCustomersRead Permission = "merchant:customers:read"
	RootCustomersRead     Permission = "root:customers:read"
)

// Permissions is every permission a route checks, in both personas: what a
// host declares in its auth and grants in its roles.
func Permissions() []Permission {
	return []Permission{
		MerchantCustomerRead, RootCustomerRead, MerchantCustomerManage, RootCustomerManage,
		MerchantCatalogManage, RootCatalogManage, MerchantConfigRead, RootConfigRead, MerchantConfigManage, RootConfigManage,
		MerchantMetricsRead, RootMetricsRead,
		MerchantEntitlementsRead, RootEntitlementsRead, MerchantCatalogRead, RootCatalogRead,
		MerchantUsageRead, RootUsageRead, MerchantUsageManage, RootUsageManage,
		MerchantEventsRead, RootEventsRead, MerchantOrdersManage, RootOrdersManage,
		MerchantCustomersRead, RootCustomersRead,
	}
}

// Platform-operator (root) permissions of the standalone server's accounts.
// No route checks them, so Permissions leaves them out; a hosted product
// gates its own operator pages with them.
const (
	RootMerchantsRead    Permission = "root:merchants:read"
	RootMerchantsDelete  Permission = "root:merchants:delete"
	RootMerchantsRestore Permission = "root:merchants:restore"
	// RootWorkerHealthRead is for a worker-health view, whose last_error
	// text can name any merchant's records.
	RootWorkerHealthRead      Permission = "root:worker-health:read"
	RootAdminRateLimitsUnlock Permission = "root:admin-rate-limits:unlock"
)
