package authtest

import "github.com/open-rails/openrails/internal/config"

// Groups turns on every staff route group.
func Groups() config.RouteGroups {
	return config.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true}
}

// AdminGroups turns on the admin group alone.
func AdminGroups() config.RouteGroups { return config.RouteGroups{Admin: true} }
