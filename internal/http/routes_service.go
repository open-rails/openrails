package server

import (
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/middleware"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerServiceRoutes sets up routes on the private/service API.
// These endpoints are authenticated via X-API-KEY header and are intended
// for server-to-server communication only (e.g., AuthKit fetching entitlements).
// This API runs on a separate port (default 8060) that should NOT be exposed
// to the public internet.
func (s *Server) registerServiceRoutes() {
	apiKey := strings.TrimSpace(s.cfg.APIKey)
	if apiKey == "" {
		log.Warn("API key not configured; service API endpoints will be disabled")
		// Still set up a health endpoint for the private server
		s.privateHandler.GET("/health", s.serviceHealth)
		return
	}

	// Health check (no auth required)
	s.privateHandler.GET("/health", s.serviceHealth)

	// Private API v1 routes (X-API-KEY required)
	// No /internal or /service prefix needed - the separate port (8060) is the boundary
	v1 := s.privateHandler.Group(StandaloneV1Prefix)
	httproutes.RegisterServiceRoutes(v1, s.runtime, middleware.APIKeyRequired(apiKey))

	log.Info("Service API routes registered on private handler")
}

func (s *Server) serviceHealth(c *gin.Context) {
	c.JSON(200, gin.H{"status": "ok", "api": "service"})
}
