package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/staffperm"
)

// staffPermissions are the server's own permissions on its merchant persona.
var staffPermissions = httproutes.Permissions{AdminRead: staffperm.Read, AdminWrite: staffperm.Write, CatalogWrite: staffperm.Admin, MerchantConfig: staffperm.Admin}

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	opts := httproutes.Options{
		Auth:              s.staffAuth(),
		AuthBindsMerchant: true,
		AdminLimiter:      s.adminLimiter,
		Capabilities:      s.capabilities(),
		Permissions:       staffPermissions,
	}
	httproutes.RegisterStaffRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordMerchantRoute), s.runtime, opts)
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
