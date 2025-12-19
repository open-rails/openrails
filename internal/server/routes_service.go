package server

import (
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/doujins-org/doujins-billing/internal/handlers"
	"github.com/doujins-org/doujins-billing/internal/middleware"
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
		s.privateHandler.GET("/health", s.wrap(func(r *handlers.Request) {
			r.SuccessJSON(map[string]string{"status": "ok", "api": "service"})
		}))
		return
	}

	// Health check (no auth required)
	s.privateHandler.GET("/health", s.wrap(func(r *handlers.Request) {
		r.SuccessJSON(map[string]string{"status": "ok", "api": "service"})
	}))

	// Private API v1 routes (X-API-KEY required)
	// No /internal or /service prefix needed - the separate port (8060) is the boundary
	v1 := s.privateHandler.Group("/v1")
	v1.Use(middleware.APIKeyRequired(apiKey))
	{
		users := v1.Group("/users/:user_id")
		{
			// GET /v1/users/:user_id/entitlements - Get user's active entitlements
			users.GET("/entitlements", s.wrap(handlers.ServiceGetUserEntitlements))
			// GET /v1/users/:user_id/credits - Get user's credit balance
			users.GET("/credits", s.wrap(handlers.ServiceGetUserCredits))
		}

		credits := v1.Group("/credits")
		{
			credits.POST("/withdraw", s.wrap(handlers.ServiceWithdrawCredits))
			credits.POST("/hold", s.wrap(handlers.ServiceHoldCredits))
			credits.POST("/hold/:id/capture", s.wrap(handlers.ServiceCaptureHold))
			credits.POST("/hold/:id/release", s.wrap(handlers.ServiceReleaseHold))
		}
	}

	log.Info("Service API routes registered on private handler")
}
