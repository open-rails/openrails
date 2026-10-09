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
)

// Permissions gives each bundle a permission naming it.
func Permissions() config.Permissions {
	return config.Permissions{AdminRead: Perm(StaffRead), AdminWrite: Perm(StaffWrite), CatalogWrite: Perm(StaffCatalog), MerchantConfig: Perm(StaffAdmin)}
}

// AdminPermissions is the admin bundle alone.
func AdminPermissions() config.Permissions {
	return config.Permissions{AdminRead: Perm(StaffRead), AdminWrite: Perm(StaffWrite)}
}
