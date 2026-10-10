package routes

import "fmt"

// Permissions are the host's permissions, as the gate asks the Verified's Can
// for them: the staff groups' (the admin group's reads and updates, the
// catalog, the merchant's configuration, its business metrics) and the
// programmatic routes' (entitlement checks, offers, usage, provider costs,
// host events). An empty one leaves its routes unmounted. Each field is
// named by a Need.
type Permissions struct {
	AdminRead, AdminUpdate, Catalog, MerchantConfig, Metrics string
	Entitlements, Offers, Usage, Costs, Events               string
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
	NeedOffers       Need = "Offers"
	NeedUsage        Need = "Usage"
	NeedCosts        Need = "Costs"
	NeedEvents       Need = "Events"
)

// StaffNeeds are the staff groups' permissions.
var StaffNeeds = []Need{NeedAdminRead, NeedAdminUpdate, NeedCatalog, NeedMerchantConfig, NeedMetrics}

// AppNeeds are the permissions a programmatic route may declare. A new one is
// a Need here, a field of Permissions (and of the public config.Permissions)
// named by it, and its case in Field; TestEveryNeedIsAField names what is
// missing.
var AppNeeds = []Need{NeedEntitlements, NeedOffers, NeedUsage, NeedCosts, NeedEvents}

// AllNeeds is every Permissions field.
func AllNeeds() []Need { return append(append([]Need(nil), StaffNeeds...), AppNeeds...) }

// Field is p's field n names; nil for an unknown Need.
func (p *Permissions) Field(n Need) *string {
	switch n {
	case NeedAdminRead:
		return &p.AdminRead
	case NeedAdminUpdate:
		return &p.AdminUpdate
	case NeedCatalog:
		return &p.Catalog
	case NeedMerchantConfig:
		return &p.MerchantConfig
	case NeedMetrics:
		return &p.Metrics
	case NeedEntitlements:
		return &p.Entitlements
	case NeedOffers:
		return &p.Offers
	case NeedUsage:
		return &p.Usage
	case NeedCosts:
		return &p.Costs
	case NeedEvents:
		return &p.Events
	}
	return nil
}

// Validate refuses AdminUpdate without AdminRead.
func (p Permissions) Validate() error {
	if p.AdminUpdate != "" && p.AdminRead == "" {
		return fmt.Errorf("openrails: Permissions.AdminUpdate needs AdminRead")
	}
	return nil
}

// only is p's needs alone.
func (p Permissions) only(needs []Need) Permissions {
	var out Permissions
	for _, n := range needs {
		*out.Field(n) = *p.Field(n)
	}
	return out
}

// Staff is p's staff groups' permissions alone.
func (p Permissions) Staff() Permissions { return p.only(StaffNeeds) }

// App is p's programmatic routes' permissions alone.
func (p Permissions) App() Permissions { return p.only(AppNeeds) }

// For is the permission a route checks: "" when it needs none, or its
// permission is not given and it is not mounted.
func (p Permissions) For(r Route) string {
	if f := p.Field(r.Needs()); f != nil {
		return *f
	}
	return ""
}

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
