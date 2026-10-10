package authtest

import "github.com/open-rails/openrails/internal/config"

// Perm is a host's permission.
type Perm string

func (p Perm) String() string { return string(p) }

// The permissions Permissions names.
const (
	BillingRead   = "root:billing:read"
	BillingManage = "root:billing:manage"
	CatalogManage = "root:catalog:manage"
	ConfigManage  = "root:config:manage"
	MetricsRead   = "root:metrics:read"
)

// Permissions gives each group a permission naming it.
func Permissions() config.Permissions {
	return config.Permissions{AdminRead: Perm(BillingRead), AdminUpdate: Perm(BillingManage), Catalog: Perm(CatalogManage), MerchantConfig: Perm(ConfigManage), Metrics: Perm(MetricsRead)}
}

// AdminPermissions is the admin group alone.
func AdminPermissions() config.Permissions {
	return config.Permissions{AdminRead: Perm(BillingRead), AdminUpdate: Perm(BillingManage)}
}

// Groups turns on every staff route group Permissions names.
func Groups() config.RouteGroups {
	return config.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true}
}

// AdminGroups turns on the admin group alone.
func AdminGroups() config.RouteGroups { return config.RouteGroups{Admin: true} }
