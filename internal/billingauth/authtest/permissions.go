package authtest

import "github.com/open-rails/openrails/internal/config"

// Perm is a host's permission.
type Perm string

func (p Perm) String() string { return string(p) }

// The permissions Permissions names.
const (
	StaffRead    = "staff:read"
	StaffWrite   = "staff:write"
	StaffCatalog = "staff:catalog"
	StaffAdmin   = "staff:admin"
	StaffMetrics = "staff:metrics"
)

// Permissions gives each group a permission naming it.
func Permissions() config.Permissions {
	return config.Permissions{AdminRead: Perm(StaffRead), AdminUpdate: Perm(StaffWrite), Catalog: Perm(StaffCatalog), MerchantConfig: Perm(StaffAdmin), Metrics: Perm(StaffMetrics)}
}

// AdminPermissions is the admin group alone.
func AdminPermissions() config.Permissions {
	return config.Permissions{AdminRead: Perm(StaffRead), AdminUpdate: Perm(StaffWrite)}
}

// Groups turns on every staff route group Permissions names.
func Groups() config.RouteGroups {
	return config.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true}
}

// AdminGroups turns on the admin group alone.
func AdminGroups() config.RouteGroups { return config.RouteGroups{Admin: true} }
