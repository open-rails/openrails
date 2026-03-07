package server

import (
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/handlers"
	"github.com/open-rails/openrails/internal/middleware"
)

func (s *Server) registerServiceRoutesAt(v1 *gin.RouterGroup) {
	users := v1.Group("/users/:user_id")
	{
		users.GET("/entitlements", s.wrap(handlers.ServiceGetUserEntitlements))
		users.GET("/credits", s.wrap(handlers.ServiceGetUserCredits))
	}

	credits := v1.Group("/credits")
	{
		credits.POST("/deposit", s.wrap(handlers.ServiceDepositCredits))
		credits.POST("/withdraw", s.wrap(handlers.ServiceWithdrawCredits))
		credits.POST("/hold", s.wrap(handlers.ServiceHoldCredits))
		// Aliases: pluralized holds paths (preferred in docs)
		credits.POST("/holds/:id/capture", s.wrap(handlers.ServiceCaptureHold))
		credits.POST("/holds/:id/release", s.wrap(handlers.ServiceReleaseHold))
		credits.POST("/hold/:id/capture", s.wrap(handlers.ServiceCaptureHold))
		credits.POST("/hold/:id/release", s.wrap(handlers.ServiceReleaseHold))
		credits.GET("/users/:user_id", s.wrap(handlers.ServiceGetUserCredits))
	}

	creditTypes := v1.Group("/credit-types")
	{
		creditTypes.POST("", s.wrap(handlers.ServiceCreateCreditType))
		creditTypes.GET("", s.wrap(handlers.ServiceListCreditTypes))
		creditTypes.PATCH("/:name", s.wrap(handlers.ServiceUpdateCreditType))
		creditTypes.POST("/:name/deactivate", s.wrap(handlers.ServiceDeactivateCreditType))
		creditTypes.POST("/:name/activate", s.wrap(handlers.ServiceActivateCreditType))
	}

	catalog := v1.Group("/catalog")
	{
		products := catalog.Group("/products")
		products.POST("", s.wrap(handlers.ServiceCreateProduct))
		products.PATCH("/:id", s.wrap(handlers.ServiceUpdateProduct))

		prices := catalog.Group("/prices")
		prices.POST("", s.wrap(handlers.ServiceCreatePrice))
		prices.PATCH("/:id", s.wrap(handlers.ServiceUpdatePrice))
	}
}

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
	v1 := s.privateHandler.Group(StandaloneV1Prefix)
	v1.Use(middleware.APIKeyRequired(apiKey))
	s.registerServiceRoutesAt(v1)

	log.Info("Service API routes registered on private handler")
}
