package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// routeOptions mount the engine's routes behind AuthKit, as any host does:
// each request acts on the merchant whose AuthKit group its credential is
// bound to. A merchant's staff hold their merchant: permissions in that
// group, the platform's their root: ones in the root group.
func (s *Server) routeOptions() httproutes.Options {
	return httproutes.Options{
		Auth:         s.auth,
		AdminLimiter: s.adminLimiter,
		Capabilities: s.capabilities(),
		Groups:       s.groups,
	}
}

// The staff and programmatic routes are browser tier: a merchant's own site
// renders billing-ui's admin screens and calls them cross-origin with the
// access token its trusted issuer mints for this server. That bearer token is
// their only credential (cookies never reach an Authenticator), so allowing
// every origin without credentials exposes nothing a stolen token would not.
func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	opts := s.routeOptions()
	httproutes.RegisterStaffRoutes(s.Log, router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, opts)
	httproutes.RegisterAppRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, opts)
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
