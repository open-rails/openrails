package routes

import "fmt"

// Permissions are the host's permissions for the staff route groups, as
// Auth.RequirePermission takes them: the admin group's reads and updates,
// the catalog, the merchant's configuration and its business metrics. Each
// group is independent; an empty one leaves its routes unmounted.
type Permissions struct{ AdminRead, AdminUpdate, Catalog, MerchantConfig, Metrics string }

// Validate refuses AdminUpdate without AdminRead.
func (p Permissions) Validate() error {
	if p.AdminUpdate != "" && p.AdminRead == "" {
		return fmt.Errorf("openrails: Permissions.AdminUpdate needs AdminRead")
	}
	return nil
}

// For is the permission a staff route checks: "" when its group is not
// mounted.
func (p Permissions) For(r Route) string {
	switch r.Needs() {
	case "AdminRead":
		return p.AdminRead
	case "AdminUpdate":
		return p.AdminUpdate
	case "Catalog":
		return p.Catalog
	case "MerchantConfig":
		return p.MerchantConfig
	case "Metrics":
		return p.Metrics
	}
	return ""
}

// Needs names the Routes.Permissions field a staff route checks.
func (r Route) Needs() string {
	switch r.Group {
	case CatalogAdmin:
		return "Catalog"
	case MerchantConfig:
		return "MerchantConfig"
	case Metrics:
		return "Metrics"
	case Admin:
		if r.Level == LevelUpdate {
			return "AdminUpdate"
		}
		return "AdminRead"
	}
	return ""
}
