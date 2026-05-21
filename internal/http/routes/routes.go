package routes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
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
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.GET("/products", opts.AuthProvider.Optional(), wrap(httphandlers.GetProducts))
	group.GET("/prices", opts.AuthProvider.Optional(), wrap(httphandlers.GetPrices))
	group.GET("/solana/tokens", wrap(httphandlers.GetSupportedTokens))

	checkout := group.Group("/checkout")
	checkout.Use(opts.AuthProvider.Required())
	checkout.POST("", wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", wrap(httphandlers.ConfirmCheckoutSession))

	group.GET("/checkout/:id/solana-pay", wrap(httphandlers.GetSolanaPay))
	group.POST("/checkout/:id/solana-pay", wrap(httphandlers.PostSolanaPay))

	me := group.Group("/me")
	me.Use(opts.AuthProvider.Required())
	me.GET("/status", wrap(httphandlers.GetMyBillingStatus))
	me.GET("/subscriptions", wrap(httphandlers.GetMySubscriptions))
	me.GET("/subscriptions/:id", wrap(httphandlers.GetSubscription))
	me.PUT("/subscriptions/:id/payment-method", wrap(httphandlers.UpdateSubscriptionPaymentMethod))
	me.POST("/subscriptions/:id/cancel", wrap(httphandlers.CancelSubscription))
	me.POST("/subscriptions/:id/resume", wrap(httphandlers.ResumeSubscription))
	me.POST("/subscriptions/:id/change-tier", wrap(httphandlers.ChangeTier))
	me.GET("/payments", wrap(httphandlers.GetUserPayments))
	me.GET("/payment-methods", wrap(httphandlers.ListPaymentMethods))
	me.POST("/payment-methods", wrap(httphandlers.CreatePaymentMethod))
	me.PUT("/payment-methods/:id", wrap(httphandlers.UpdatePaymentMethod))
	me.DELETE("/payment-methods/:id", wrap(httphandlers.DeletePaymentMethod))
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
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(opts.AuthProvider.Required())
	group.Use(authpolicy.OperatorAdminRequired(rt.Config, rt.DB.GetDB()))

	group.GET("/subscriptions", wrap(httphandlers.GetAdminSubscriptions))
	group.GET("/subscriptions/:id", wrap(httphandlers.GetAdminSubscription))
	group.POST("/subscriptions/:id/cancel", wrap(httphandlers.AdminCancelSubscription))

	group.GET("/payments", wrap(httphandlers.GetAdminPayments))
	group.GET("/payments/:id", wrap(httphandlers.GetAdminPayment))
	group.POST("/payments/:id/refund", wrap(httphandlers.AdminRefundPayment))
	group.GET("/users/:user_id/payments", wrap(httphandlers.GetAdminUserPayments))
	group.POST("/users/:user_id/payments/off-channel", wrap(httphandlers.AdminCreateOffChannelPayment))

	group.GET("/users/:user_id", wrap(httphandlers.GetAdminUserBillingProfile))
	group.GET("/users/:user_id/entitlements", wrap(httphandlers.GetAdminUserEntitlements))
	group.GET("/users/:user_id/nmi", wrap(httphandlers.GetAdminUserNMI))
	group.GET("/users/:user_id/nmi/metrics", wrap(httphandlers.GetAdminUserNMIMetrics))
	group.GET("/users/:user_id/ccbill", wrap(httphandlers.GetAdminUserCCBill))
	group.GET("/users/:user_id/ccbill/metrics", wrap(httphandlers.GetAdminUserCCBillMetrics))
	group.POST("/users/:user_id/entitlements", wrap(httphandlers.GrantAdminEntitlement))
	group.DELETE("/users/:user_id/entitlements/:id", wrap(httphandlers.RevokeAdminEntitlement))

	// Admin catalog API (issue #205). Mounted alongside subscriptions/payments/users.
	// Symmetric with pkg/service facade; embedded hosts may call the facade directly.
	adminCatalog := group.Group("/catalog")
	adminProducts := adminCatalog.Group("/products")
	adminProducts.POST("", wrap(httphandlers.AdminCreateProduct))
	adminProducts.GET("", wrap(httphandlers.AdminListProducts))
	adminProducts.GET("/:id", wrap(httphandlers.AdminGetProduct))
	adminProducts.GET("/by-slug/:slug", wrap(httphandlers.AdminGetProductBySlug))
	adminProducts.PATCH("/:id", wrap(httphandlers.AdminUpdateProduct))
	adminProducts.POST("/:id/activate", wrap(httphandlers.AdminActivateProduct))
	adminProducts.POST("/:id/deactivate", wrap(httphandlers.AdminDeactivateProduct))
	adminProducts.POST("/:id/reconcile", wrap(httphandlers.AdminReconcileProduct))

	adminPrices := adminCatalog.Group("/prices")
	adminPrices.POST("", wrap(httphandlers.AdminCreatePrice))
	adminPrices.GET("", wrap(httphandlers.AdminListPrices))
	adminPrices.GET("/:id", wrap(httphandlers.AdminGetPrice))
	adminPrices.PATCH("/:id", wrap(httphandlers.AdminUpdatePrice))
	adminPrices.POST("/:id/activate", wrap(httphandlers.AdminActivatePrice))
	adminPrices.POST("/:id/deactivate", wrap(httphandlers.AdminDeactivatePrice))
	adminPrices.POST("/:id/reconcile", wrap(httphandlers.AdminReconcilePrice))

	// Catalog reconciliation loop drift surface (issue #209). Alert-only:
	// these endpoints never mutate Stripe, NMI, or the catalog rows. Operators
	// resolve drift via the per-price reconcile action above. CCBill is never
	// reconciled (no catalog-list API), so it never appears in these surfaces.
	adminCatalog.GET("/drift", wrap(httphandlers.AdminListCatalogDrift))
	adminCatalog.POST("/drift/refresh", wrap(httphandlers.AdminRefreshCatalogDrift))
	// reconcile-all is the spec-named alias for an on-demand synchronous pull.
	adminCatalog.POST("/drift/reconcile-all", wrap(httphandlers.AdminRefreshCatalogDrift))
	// /orphans is provider-filterable (?provider=stripe|nmi); /stripe/orphans is
	// the operator-friendly convenience alias scoped to Stripe.
	adminCatalog.GET("/orphans", wrap(httphandlers.AdminListCatalogOrphans))
	adminCatalog.GET("/stripe/orphans", wrap(httphandlers.AdminListStripeOrphans))

	group.GET("/metrics/summary", wrap(httphandlers.GetAdminMetricsSummary))
	group.GET("/metrics/revenue", wrap(httphandlers.GetAdminMetricsRevenue))
	group.GET("/metrics/subscriptions", wrap(httphandlers.GetAdminMetricsSubscriptions))
	group.GET("/metrics/processors", wrap(httphandlers.GetAdminMetricsProcessors))
	group.GET("/metrics/churn", wrap(httphandlers.GetAdminMetricsChurn))
}

func RegisterWebhookRoutes(group *gin.RouterGroup, rt *app.Runtime) {
	group.POST(":provider", wrapHandler(rt, httphandlers.Webhook))
}

func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, authMiddleware gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(authMiddleware)

	users := group.Group("/users/:user_id")
	users.GET("/entitlements", wrap(httphandlers.ServiceGetUserEntitlements))
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

}
