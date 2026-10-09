package routes

import "fmt"

// Permissions are the host's permissions for the staff bundles, as
// Auth.RequirePermission takes them: the admin routes' reads and writes,
// catalog edits, and the merchant's configuration. An empty one leaves its
// routes unmounted.
type Permissions struct{ AdminRead, AdminWrite, CatalogWrite, MerchantConfig string }

// Validate refuses AdminWrite or CatalogWrite without AdminRead.
func (p Permissions) Validate() error {
	switch {
	case p.AdminRead != "":
	case p.AdminWrite != "":
		return fmt.Errorf("openrails: Routes.Permissions.AdminWrite needs AdminRead")
	case p.CatalogWrite != "":
		return fmt.Errorf("openrails: Routes.Permissions.CatalogWrite needs AdminRead")
	}
	return nil
}

// For is the permission a staff route checks: "" when its bundle is not
// mounted.
func (p Permissions) For(r Route) string {
	switch r.Needs() {
	case "AdminRead":
		return p.AdminRead
	case "AdminWrite":
		return p.AdminWrite
	case "CatalogWrite":
		return p.CatalogWrite
	case "MerchantConfig":
		return p.MerchantConfig
	}
	return ""
}

// Needs names the Routes.Permissions field a staff route checks.
func (r Route) Needs() string {
	switch {
	case r.Group == CatalogWrite:
		return "CatalogWrite"
	case r.Group == MerchantConfig:
		return "MerchantConfig"
	case r.Group != Admin:
		return ""
	case r.Level == LevelWrite:
		return "AdminWrite"
	}
	return "AdminRead"
}
