package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

func (s *Server) registerUserRoutesAt(e *gin.Engine, apiPrefix string) {
	api := e.Group(apiPrefix)
	httproutes.RegisterUserRoutes(api, s.runtime, httproutes.Options{AuthProvider: s.authProvider})
}

func (s *Server) registerUserRoutes(e *gin.Engine) {
	s.registerUserRoutesAt(e, StandaloneV1Prefix)
}

func (s *Server) registerWebhookRoutesAt(e *gin.Engine, apiPrefix string) {
	api := e.Group(apiPrefix)
	webhooks := api.Group("/webhooks")
	httproutes.RegisterWebhookRoutes(webhooks, s.runtime)
}

func (s *Server) registerWebhookRoutes(e *gin.Engine) {
	s.registerWebhookRoutesAt(e, StandaloneV1Prefix)
}

// registerStandaloneMetaRoutes registers banner/health endpoints that are appropriate for the
// standalone billing service, but should not be forced onto embedded hosts.
func (s *Server) registerStandaloneMetaRoutes(e *gin.Engine) {
	// Root: simple JSON banner for API servers
	e.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service":   "billing",
			"status":    "ok",
			"endpoints": []string{"/health/live", "/health/ready", StandaloneV1Prefix},
		})
	})

	e.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "billing"})
	})

	e.GET("/health/ready", s.readyHandler)

	// Kubernetes-style health check endpoints (aliases)
	e.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "billing"})
	})
	e.GET("/readyz", s.readyHandler)
}

func (s *Server) readyHandler(c *gin.Context) {
	ctx := c.Request.Context()
	checks := gin.H{
		"db":      "ok",
		"redis":   "ok",
		"authkit": "ok",
	}

	// Check database (critical)
	var one int
	if s.runtime != nil && s.runtime.DB != nil {
		if err := s.runtime.DB.GetDB().NewSelect().ColumnExpr("1").Scan(ctx, &one); err != nil {
			checks["db"] = "down"
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": checks})
			return
		}
	} else {
		checks["db"] = "missing"
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": checks})
		return
	}

	// Check Redis (critical for billing operations)
	if s.runtime != nil && s.runtime.RedisClient != nil {
		if _, err := s.runtime.RedisClient.Ping(ctx).Result(); err != nil {
			checks["redis"] = "down"
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": checks})
			return
		}
	} else {
		checks["redis"] = "missing"
	}

	// Check AuthKit verifier (critical for authentication)
	if s.authProvider == nil {
		checks["authkit"] = "not_initialized"
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": checks})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ready", "checks": checks})
}
