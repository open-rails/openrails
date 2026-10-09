package server

import (
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/staffperm"
)

// staffPermissions are the server's own permissions on its merchant persona.
var staffPermissions = httproutes.Permissions{AdminRead: staffperm.Read, AdminUpdate: staffperm.Write, Catalog: staffperm.Admin, MerchantConfig: staffperm.Admin, Metrics: staffperm.Metrics}

// staffPermissionsFor is staffPermissions for the staff route groups that
// are on.
func staffPermissionsFor(groups config.RouteGroups) httproutes.Permissions {
	var p httproutes.Permissions
	if groups.Admin {
		p.AdminRead, p.AdminUpdate = staffPermissions.AdminRead, staffPermissions.AdminUpdate
	}
	if groups.Catalog {
		p.Catalog = staffPermissions.Catalog
	}
	if groups.MerchantConfig {
		p.MerchantConfig = staffPermissions.MerchantConfig
	}
	if groups.Metrics {
		p.Metrics = staffPermissions.Metrics
	}
	return p
}

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	opts := httproutes.Options{
		Auth:              s.staffAuth(),
		AuthBindsMerchant: true,
		AdminLimiter:      s.adminLimiter,
		Capabilities:      s.capabilities(),
		Permissions:       s.permissions,
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
