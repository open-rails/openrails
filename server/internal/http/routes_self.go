package server

import (
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerSelfServiceRoutes mounts the browser-direct self-service billing
// surface under /v1/me/*. A trusted issuer mints a short-lived, DPoP-bound
// access token (scope openrails:self) for the signed-in end user; the browser
// calls OpenRails directly with it. Every operation is scoped to the token's
// customer and resolved merchant.
func (s *Server) registerSelfServiceRoutes(mux router.Registrar) {
	// Browser tier: self-service patterns get the static permissive CORS policy.
	httproutes.RegisterCustomerRoutes(
		router.NewMuxRecorded(mux, StandaloneV1Prefix+httproutes.SelfRoutePrefix, s.runtime, s.recordBrowserRoute),
		s.runtime, httproutes.CustomerMount{Auth: s.customerAuth(), AuthBindsMerchant: true, Providers: embedhttp.ProviderRoutesForRuntime(s.runtime, nil)})

	log.WithField("prefix", StandaloneV1Prefix+httproutes.SelfRoutePrefix).
		Info("self-service API routes registered on public handler")
}
