package server

import (
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/staffperm"
)

// merchantPermissions are the server's own permissions on its merchant
// persona.
var merchantPermissions = httproutes.Permissions{
	AdminRead: staffperm.BillingRead, AdminUpdate: staffperm.BillingManage, Catalog: staffperm.CatalogManage,
	MerchantConfig: staffperm.ConfigManage, Metrics: staffperm.MetricsRead,
	Entitlements: staffperm.EntitlementsRead, Usage: staffperm.UsageManage, Costs: staffperm.CostsManage, Events: staffperm.EventsRead,
}

// permissionsFor is merchantPermissions for the route groups that are on.
func permissionsFor(groups config.RouteGroups) httproutes.Permissions {
	var p httproutes.Permissions
	if groups.Admin {
		p.AdminRead, p.AdminUpdate = merchantPermissions.AdminRead, merchantPermissions.AdminUpdate
	}
	if groups.Catalog {
		p.Catalog = merchantPermissions.Catalog
	}
	if groups.MerchantConfig {
		p.MerchantConfig = merchantPermissions.MerchantConfig
	}
	if groups.Metrics {
		p.Metrics = merchantPermissions.Metrics
	}
	if groups.Programmatic {
		app := merchantPermissions.App()
		p.Entitlements, p.Usage, p.Costs, p.Events = app.Entitlements, app.Usage, app.Costs, app.Events
	}
	return p
}

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	auth := s.staffAuth()
	opts := httproutes.Options{
		Auth:            auth,
		Scope:           auth.Scope,
		ResolveMerchant: auth.ResolveMerchant,
		AdminLimiter:    s.adminLimiter,
		Capabilities:    s.capabilities(),
		Permissions:     s.permissions,
	}
	httproutes.RegisterStaffRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordMerchantRoute), s.runtime, opts)
	if s.groups.Programmatic {
		// SCIM provisioning included: the server keeps the pushed copy of
		// each merchant's users.
		httproutes.RegisterAppRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordMerchantRoute), s.runtime, opts)
	}
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
