package server

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerSelfServiceRoutes mounts the browser-direct self-service billing
// surface on the PUBLIC API engine under /v1/self/*, authenticated by a
// browser's DELEGATED ACCESS TOKEN (issue #222 browser-tier foundation, the
// backend prerequisite for doujins #253 / hentai0 #142 / cozy-art #46).
//
// A tenant's host frontend mints a short-lived AuthKit delegated access token
// (aud=openrails, tenant, delegated_sub, openrails:self:* perms) for the
// logged-in end-user; the browser calls OpenRails directly with it. Every
// operation is scoped to the token's delegated_sub + resolved tenant.
//
// Mounted only when the OpenRails-owned AuthKit control plane is configured (it
// owns the delegated-token verifier). In verifier-only mode there is no delegated
// issuer and the self-service surface is not mounted.
func (s *Server) registerSelfServiceRoutes(e *gin.Engine) {
	if s.controlPlane == nil {
		log.Info("control plane disabled; delegated self-service API routes will not be mounted")
		return
	}

	group := e.Group(StandaloneV1Prefix + httproutes.SelfRoutePrefix)
	httproutes.RegisterSelfServiceRoutes(group, s.runtime, ginmw.DelegatedSelfRequired(s.controlPlane))

	log.WithField("prefix", StandaloneV1Prefix+httproutes.SelfRoutePrefix).
		Info("delegated self-service API routes registered on public handler")

	// Browser-direct TENANT-ADMIN surface (issue #259): the SAME delegated-token
	// middleware authenticates; per-route gates require `openrails:tenant:*`
	// permissions and the handlers act on a `:user_id` WITHIN the token's pinned
	// tenant. Mounted on the same public engine alongside /v1/self/*.
	adminGroup := e.Group(StandaloneV1Prefix + httproutes.TenantAdminRoutePrefix)
	httproutes.RegisterTenantAdminRoutes(adminGroup, s.runtime, ginmw.DelegatedSelfRequired(s.controlPlane))

	log.WithField("prefix", StandaloneV1Prefix+httproutes.TenantAdminRoutePrefix).
		Info("delegated tenant-admin API routes registered on public handler")
}
