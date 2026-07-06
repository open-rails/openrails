package server

import (
	"net/http"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerSelfServiceRoutes mounts the browser-direct self-service billing
// surface on the PUBLIC API mux under /v1/me/* (+ the customer treasury under
// /v1/customers/:customer_id/*), authenticated by a delegated customer
// principal.
//
// A merchant's host frontend mints a short-lived AuthKit delegated access token
// (aud=openrails, merchant issuer, delegated_sub) for the logged-in end-user;
// the browser calls OpenRails directly with it. Every operation is scoped to the
// token's delegated_sub + resolved merchant.
//
// The surface is ALWAYS mounted (#469): the OpenRails-owned AuthKit control
// plane is the default delegated-token verifier. IDENTITY IS HOST-PLUGGABLE
// (issue #339): a host-supplied billingauth.DelegatedAuthenticator overrides
// the control-plane verifier.
func (s *Server) registerSelfServiceRoutes(mux *http.ServeMux) {
	delegatedMW := s.delegatedMiddleware()
	providerRoutes := embedhttp.ProviderRoutesForRuntime(s.runtime, nil)

	// Browser tier (#765): self-service + customer-treasury are the delegated
	// browser-direct surfaces, so their patterns feed browserTierRoutes for the
	// static permissive CORS policy.
	httproutes.RegisterSelfServiceRoutes(
		router.NewMuxRecorded(mux, StandaloneV1Prefix+httproutes.SelfRoutePrefix, s.runtime, s.recordBrowserRoute),
		s.runtime, delegatedMW, providerRoutes)

	var ensurer middleware.CustomerGroupEnsurer
	if s.controlPlane != nil {
		ensurer = s.controlPlane
	}
	httproutes.RegisterCustomerTreasuryRoutes(
		router.NewMuxRecorded(mux, StandaloneV1Prefix+httproutes.CustomerRoutePrefix, s.runtime, s.recordBrowserRoute),
		s.runtime, delegatedMW, providerRoutes,
		middleware.EnsureCustomerPermissionGroup(ensurer))

	log.WithField("prefix", StandaloneV1Prefix+httproutes.SelfRoutePrefix).
		Info("delegated self-service API routes registered on public handler")
	log.WithField("prefix", StandaloneV1Prefix+httproutes.CustomerRoutePrefix).
		Info("delegated customer-treasury API routes registered on public handler")
}

// delegatedMiddleware picks the delegated-identity middleware for the
// self-service surface (#339): the host-supplied DelegatedAuthenticator when
// present (an explicit override), else the control plane's delegated-token
// verifier (always available, #469).
func (s *Server) delegatedMiddleware() router.Middleware {
	if s.delegatedAuthenticator != nil {
		return middleware.DelegatedPrincipalRequired(s.delegatedAuthenticator)
	}
	resolver := middleware.DelegatedResolver(s.controlPlane)
	if s.delegatedResolver != nil {
		resolver = s.delegatedResolver
	}
	return middleware.DelegatedSelfRequired(resolver)
}
