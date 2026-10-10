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
	Entitlements: staffperm.EntitlementsRead, Offers: staffperm.CatalogRead, Usage: staffperm.UsageManage, Costs: staffperm.CostsManage, Events: staffperm.EventsRead,
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
		for _, n := range httproutes.AppNeeds {
			*p.Field(n) = *merchantPermissions.Field(n)
		}
	}
	return p
}

// routeOptions mount the engine's routes behind AuthKit, as any host does,
// with each merchant's AuthKit group as its scope.
func (s *Server) routeOptions() httproutes.Options {
	return httproutes.Options{
		Auth:            s.auth,
		Scope:           s.scope,
		ResolveMerchant: s.resolveMerchant,
		AdminLimiter:    s.adminLimiter,
		Capabilities:    s.capabilities(),
		Permissions:     s.permissions,
	}
}

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	opts := s.routeOptions()
	httproutes.RegisterStaffRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, opts)
	if s.groups.Programmatic {
		httproutes.RegisterAppRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, opts)
	}
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
