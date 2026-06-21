package ginroutes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/request/ginreq"
)

// SelfRoutePrefix is the canonical browser self-service billing surface. The
// credential profile may be a delegated JWT in standalone mode or a host/user
// bearer in embedded mode; the URL is intentionally one stable `/me` surface
// and the credential profile lives on the resolved Principal.
const SelfRoutePrefix = "/me"

// AdminRoutePrefix is the path under the merchant-scoped public API where the
// (delegated) ADMIN billing surface lives (#259, #528). A merchant's host
// frontend mints a FEDERATED, MERCHANT-SIGNED delegated access token carrying
// browser-safe `org:` permissions for one of its ADMIN users; the browser
// presents it as a Bearer token directly against this surface to act on ANY user
// WITHIN the token's merchant via the `:user_id` path param. The acting admin is
// the token's `delegated_sub` (recorded for audit) and the acting merchant is
// pinned from the token's validated issuer — never the URL. (#528 retired the
// per-user admin surface; this delegated surface IS the admin API.)
const AdminRoutePrefix = "/admin"

func wrapHandler(rt *app.Runtime, fn func(r *httprequest.Request)) gin.HandlerFunc {
	return func(c *gin.Context) {
		fn(ginreq.New(c, rt))
	}
}

// RegisterSelfServiceRoutes mounts the browser self-service billing surface on
// a merchant-scoped PUBLIC route group authenticated by a bearer principal. It
// reuses the existing user-facing handlers — they all read the acting user via
// r.GetUser(), which delegatedMW binds from the resolved Principal — so every
// operation is automatically scoped to the authenticated end-user and their
// merchant. There is no `:user_id` in any path: a browser token can only act on
// its own subject.
//
// delegatedMW must authenticate the delegated token (typically
// ginmw.DelegatedSelfRequired); it pins the resolved merchant + acting user +
// self-permissions onto the context that the per-route RequirePermission gates
// and the handlers read.
func RegisterSelfServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// merchant, so RLS constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	// Customer money self-service. Customer routes operate only on the
	// authenticated subject; no self permissions or customer_id path params.
	group.GET("/balance", wrap(httphandlers.GetMyBalance))
	group.GET("/transactions", wrap(httphandlers.GetMyAccountTransactions))
	group.PUT("/settings", wrap(httphandlers.SetMyCreditAccountSettings))
	group.GET("/status", wrap(httphandlers.GetMyBillingStatus))

	// Usage breakdown (issue #289): the acting user's metered usage rolled up by
	// event_type (endpoint/model) over a [from, to) window, with summed dimensions.
	// Scoped to the token's subject + merchant like every other self route.
	group.GET("/usage", wrap(httphandlers.GetMyUsage))

	// Invoices (issue #303): the acting user's finalized monthly itemized
	// statements (newest first, paginated) and a single invoice with its line
	// items. Scoped to the token's subject + merchant like every other self route.
	group.GET("/invoices", wrap(httphandlers.GetMyInvoices))
	group.GET("/invoices/:id", wrap(httphandlers.GetMyInvoice))

	// Payment / transaction history.
	group.GET("/payments", wrap(httphandlers.GetUserPayments))
	group.GET("/entitlements/active", wrap(httphandlers.SelfGetActiveEntitlements))

	group.GET("/notifications", wrap(httphandlers.GetNotifications))
	group.GET("/notifications/unread-count", wrap(httphandlers.GetUnreadNotificationCount))
	group.POST("/notifications/:id/read", wrap(httphandlers.MarkNotificationRead))
	group.GET("/products", wrap(httphandlers.GetMyProducts))
	group.GET("/products/:product_id/access", wrap(httphandlers.GetMyProductAccess))

	// Subscriptions: every operation is scoped to the authenticated subject.
	subs := group.Group("/subscriptions")
	subs.GET("", wrap(httphandlers.GetMySubscriptions))
	subs.GET("/:id", wrap(httphandlers.GetSubscription))
	subs.POST("/:id/cancel", wrap(httphandlers.CancelSubscription))
	subs.POST("/:id/resume", wrap(httphandlers.ResumeSubscription))
	subs.POST("/:id/change-tier", wrap(httphandlers.ChangeTier))
	subs.POST("/:id/change-tier/preview", wrap(httphandlers.ChangeTierPreview))
	subs.PUT("/:id/payment-method", wrap(httphandlers.UpdateSubscriptionPaymentMethod))
	// App-driven on-chain cancel/revoke (#266/#271): the full prepare -> sign ->
	// confirm -> mirror loop. solana-cancel-tx builds the unsigned cancel tx the
	// wallet signs+sends; solana-cancel confirms the signature landed on-chain and
	// then mirrors the cancel into the DB (stops the cranker). Solana is the source
	// of truth — there is no DB-only "soft cancel".
	subs.POST("/:id/solana-cancel-tx", wrap(httphandlers.PrepareSolanaCancelTx))
	subs.POST("/:id/solana-cancel", wrap(httphandlers.ConfirmSolanaCancel))
	// App-driven on-chain tier change (#272): the prepare -> sign -> confirm ->
	// mirror loop for changing tier on an existing Solana subscription. prepare
	// returns the SINGLE ATOMIC cancel-old+subscribe-new tx (co-signed for an
	// upgrade's prorated transfer); confirm verifies it landed on-chain and mirrors
	// the switch into the DB (old cancelled, new active, next_pull_at per kind).
	subs.POST("/:id/solana-tier-change", wrap(httphandlers.PrepareSolanaTierChange))
	subs.POST("/:id/solana-tier-change/confirm", wrap(httphandlers.ConfirmSolanaTierChange))

	// Payment methods.
	pm := group.Group("/payment-methods")
	pm.GET("", wrap(httphandlers.ListPaymentMethods))
	pm.POST("", wrap(httphandlers.CreatePaymentMethod))
	pm.PUT("/:id", wrap(httphandlers.UpdatePaymentMethod))
	pm.DELETE("/:id", wrap(httphandlers.DeletePaymentMethod))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkout := group.Group("/checkout")
	checkout.POST("", wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", wrap(httphandlers.ConfirmCheckoutSession))

	// Stripe customer portal handoff, moved from the old `/stripe/portal` raw
	// user route to the canonical self surface.
	group.POST("/stripe/portal", wrap(httphandlers.CreatePortalSession))
}

// RegisterAdminRoutes mounts the browser-direct delegated admin billing
// surface on a merchant-scoped PUBLIC route group authenticated by a FEDERATED,
// MERCHANT-SIGNED delegated access token carrying browser-safe `org:` permissions
// (issue #259). Unlike the self-service surface (which has no `:user_id` and acts
// only on the token's own subject), these routes act on ANY user WITHIN the
// token's merchant via the `:user_id` path param.
//
// It REUSES the existing operator admin handlers unchanged. They are safe here
// because:
//   - the target user is the `:user_id` path param,
//   - the acting admin is r.GetUser() == the token's `delegated_sub` (so audit
//     fields like EntitlementGrant.GrantedBy record the acting admin),
//   - the merchant is pinned onto the request context by delegatedMW from the
//     token's validated issuer, so every merchant-owned query is scoped to that
//     merchant (RLS-enforced, #227). A `:user_id` belonging to another merchant is
//     therefore unreachable — fail closed, no cross-merchant access.
//
// delegatedMW must authenticate the delegated token (ginmw.DelegatedSelfRequired);
// it pins the resolved merchant + acting admin + merchant-permissions onto the
// context that the per-route RequirePermission gates and handlers read.
func RegisterAdminRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// merchant, so RLS constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	read := ginmw.RequirePermission(controlplane.PermMerchantCustomersRead)
	subRead := ginmw.RequirePermission(controlplane.PermMerchantCustomersRead)
	entWrite := ginmw.RequirePermission(controlplane.PermMerchantCustomersUpdate)
	productAccessWrite := ginmw.RequirePermission(controlplane.PermMerchantCustomersUpdate)
	payWrite := ginmw.RequirePermission(controlplane.PermMerchantCustomersUpdate)
	subWrite := ginmw.RequirePermission(controlplane.PermMerchantSubscriptionsUpdate)
	configRead := ginmw.RequirePermission(controlplane.PermMerchantSettingsRead)
	configWrite := ginmw.RequirePermission(controlplane.PermMerchantSettingsUpdate)
	metricsRead := ginmw.RequirePermission(controlplane.PermMerchantUsageRead)
	secretsList := ginmw.RequirePermission(controlplane.PermMerchantPaymentProvidersRead)
	secretsWrite := ginmw.RequirePermission(controlplane.PermMerchantPaymentProvidersUpdate)
	secretsDelete := ginmw.RequirePermission(controlplane.PermMerchantPaymentProvidersUpdate)
	secretsTest := ginmw.RequirePermission(controlplane.PermMerchantPaymentProvidersUpdate)

	// Merchant metrics (issue #259 + #232): a merchant admin reads THEIR OWN merchant's
	// analytics. The metrics queries are merchant-scoped to the request's merchant
	// (resolved from the delegated token's issuer + pinned above), so these never
	// expose another merchant's numbers — cross-merchant aggregation is the separate
	// platform-superadmin path, not reachable here.
	// #528: one folded metrics endpoint (was /metrics/{summary,revenue,subscriptions,
	// processors,churn}); ?period/?currency/?granularity control the shape.
	group.GET("/metrics", metricsRead, wrap(httphandlers.GetAdminMetrics))

	group.GET("/merchant-configuration", configRead, wrap(httphandlers.ServiceGetMerchantConfiguration))
	group.PUT("/merchant-configuration", configWrite, wrap(httphandlers.ServiceSetMerchantConfiguration))

	secretGroup := group.Group("/secrets")
	secretGroup.GET("", secretsList, merchantSecretListHandler(rt))
	secretGroup.GET("/registry", secretsList, merchantSecretRegistryHandler())
	secretGroup.PUT("/*name", secretsWrite, merchantSecretPutHandler(rt))
	secretGroup.DELETE("/*name", secretsDelete, merchantSecretDeleteHandler(rt))
	secretGroup.POST("/validate/*name", secretsTest, merchantSecretValidateHandler(rt))

	// Merchant-wide operational lists. They reuse admin handlers, but the delegated
	// middleware pins the merchant before RLS-aware queries run.
	group.GET("/repair-alerts", read, wrap(httphandlers.GetAdminRepairAlerts))
	group.GET("/manual-rebill-attempts", read, wrap(httphandlers.GetAdminManualRebillAttempts))

	users := group.Group("/users/:user_id")

	// Reads — the merchant-admin billing:read capability.
	users.GET("", read, wrap(httphandlers.GetAdminUserBillingProfile))
	users.GET("/payments", read, wrap(httphandlers.GetAdminUserPayments))

	// Entitlement grants/revokes — entitlements:write.
	users.POST("/entitlements", entWrite, wrap(httphandlers.GrantAdminEntitlement))
	users.DELETE("/entitlements/:id", entWrite, wrap(httphandlers.RevokeAdminEntitlement))

	// Product access (ownership) grants/revokes — product-access:write (#250, #528).
	// A separate concept from entitlements: durable per-(user,product) ownership.
	users.POST("/product-access", productAccessWrite, wrap(httphandlers.GrantAdminProductAccess))
	users.DELETE("/product-access/:id", productAccessWrite, wrap(httphandlers.RevokeAdminProductAccess))

	// Off-channel payment recording — payments:write.
	users.POST("/payments/off-channel", payWrite, wrap(httphandlers.AdminCreateOffChannelPayment))
	users.DELETE("/payment-methods/:id", payWrite, wrap(httphandlers.DeleteAdminUserPaymentMethod))

	// Merchant-wide payments (#528: ported from the retired per-user surface).
	payments := group.Group("/payments")
	payments.GET("", read, wrap(httphandlers.GetAdminPayments))
	payments.GET("/:id", read, wrap(httphandlers.GetAdminPayment))
	payments.POST("/:id/refunds", payWrite, wrap(httphandlers.AdminRefundPayment))

	// Subscriptions: a merchant admin may cancel a subscription owned by a user in
	// its merchant. Subscriptions are addressed by id; the handler operates within
	// the pinned merchant, so a sub outside the merchant is unreachable (fail closed).
	subs := group.Group("/subscriptions")
	subs.GET("", subRead, wrap(httphandlers.GetAdminSubscriptions))
	subs.GET("/:id", subRead, wrap(httphandlers.GetAdminSubscription))
	subs.DELETE("/:id", subWrite, wrap(httphandlers.AdminCancelSubscription)) // #528: cancel = DELETE
}
