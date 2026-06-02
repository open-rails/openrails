package server

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/middleware"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerServiceRoutes mounts the server-to-server billing surface on the PUBLIC
// API engine under /v1/service/*, authenticated by OpenRails-issued tenant OATs
// (issue #222). This REPLACES the retired private/mTLS listener entirely: there is
// no separate trust surface or port — machine callers present an OAT as a Bearer
// token on the one public tenant API.
//
// Mounted only when the OpenRails-owned AuthKit control plane is configured (it is
// what resolves and authorizes OATs). In verifier-only mode there is no OAT issuer
// and the service surface is not mounted.
func (s *Server) registerServiceRoutes(e *gin.Engine) {
	if s.controlPlane == nil {
		log.Info("control plane disabled; OAT service API routes will not be mounted")
		return
	}

	group := e.Group(StandaloneV1Prefix + httproutes.ServiceRoutePrefix)
	httproutes.RegisterServiceRoutes(group, s.runtime, middleware.OATRequired(s.controlPlane), s.controlPlane, s.controlPlane)

	log.WithField("prefix", StandaloneV1Prefix+httproutes.ServiceRoutePrefix).
		Info("OAT-authenticated service API routes registered on public handler")
}
