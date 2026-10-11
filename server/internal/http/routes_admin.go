package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// routeOptions mount the engine's routes behind AuthKit, as any host does:
// a merchant's staff hold their merchant: permissions in its AuthKit group,
// the platform's their root: ones in the root group.
func (s *Server) routeOptions() httproutes.Options {
	return httproutes.Options{
		Auth:            s.auth,
		ResolveMerchant: s.resolveMerchant,
		AdminLimiter:    s.adminLimiter,
		Capabilities:    s.capabilities(),
		Groups:          s.groups,
		NativeIssuer:    s.controlPlane.Issuer(),
	}
}

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	opts := s.routeOptions()
	httproutes.RegisterStaffRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, opts)
	httproutes.RegisterAppRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, opts)
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
