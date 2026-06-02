package routes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/authprovider"
)

// ServiceRoutePrefix is the path under the tenant-scoped public API where
// OAT-authenticated server-to-server billing operations live (issue #222). It
// REPLACES the retired private/mTLS service listener: machine callers present an
// OpenRails-issued tenant OAT as a Bearer token against this public surface. The
// acting tenant is bound by the OAT's owning org, not the URL.
const ServiceRoutePrefix = "/service"

type Options struct {
	AuthProvider authprovider.Provider

	// OperatorPermissionChecker, when set, makes the operator org the LIVE
	// authority for admin routes (#224): after the operator-org gate, admin
	// routes additionally require the openrails:admin permission evaluated live
	// against the operator org. nil in verifier-only mode (legacy gate only).
	OperatorPermissionChecker authpolicy.OperatorPermissionChecker
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
	// #224: when the OpenRails-owned AuthKit control plane is present, the
	// operator org is the LIVE authority — require the openrails:admin permission
	// evaluated against the operator org at request time. This is the forward
	// path away from the global-admin fallback.
	if opts.OperatorPermissionChecker != nil {
		group.Use(authpolicy.OperatorPermissionRequired(opts.OperatorPermissionChecker, controlplane.PermAdmin))
	}

	group.GET("/subscriptions", wrap(httphandlers.GetAdminSubscriptions))
	group.GET("/subscriptions/:id", wrap(httphandlers.GetAdminSubscription))
	group.POST("/subscriptions/:id/cancel", wrap(httphandlers.AdminCancelSubscription))

	group.GET("/payments", wrap(httphandlers.GetAdminPayments))
	group.GET("/payments/:id", wrap(httphandlers.GetAdminPayment))
	group.POST("/payments/:id/refund", wrap(httphandlers.AdminRefundPayment))
	group.GET("/users/:user_id/payments", wrap(httphandlers.GetAdminUserPayments))
	group.POST("/users/:user_id/payments/off-channel", wrap(httphandlers.AdminCreateOffChannelPayment))
	group.GET("/repair-alerts", wrap(httphandlers.GetAdminRepairAlerts))
	group.GET("/manual-rebill-attempts", wrap(httphandlers.GetAdminManualRebillAttempts))

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

// RegisterServiceRoutes mounts the server-to-server billing surface on a
// tenant-scoped PUBLIC route group authenticated by OpenRails-issued tenant OATs
// (issue #222). This replaces the retired private/mTLS listener and its
// certificate-scope model: every operation is gated by an OAT permission from the
// canonical colon-form OpenRails permission catalog, and the acting tenant is the
// OAT's owning tenant (pinned by OATRequired before any tenant-owned DB access).
//
// oatMW must authenticate the OAT (typically middleware.OATRequired); it pins the
// resolved tenant + permissions onto the context that the per-route
// RequireOATPermission gates and the handlers read.
func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, oatMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(oatMW)

	users := group.Group("/users/:user_id")
	users.GET("/entitlements", middleware.RequireOATPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserEntitlements))
	users.GET("/credits", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetUserCredits))

	credits := group.Group("/credits")
	credits.POST("/deposit", middleware.RequireOATPermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceDepositCredits))
	credits.POST("/withdraw", middleware.RequireOATPermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceWithdrawCredits))
	credits.POST("/hold", middleware.RequireOATPermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceHoldCredits))
	credits.POST("/holds/:id/capture", middleware.RequireOATPermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/holds/:id/release", middleware.RequireOATPermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceReleaseHold))
	credits.POST("/hold/:id/capture", middleware.RequireOATPermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/hold/:id/release", middleware.RequireOATPermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceReleaseHold))
	credits.GET("/transactions/lookup", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceLookupCreditTransaction))
	credits.GET("/users/:user_id", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetUserCredits))

	// Credit-type definition writes are catalog-definition operations: gate them
	// behind the explicit catalog-write permission (issue #222 — catalog/definition
	// writes available to OATs only under an explicit permission). Reads use the
	// coarse credits:read capability.
	creditTypes := group.Group("/credit-types")
	creditTypes.POST("", middleware.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceCreateCreditType))
	creditTypes.GET("", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceListCreditTypes))
	creditTypes.PATCH("/:name", middleware.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceUpdateCreditType))
	creditTypes.POST("/:name/deactivate", middleware.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceDeactivateCreditType))
	creditTypes.POST("/:name/activate", middleware.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceActivateCreditType))
}
