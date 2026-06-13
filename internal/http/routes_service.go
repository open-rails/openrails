package server

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
)

// registerServiceRoutes mounts the server-to-server billing surface on the PUBLIC
// API engine under /v1/service/*, authenticated by OpenRails-issued tenant service tokens
// (issue #222). This REPLACES the retired private/mTLS listener entirely: there is
// no separate trust surface or port — machine callers present a service token as a Bearer
// token on the one public tenant API.
//
// The OpenRails-owned AuthKit control plane is what resolves and authorizes
// service tokens; it is always present on this surface (#469).
func (s *Server) registerServiceRoutes(e *gin.Engine) {
	group := e.Group(StandaloneV1Prefix + httproutes.ServiceRoutePrefix)
	httproutes.RegisterServiceRoutes(group, s.runtime, ginmw.ServiceTokenRequired(s.controlPlane), s.controlPlane)

	log.WithField("prefix", StandaloneV1Prefix+httproutes.ServiceRoutePrefix).
		Info("service token-authenticated service API routes registered on public handler")
}
