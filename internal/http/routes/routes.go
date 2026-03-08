package routes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/handlers"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/authprovider"
)

type Options struct {
	AuthProvider authprovider.Provider
}

func wrapHandler(rt *app.Runtime, fn func(r *httprequest.Request)) gin.HandlerFunc {
	return func(c *gin.Context) {
		fn(httprequest.New(c, rt))
	}
}

func RegisterUserRoutes(group *gin.RouterGroup, rt *app.Runtime, opts Options) {
	if opts.AuthProvider == nil {
		panic("AuthProvider is required for user routes")
	}

	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.GET("/products", opts.AuthProvider.Optional(), wrap(httphandlers.GetProducts))
	group.GET("/prices", opts.AuthProvider.Optional(), wrap(httphandlers.GetPrices))
	group.GET("/solana/tokens", wrap(httphandlers.GetSupportedTokens))

	checkout := group.Group("/checkout")
	checkout.Use(opts.AuthProvider.Required())
	checkout.POST("", wrap(handlers.CreateCheckoutSession))
	checkout.GET("/:id", wrap(handlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", wrap(handlers.ConfirmCheckoutSession))

	group.GET("/checkout/:id/solana-pay", wrap(handlers.GetSolanaPay))
	group.POST("/checkout/:id/solana-pay", wrap(handlers.PostSolanaPay))

	me := group.Group("/me")
	me.Use(opts.AuthProvider.Required())
	me.GET("/status", wrap(httphandlers.GetMyBillingStatus))
	me.GET("/subscriptions", wrap(httphandlers.GetMySubscriptions))
	me.GET("/subscriptions/:id", wrap(httphandlers.GetSubscription))
	me.PUT("/subscriptions/:id/payment-method", wrap(handlers.UpdateSubscriptionPaymentMethod))
	me.POST("/subscriptions/:id/cancel", wrap(httphandlers.CancelSubscription))
	me.POST("/subscriptions/:id/resume", wrap(httphandlers.ResumeSubscription))
	me.POST("/subscriptions/:id/change-tier", wrap(httphandlers.ChangeTier))
	me.GET("/payments", wrap(httphandlers.GetUserPayments))
	me.GET("/payment-methods", wrap(handlers.ListPaymentMethods))
	me.POST("/payment-methods", wrap(handlers.CreatePaymentMethod))
	me.PUT("/payment-methods/:id", wrap(handlers.UpdatePaymentMethod))
	me.DELETE("/payment-methods/:id", wrap(handlers.DeletePaymentMethod))
	me.GET("/notifications", wrap(httphandlers.GetNotifications))
	me.GET("/notifications/unread-count", wrap(httphandlers.GetUnreadNotificationCount))
	me.POST("/notifications/:id/read", wrap(httphandlers.MarkNotificationRead))
	me.GET("/credits", wrap(httphandlers.GetMyCredits))
	me.GET("/credits/:type", wrap(httphandlers.GetMyCreditsType))
	me.GET("/credits/:type/transactions", wrap(httphandlers.GetMyCreditTransactions))

	stripe := group.Group("/stripe")
	stripe.Use(opts.AuthProvider.Required())
	stripe.POST("/portal", wrap(httphandlers.CreatePortalSession))
}

func RegisterAdminRoutes(group *gin.RouterGroup, rt *app.Runtime, opts Options) {
	if opts.AuthProvider == nil {
		panic("AuthProvider is required for admin routes")
	}

	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(opts.AuthProvider.Required())
	group.Use(authpolicy.AdminRequired(rt.DB.GetDB()))

	group.GET("/subscriptions", wrap(handlers.GetAdminSubscriptions))
	group.GET("/subscriptions/:id", wrap(handlers.GetAdminSubscription))
	group.POST("/subscriptions/:id/cancel", wrap(handlers.AdminCancelSubscription))

	group.GET("/payments", wrap(handlers.GetAdminPayments))
	group.GET("/payments/:id", wrap(handlers.GetAdminPayment))
	group.POST("/payments/:id/refund", wrap(handlers.AdminRefundPayment))
	group.GET("/users/:user_id/payments", wrap(handlers.GetAdminUserPayments))
	group.POST("/users/:user_id/payments/off-channel", wrap(handlers.AdminCreateOffChannelPayment))

	group.GET("/users/:user_id", wrap(handlers.GetAdminUserBillingProfile))
	group.GET("/users/:user_id/entitlements", wrap(handlers.GetAdminUserEntitlements))
	group.GET("/users/:user_id/mobius", wrap(handlers.GetAdminUserMobius))
	group.GET("/users/:user_id/mobius/metrics", wrap(handlers.GetAdminUserMobiusMetrics))
	group.GET("/users/:user_id/ccbill", wrap(handlers.GetAdminUserCCBill))
	group.GET("/users/:user_id/ccbill/metrics", wrap(handlers.GetAdminUserCCBillMetrics))
	group.POST("/users/:user_id/entitlements", wrap(handlers.GrantAdminEntitlement))
	group.DELETE("/users/:user_id/entitlements/:id", wrap(handlers.RevokeAdminEntitlement))

	group.GET("/metrics/summary", wrap(httphandlers.GetAdminMetricsSummary))
	group.GET("/metrics/revenue", wrap(httphandlers.GetAdminMetricsRevenue))
	group.GET("/metrics/subscriptions", wrap(httphandlers.GetAdminMetricsSubscriptions))
	group.GET("/metrics/processors", wrap(httphandlers.GetAdminMetricsProcessors))
	group.GET("/metrics/churn", wrap(httphandlers.GetAdminMetricsChurn))
}

func RegisterWebhookRoutes(group *gin.RouterGroup, rt *app.Runtime) {
	group.POST(":provider", wrapHandler(rt, handlers.Webhook))
}

func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, authMiddleware gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(authMiddleware)

	users := group.Group("/users/:user_id")
	users.GET("/entitlements", wrap(handlers.ServiceGetUserEntitlements))
	users.GET("/credits", wrap(httphandlers.ServiceGetUserCredits))

	credits := group.Group("/credits")
	credits.POST("/deposit", wrap(httphandlers.ServiceDepositCredits))
	credits.POST("/withdraw", wrap(httphandlers.ServiceWithdrawCredits))
	credits.POST("/hold", wrap(httphandlers.ServiceHoldCredits))
	credits.POST("/holds/:id/capture", wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/holds/:id/release", wrap(httphandlers.ServiceReleaseHold))
	credits.POST("/hold/:id/capture", wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/hold/:id/release", wrap(httphandlers.ServiceReleaseHold))
	credits.GET("/transactions/lookup", wrap(httphandlers.ServiceLookupCreditTransaction))
	credits.GET("/users/:user_id", wrap(httphandlers.ServiceGetUserCredits))

	creditTypes := group.Group("/credit-types")
	creditTypes.POST("", wrap(httphandlers.ServiceCreateCreditType))
	creditTypes.GET("", wrap(httphandlers.ServiceListCreditTypes))
	creditTypes.PATCH("/:name", wrap(httphandlers.ServiceUpdateCreditType))
	creditTypes.POST("/:name/deactivate", wrap(httphandlers.ServiceDeactivateCreditType))
	creditTypes.POST("/:name/activate", wrap(httphandlers.ServiceActivateCreditType))

	catalog := group.Group("/catalog")
	products := catalog.Group("/products")
	products.POST("", wrap(httphandlers.ServiceCreateProduct))
	products.PATCH("/:id", wrap(httphandlers.ServiceUpdateProduct))

	prices := catalog.Group("/prices")
	prices.POST("", wrap(httphandlers.ServiceCreatePrice))
	prices.PATCH("/:id", wrap(httphandlers.ServiceUpdatePrice))
}
