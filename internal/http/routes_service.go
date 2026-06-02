package server

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/middleware"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerServiceRoutes sets up routes on the mTLS service API.
// These endpoints are authenticated by verified client certificates and are
// intended for service-to-service communication only.
func (s *Server) registerServiceRoutes() {
	if !s.cfg.ServiceMTLS.Enabled {
		log.Info("Service mTLS listener disabled; service API routes will not be mounted")
		return
	}

	// No /internal or /service prefix needed: the dedicated mTLS listener is the boundary.
	v1 := s.privateHandler.Group(StandaloneV1Prefix)
	httproutes.RegisterServiceRoutes(v1, s.runtime, middleware.ServiceMTLSRequired(s.cfg.ServiceMTLS.ClientScopes()))

	log.Info("Service API routes registered on mTLS handler")
}

func (s *Server) serviceHealth(c *gin.Context) {
	c.JSON(200, gin.H{"status": "ok", "api": "service"})
}
