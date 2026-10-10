// Package staffperm is the standalone server's permissions: its merchant
// persona's, held in each merchant's group, which the control plane declares
// and the server's staff and programmatic routes check.
package staffperm

// The server's Routes.Permissions, one per field.
const (
	BillingRead   = "merchant:billing:read"   // AdminRead: customer support's reads
	BillingManage = "merchant:billing:manage" // AdminUpdate: refunds, cancellations, credits
	CatalogManage = "merchant:catalog:manage" // Catalog
	ConfigManage  = "merchant:config:manage"  // MerchantConfig: PSPs, settings
	MetricsRead   = "merchant:metrics:read"   // Metrics: revenue, sales; support does not hold it

	EntitlementsRead = "merchant:entitlements:read" // Entitlements: the content gate
	UsageManage      = "merchant:usage:manage"      // Usage: admissions and usage events
	CostsManage      = "merchant:costs:manage"      // Costs: provider operations
	EventsRead       = "merchant:events:read"       // Events: host events

	// AuthKit's built-ins on the merchant persona: the team, and the
	// merchant's API keys.
	MembersRead       = "merchant:members:read"
	MembersManage     = "merchant:members:manage"
	CredentialsManage = "merchant:credentials:manage"

	// All is the merchant owner's grant.
	All = "merchant:*"
)

// Declared is every permission the merchant persona declares: one per
// Routes.Permissions field.
var Declared = []string{
	BillingRead, BillingManage, CatalogManage, ConfigManage, MetricsRead,
	EntitlementsRead, UsageManage, CostsManage, EventsRead,
}
