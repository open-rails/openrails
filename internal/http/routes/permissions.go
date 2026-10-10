package routes

import "fmt"

// Permissions are the host's permissions, as the gate asks the Verified's Can
// for them: the staff groups' (the admin group's reads and updates, the
// catalog, the merchant's configuration, its business metrics) and the
// programmatic routes' (entitlement checks, usage, provider costs, host
// events). An empty one leaves its routes unmounted.
type Permissions struct {
	AdminRead, AdminUpdate, Catalog, MerchantConfig, Metrics string
	Entitlements, Usage, Costs, Events                       string
}

// Validate refuses AdminUpdate without AdminRead.
func (p Permissions) Validate() error {
	if p.AdminUpdate != "" && p.AdminRead == "" {
		return fmt.Errorf("openrails: Permissions.AdminUpdate needs AdminRead")
	}
	return nil
}

// Staff is p's staff groups' permissions alone.
func (p Permissions) Staff() Permissions {
	return Permissions{AdminRead: p.AdminRead, AdminUpdate: p.AdminUpdate, Catalog: p.Catalog, MerchantConfig: p.MerchantConfig, Metrics: p.Metrics}
}

// App is p's programmatic routes' permissions alone.
func (p Permissions) App() Permissions {
	return Permissions{Entitlements: p.Entitlements, Usage: p.Usage, Costs: p.Costs, Events: p.Events}
}

// For is the permission a route checks: "" when it needs none, or its
// permission is not given and it is not mounted.
func (p Permissions) For(r Route) string {
	switch r.Needs() {
	case NeedAdminRead:
		return p.AdminRead
	case NeedAdminUpdate:
		return p.AdminUpdate
	case NeedCatalog:
		return p.Catalog
	case NeedMerchantConfig:
		return p.MerchantConfig
	case NeedMetrics:
		return p.Metrics
	case NeedEntitlements:
		return p.Entitlements
	case NeedUsage:
		return p.Usage
	case NeedCosts:
		return p.Costs
	case NeedEvents:
		return p.Events
	}
	return ""
}

// Need names the Routes.Permissions field a route's caller holds.
type Need string

const (
	NeedAdminRead      Need = "AdminRead"
	NeedAdminUpdate    Need = "AdminUpdate"
	NeedCatalog        Need = "Catalog"
	NeedMerchantConfig Need = "MerchantConfig"
	NeedMetrics        Need = "Metrics"
	// The programmatic routes': a route declares its own (Route.Permission).
	NeedEntitlements Need = "Entitlements"
	NeedUsage        Need = "Usage"
	NeedCosts        Need = "Costs"
	NeedEvents       Need = "Events"
)

// appNeeds are the permissions a programmatic route may declare.
var appNeeds = []Need{NeedEntitlements, NeedUsage, NeedCosts, NeedEvents}

// Needs names the Routes.Permissions field a route checks: a staff route's
// group's (and level's), a programmatic route's own, none for SCIM.
func (r Route) Needs() Need {
	switch r.Group {
	case CatalogAdmin:
		return NeedCatalog
	case MerchantConfig:
		return NeedMerchantConfig
	case Metrics:
		return NeedMetrics
	case Admin:
		if r.Level == LevelUpdate {
			return NeedAdminUpdate
		}
		return NeedAdminRead
	case App:
		return r.Permission
	}
	return ""
}
