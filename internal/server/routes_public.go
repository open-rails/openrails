package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/doujins-org/doujins-billing/internal/handlers"
)

func (s *Server) registerPublicRoutes() {
	// Root: simple JSON banner for API servers
	s.publicHandler.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service":   "billing",
			"status":    "ok",
			"endpoints": []string{"/health/live", "/health/ready", "/v1"},
		})
	})

	api := s.publicHandler.Group("/v1")

	// Products and Prices - public catalog endpoints
	api.GET("/products", s.authProvider.Optional(), s.wrap(handlers.GetProducts))
	api.GET("/prices", s.authProvider.Optional(), s.wrap(handlers.GetPrices))

	// Webhooks - single provider path (mobius/ccbill/solana)
	webhooks := api.Group("/webhooks")
	webhooks.POST("/:provider", s.wrap(handlers.Webhook))

	// Solana tokens endpoint (public, no auth required)
	api.GET("/solana/tokens", s.wrap(handlers.GetSupportedTokens))

	// Solana Pay - simplified Transfer Request flow with Redis-backed pending payments
	// POST /v1/solana/pay - Create a new Solana Pay Transfer Request URL
	// GET /v1/solana/pay/:reference - Check payment status by reference
	solanaPay := api.Group("/solana/pay")
	solanaPay.Use(s.authProvider.Required())
	solanaPay.POST("", s.wrap(handlers.CreateSolanaPay))
	solanaPay.GET("/:reference", s.wrap(handlers.GetSolanaPayByReference))

	me := api.Group("/me")
	me.Use(s.authProvider.Required())
	me.GET("/status", s.wrap(handlers.GetMyBillingStatus))
	// Unified checkout endpoint - handles both subscriptions and one-time purchases
	me.POST("/checkout", s.wrap(handlers.Checkout))
	// New user-scoped endpoints
	me.GET("/subscriptions", s.wrap(handlers.GetMySubscriptions))
	me.PUT("/subscriptions/payment-method", s.wrap(handlers.UpdateSubscriptionPaymentMethod))
	me.POST("/subscriptions/cancel", s.wrap(handlers.CancelSubscription))
	me.POST("/subscriptions/resume", s.wrap(handlers.ResumeSubscription))
	me.POST("/subscriptions/change", s.wrap(handlers.ChangeSubscription))
	me.GET("/payments", s.wrap(handlers.GetUserPayments))
	me.GET("/payment-methods", s.wrap(handlers.ListPaymentMethods))
	me.POST("/payment-methods", s.wrap(handlers.CreatePaymentMethod))
	me.PUT("/payment-methods/:id", s.wrap(handlers.UpdatePaymentMethod))
	me.DELETE("/payment-methods/:id", s.wrap(handlers.DeletePaymentMethod))
	me.PUT("/payment-methods/:id/activate", s.wrap(handlers.ActivatePaymentMethod))
	me.GET("/notifications", s.wrap(handlers.GetNotifications))
	me.GET("/notifications/unread-count", s.wrap(handlers.GetUnreadNotificationCount))
	me.POST("/notifications/:id/read", s.wrap(handlers.MarkNotificationRead))
	me.GET("/credits", s.wrap(handlers.GetMyCredits))
	me.GET("/credits/:type", s.wrap(handlers.GetMyCreditsType))
	me.GET("/credits/:type/transactions", s.wrap(handlers.GetMyCreditTransactions))
	me.POST("/portal", s.wrap(handlers.CreatePortalSession))

	s.publicHandler.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "billing"})
	})

	s.publicHandler.GET("/health/ready", s.readyHandler)

	// Kubernetes-style health check endpoints (aliases)
	s.publicHandler.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "billing"})
	})
	s.publicHandler.GET("/readyz", s.readyHandler)
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
