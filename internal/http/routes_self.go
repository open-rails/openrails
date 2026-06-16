package server

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
)

// registerSelfServiceRoutes mounts the browser-direct self-service billing
// surface on the PUBLIC API engine under /v1/self/*, authenticated by a
// browser's DELEGATED ACCESS TOKEN (issue #222 browser-tier foundation, the
// backend prerequisite for browser-direct self-service consumers.
//
// A merchant's host frontend mints a short-lived AuthKit delegated access token
// (aud=openrails, tenant, delegated_sub, openrails:self:* perms) for the
// logged-in end-user; the browser calls OpenRails directly with it. Every
// operation is scoped to the token's delegated_sub + resolved tenant.
//
// The surface is ALWAYS mounted (#469): the OpenRails-owned AuthKit control
// plane is the default delegated-token verifier. IDENTITY IS HOST-PLUGGABLE
// (issue #339): a host-supplied billingauth.DelegatedAuthenticator overrides
// the control-plane verifier — a host running OpenRails as a subsystem
// verifies its own credential and supplies the explicitly mapped principal,
// one system, one credential.
func (s *Server) registerSelfServiceRoutes(e *gin.Engine) {
	delegatedMW := s.delegatedMiddleware()

	group := e.Group(StandaloneV1Prefix + httproutes.SelfRoutePrefix)
	httproutes.RegisterSelfServiceRoutes(group, s.runtime, delegatedMW)

	log.WithField("prefix", StandaloneV1Prefix+httproutes.SelfRoutePrefix).
		Info("delegated self-service API routes registered on public handler")

	// Browser-direct MERCHANT-ADMIN surface (issue #259): the SAME delegated
	// middleware authenticates; per-route gates require `openrails:merchant:*`
	// permissions and the handlers act on a `:user_id` WITHIN the token's pinned
	// tenant. Mounted on the same public engine alongside /v1/self/*.
	adminGroup := e.Group(StandaloneV1Prefix + httproutes.MerchantAdminRoutePrefix)
	httproutes.RegisterMerchantAdminRoutes(adminGroup, s.runtime, delegatedMW)

	log.WithField("prefix", StandaloneV1Prefix+httproutes.MerchantAdminRoutePrefix).
		Info("delegated merchant-admin API routes registered on public handler")
}

// delegatedMiddleware picks the delegated-identity middleware for the
// self-service + merchant-admin surfaces (#339): the host-supplied
// DelegatedAuthenticator when present (an explicit override), else the
// control plane's delegated-token verifier (always available, #469).
func (s *Server) delegatedMiddleware() gin.HandlerFunc {
	if s.delegatedAuthenticator != nil {
		return ginmw.DelegatedPrincipalRequired(s.delegatedAuthenticator)
	}
	return ginmw.DelegatedSelfRequired(s.controlPlane)
}
