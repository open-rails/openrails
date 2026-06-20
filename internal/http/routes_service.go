package server

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
)

// registerServiceRoutes mounts the server-to-server billing surface on the PUBLIC
// API engine under /v1/service/*, authenticated by OpenRails-issued merchant API keys
// (issue #222). This REPLACES the retired private/mTLS listener entirely: there is
// no separate trust surface or port — machine callers present an API key as a Bearer
// token on the one public tenant API.
//
// The OpenRails-owned AuthKit control plane is what resolves and authorizes
// API keys; it is always present on this surface (#469).
func (s *Server) registerServiceRoutes(e *gin.Engine) {
	group := e.Group(StandaloneV1Prefix + httproutes.ServiceRoutePrefix)
	httproutes.RegisterServiceRoutes(group, s.runtime, ginmw.ServiceCredentialRequired(s.controlPlane))

	log.WithField("prefix", StandaloneV1Prefix+httproutes.ServiceRoutePrefix).
		Info("API-key-authenticated service API routes registered on public handler")
}
