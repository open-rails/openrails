package server

import (
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerSelfServiceRoutes mounts the browser-direct self-service billing
// surface on the PUBLIC API mux under /v1/me/*, authenticated by a delegated
// customer principal.
//
// A trusted issuer mints a short-lived, DPoP-bound access token (scope
// openrails:self) for the signed-in end user; the browser calls OpenRails
// directly with it. Every operation is scoped to the token's subject and
// resolved merchant. IDENTITY IS HOST-PLUGGABLE (#339): a host-supplied
// billingauth.DelegatedAuthenticator overrides the trusted issuers.
func (s *Server) registerSelfServiceRoutes(mux router.Registrar) {
	delegatedMW := s.delegatedMiddleware()
	providerRoutes := embedhttp.ProviderRoutesForRuntime(s.runtime, nil)

	// Browser tier (#765): self-service is the delegated browser-direct
	// surface, so its patterns feed browserTierRoutes for the static
	// permissive CORS policy.
	httproutes.RegisterSelfServiceRoutes(
		router.NewMuxRecorded(mux, StandaloneV1Prefix+httproutes.SelfRoutePrefix, s.runtime, s.recordBrowserRoute),
		s.runtime, delegatedMW, providerRoutes)

	log.WithField("prefix", StandaloneV1Prefix+httproutes.SelfRoutePrefix).
		Info("delegated self-service API routes registered on public handler")
}

// delegatedMiddleware picks the customer-identity middleware for the
// self-service surface (#339): the host-supplied DelegatedAuthenticator when
// present (an explicit override), else trusted issuers' openrails:self
// access tokens (#1140).
func (s *Server) delegatedMiddleware() router.Middleware {
	if s.delegatedAuthenticator != nil {
		return middleware.DelegatedPrincipalRequired(s.delegatedAuthenticator)
	}
	resolver := middleware.ResourceCustomerResolver(s.controlPlane)
	if s.customerResolver != nil {
		resolver = s.customerResolver
	}
	return middleware.ResourceCustomerRequired(resolver)
}
