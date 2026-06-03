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

// SelfRoutePrefix is the path under the tenant-scoped public API where
// browser-direct SELF-SERVICE billing operations live (issue #222 browser
// tier). A tenant's host frontend mints a short-lived DELEGATED ACCESS TOKEN for
// the logged-in end-user and the browser presents it as a Bearer token directly
// against this surface. The acting user is the token's `delegated_sub` and the
// acting tenant is bound by the token's `tenant` claim — never the URL.
const SelfRoutePrefix = "/self"

// TenantAdminRoutePrefix is the path under the tenant-scoped public API where
// browser-direct TENANT-ADMIN billing operations live (issue #259). A tenant's
// host frontend mints a FEDERATED, TENANT-SIGNED delegated access token carrying
// `openrails:tenant:*` permissions for one of its ADMIN users; the browser
// presents it as a Bearer token directly against this surface to act on ANY user
// WITHIN the token's tenant via the `:user_id` path param. The acting admin is
// the token's `delegated_sub` (recorded for audit) and the acting tenant is
// pinned from the token's validated issuer — never the URL.
const TenantAdminRoutePrefix = "/tenant-admin"

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

	// Pin a tenant-scoped DB connection for the request (tenant resolved by the
	// global ResolveTenant middleware) so RLS constrains tenant-owned queries
	// (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(middleware.TenantDBConn(rt.DB))
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

	// Solana recurring enrollment (#255): the wallet signs subscribe client-side,
	// then confirms here to charge the first cycle + create the membership.
	group.POST("/solana/recurring/enroll", wrap(httphandlers.ConfirmSolanaEnrollment))

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
	me.GET("/products", wrap(httphandlers.GetMyProducts))
	me.GET("/products/:product_id/access", wrap(httphandlers.GetMyProductAccess))

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
	// Pin a tenant-scoped DB connection for the request so RLS constrains
	// tenant-owned admin queries (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(middleware.TenantDBConn(rt.DB))
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

	// Stripe-shaped entitlement features (issue #245): features CRUD,
	// product-feature attachment, active-entitlements read. Operator-admin gated.
	RegisterEntitlementFeatureRoutes(group, rt)

	// Solana recurring plans — admin surface (#254). Create-only: on-chain plan
	// terms are immutable, so editing core terms = sunset + publish a new plan.
	group.POST("/solana/recurring/plans", wrap(httphandlers.AdminPublishSolanaPlan))

	// Product access grants — admin surface (issue #250).
	adminAccess := group.Group("/users/:user_id/product-access")
	adminAccess.GET("", wrap(httphandlers.GetAdminUserProductAccess))
	adminAccess.POST("", wrap(httphandlers.GrantAdminProductAccess))
	adminAccess.DELETE("/:id", wrap(httphandlers.RevokeAdminProductAccess))
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
func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, oatMW gin.HandlerFunc, minter DelegatedMinter, issuerAdmin DelegatedIssuerAdmin) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(oatMW)
	// Pin a tenant-scoped DB connection AFTER the OAT resolves the tenant, so RLS
	// constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(middleware.TenantDBConn(rt.DB))
	}

	// Browser-tier delegated-token MINT (issue #222). A host backend authenticated
	// by an OAT holding PermSelfMint asks OpenRails to mint a short-lived,
	// user-scoped delegated access token (for its OWN tenant) to hand to a browser
	// for the /v1/self/* surface. Mounted only when a minter is wired (control
	// plane present); the OAT gate + per-route permission keep it server-to-server.
	//
	// DEPRECATED (issue #259): the federated/tenant-signed tier lets hosts mint
	// their OWN aud=openrails tokens (signed with their key, verified via JWKS),
	// removing this round-trip. Kept during the dual-trust migration window; it is
	// retired once all tenants self-sign (see the issuer-management routes below).
	if minter != nil {
		group.POST("/delegated-tokens",
			middleware.RequireOATPermission(controlplane.PermSelfMint),
			mintDelegatedTokenHandler(minter),
		)
	}

	// FEDERATED issuer management (issue #259). The OAT remains the ROOT
	// tenant-management credential: a host backend authenticated by an OAT holding
	// PermAdmin for its tenant registers/rotates/disables the issuer + JWKS URL it
	// self-signs aud=openrails browser tokens with. The tenant is bound from the
	// OAT (never the body), so a caller can only manage its OWN tenant's issuers.
	if issuerAdmin != nil {
		issuers := group.Group("/tenant/issuers")
		issuers.POST("", middleware.RequireOATPermission(controlplane.PermAdmin), registerDelegatedIssuerHandler(issuerAdmin))
		issuers.GET("", middleware.RequireOATPermission(controlplane.PermAdmin), listDelegatedIssuersHandler(issuerAdmin))
		issuers.POST("/disable", middleware.RequireOATPermission(controlplane.PermAdmin), disableDelegatedIssuerHandler(issuerAdmin))
	}

	users := group.Group("/users/:user_id")
	users.GET("/entitlements", middleware.RequireOATPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserEntitlements))
	users.GET("/credits", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetUserCredits))
	users.GET("/product-access", middleware.RequireOATPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserProductAccess))

	credits := group.Group("/credits")
	// SPEND (hot-path billing) operations — authorize/hold/capture draw down a
	// payer's balance, so they require BOTH the coarse credits:write operator
	// capability AND the explicit billing:spend (credits:spend) "may you bill this
	// payer" gate (issue #246). The two are checked in sequence; PermAdmin
	// satisfies either. Release returns a remainder (un-bills), so it needs write
	// but not spend.
	creditsSpend := middleware.RequireOATPermission(controlplane.PermCreditsSpend)
	creditsWrite := middleware.RequireOATPermission(controlplane.PermCreditsWrite)

	// Unified authorize: policy decision + ATOMIC hold placement (issue #235/#247).
	credits.POST("/authorize", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAuthorizeCredits))
	// Payer balance snapshot (issue #235/#247): available = balance - held.
	credits.GET("/balance", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetCreditsBalance))

	credits.POST("/deposit", creditsWrite, wrap(httphandlers.ServiceDepositCredits))
	credits.POST("/withdraw", creditsWrite, wrap(httphandlers.ServiceWithdrawCredits))
	credits.POST("/hold", creditsWrite, creditsSpend, wrap(httphandlers.ServiceHoldCredits))
	credits.POST("/holds/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/holds/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	credits.POST("/hold/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/hold/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	credits.GET("/transactions/lookup", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceLookupCreditTransaction))
	credits.GET("/users/:user_id", middleware.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetUserCredits))

	// Org billing-account admin surface (issue #242): configure prepaid|arrears
	// mode + spend caps + auto-top-up, read settings, and list usage. Tensorhub's
	// billing-admin proxies to these with its service OAT; OpenRails owns the model.
	creditsRead := middleware.RequireOATPermission(controlplane.PermCreditsRead)
	credits.PUT("/account-settings", creditsWrite, wrap(httphandlers.ServiceSetCreditAccountSettings))
	credits.GET("/account-settings", creditsRead, wrap(httphandlers.ServiceGetCreditAccountSettings))
	credits.GET("/transactions", creditsRead, wrap(httphandlers.ServiceListOwnerCreditTransactions))

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

// RegisterSelfServiceRoutes mounts the browser-direct SELF-SERVICE billing
// surface on a tenant-scoped PUBLIC route group authenticated by a browser's
// DELEGATED ACCESS TOKEN (issue #222 browser-tier foundation). It reuses the
// existing user-facing handlers — they all read the acting user via
// r.GetUser(), which delegatedMW binds to the token's `delegated_sub` — so every
// operation is automatically scoped to the authenticated end-user and their
// tenant. There is no `:user_id` in any path: a browser token can only act on
// its own subject.
//
// delegatedMW must authenticate the delegated token (typically
// middleware.DelegatedSelfRequired); it pins the resolved tenant + acting user +
// self-permissions onto the context that the per-route RequireDelegatedPermission
// gates and the handlers read.
func RegisterSelfServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a tenant-scoped DB connection AFTER the delegated token resolves the
	// tenant, so RLS constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(middleware.TenantDBConn(rt.DB))
	}

	read := middleware.RequireDelegatedPermission(controlplane.PermSelfBillingRead)

	// Account + balance/credits read.
	group.GET("/status", read, wrap(httphandlers.GetMyBillingStatus))
	group.GET("/credits", read, wrap(httphandlers.GetMyCredits))
	group.GET("/credits/:type", read, wrap(httphandlers.GetMyCreditsType))
	group.GET("/credits/:type/transactions", read, wrap(httphandlers.GetMyCreditTransactions))

	// Payment / transaction history.
	group.GET("/payments", read, wrap(httphandlers.GetUserPayments))
	group.GET("/entitlements/active", read, wrap(httphandlers.SelfGetActiveEntitlements))

	// Subscriptions: read for the whole group; mutations gated by scope.
	//
	// cancel and resume are two halves of the same reversible-cancellation
	// lifecycle (#215/#216), so resume is gated by the SAME cancel scope — a
	// browser that may cancel its own subscription may also undo that cancel.
	// change-tier is a subscription mutation; it also rides the cancel scope as
	// the subscription-management capability for self tokens. The acting user is
	// the token's delegated_sub (bound by DelegatedSelfRequired), so every op is
	// scoped to that user's own subscription. payment-method update is a
	// payment-method mutation -> the payment-methods:manage scope.
	subscriptionManage := middleware.RequireDelegatedPermission(controlplane.PermSelfSubscriptionCancel)
	manage := middleware.RequireDelegatedPermission(controlplane.PermSelfPaymentMethods)
	subs := group.Group("/subscriptions")
	subs.GET("", read, wrap(httphandlers.GetMySubscriptions))
	subs.GET("/:id", read, wrap(httphandlers.GetSubscription))
	subs.POST("/:id/cancel", subscriptionManage, wrap(httphandlers.CancelSubscription))
	subs.POST("/:id/resume", subscriptionManage, wrap(httphandlers.ResumeSubscription))
	subs.POST("/:id/change-tier", subscriptionManage, wrap(httphandlers.ChangeTier))
	subs.PUT("/:id/payment-method", manage, wrap(httphandlers.UpdateSubscriptionPaymentMethod))
	// App-driven on-chain cancel/revoke (#266/#271): the full prepare -> sign ->
	// confirm -> mirror loop. solana-cancel-tx builds the unsigned cancel tx the
	// wallet signs+sends; solana-cancel confirms the signature landed on-chain and
	// then mirrors the cancel into the DB (stops the cranker). Solana is the source
	// of truth — there is no DB-only "soft cancel". Both gated by the cancel scope.
	subs.POST("/:id/solana-cancel-tx", subscriptionManage, wrap(httphandlers.PrepareSolanaCancelTx))
	subs.POST("/:id/solana-cancel", subscriptionManage, wrap(httphandlers.ConfirmSolanaCancel))

	// Payment methods: list is a read; mutations require the manage scope.
	pm := group.Group("/payment-methods")
	pm.GET("", read, wrap(httphandlers.ListPaymentMethods))
	pm.POST("", manage, wrap(httphandlers.CreatePaymentMethod))
	pm.PUT("/:id", manage, wrap(httphandlers.UpdatePaymentMethod))
	pm.DELETE("/:id", manage, wrap(httphandlers.DeletePaymentMethod))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkoutCreate := middleware.RequireDelegatedPermission(controlplane.PermSelfCheckoutCreate)
	checkout := group.Group("/checkout")
	checkout.POST("", checkoutCreate, wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", read, wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", checkoutCreate, wrap(httphandlers.ConfirmCheckoutSession))
}

// RegisterTenantAdminRoutes mounts the browser-direct TENANT-ADMIN billing
// surface on a tenant-scoped PUBLIC route group authenticated by a FEDERATED,
// TENANT-SIGNED delegated access token carrying `openrails:tenant:*` permissions
// (issue #259). Unlike the self-service surface (which has no `:user_id` and acts
// only on the token's own subject), these routes act on ANY user WITHIN the
// token's tenant via the `:user_id` path param.
//
// It REUSES the existing operator admin handlers unchanged. They are safe here
// because:
//   - the target user is the `:user_id` path param,
//   - the acting admin is r.GetUser() == the token's `delegated_sub` (so audit
//     fields like AdminGrant.GrantedBy record the acting admin),
//   - the tenant is pinned onto the request context by delegatedMW from the
//     token's validated issuer, so every tenant-owned query is scoped to that
//     tenant (RLS-enforced, #227). A `:user_id` belonging to another tenant is
//     therefore unreachable — fail closed, no cross-tenant access.
//
// delegatedMW must authenticate the delegated token (middleware.DelegatedSelfRequired);
// it pins the resolved tenant + acting admin + tenant-permissions onto the
// context that the per-route RequireDelegatedPermission gates and handlers read.
func RegisterTenantAdminRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a tenant-scoped DB connection AFTER the delegated token resolves the
	// tenant, so RLS constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(middleware.TenantDBConn(rt.DB))
	}

	read := middleware.RequireDelegatedPermission(controlplane.PermTenantBillingRead)
	entWrite := middleware.RequireDelegatedPermission(controlplane.PermTenantEntitlementsWrite)
	payWrite := middleware.RequireDelegatedPermission(controlplane.PermTenantPaymentsWrite)
	subWrite := middleware.RequireDelegatedPermission(controlplane.PermTenantSubscriptionsWrite)
	metricsRead := middleware.RequireDelegatedPermission(controlplane.PermTenantMetricsRead)

	// Tenant metrics (issue #259 + #232): a tenant admin reads THEIR OWN tenant's
	// analytics. The metrics queries are tenant-scoped to the request's tenant
	// (resolved from the delegated token's issuer + pinned above), so these never
	// expose another tenant's numbers — cross-tenant aggregation is the separate
	// platform-superadmin path, not reachable here.
	metrics := group.Group("/metrics")
	metrics.GET("/summary", metricsRead, wrap(httphandlers.GetAdminMetricsSummary))
	metrics.GET("/revenue", metricsRead, wrap(httphandlers.GetAdminMetricsRevenue))
	metrics.GET("/subscriptions", metricsRead, wrap(httphandlers.GetAdminMetricsSubscriptions))
	metrics.GET("/processors", metricsRead, wrap(httphandlers.GetAdminMetricsProcessors))
	metrics.GET("/churn", metricsRead, wrap(httphandlers.GetAdminMetricsChurn))

	users := group.Group("/users/:user_id")

	// Reads — the tenant-admin billing:read capability.
	users.GET("", read, wrap(httphandlers.GetAdminUserBillingProfile))
	users.GET("/payments", read, wrap(httphandlers.GetAdminUserPayments))
	users.GET("/entitlements", read, wrap(httphandlers.GetAdminUserEntitlements))
	users.GET("/nmi", read, wrap(httphandlers.GetAdminUserNMI))
	users.GET("/nmi/metrics", read, wrap(httphandlers.GetAdminUserNMIMetrics))
	users.GET("/ccbill", read, wrap(httphandlers.GetAdminUserCCBill))
	users.GET("/ccbill/metrics", read, wrap(httphandlers.GetAdminUserCCBillMetrics))

	// Entitlement grants/revokes — entitlements:write.
	users.POST("/entitlements", entWrite, wrap(httphandlers.GrantAdminEntitlement))
	users.DELETE("/entitlements/:id", entWrite, wrap(httphandlers.RevokeAdminEntitlement))

	// Off-channel payment recording — payments:write.
	users.POST("/payments/off-channel", payWrite, wrap(httphandlers.AdminCreateOffChannelPayment))

	// Subscriptions: a tenant admin may cancel a subscription owned by a user in
	// its tenant. Subscriptions are addressed by id; the handler operates within
	// the pinned tenant, so a sub outside the tenant is unreachable (fail closed).
	subs := group.Group("/subscriptions")
	subs.POST("/:id/cancel", subWrite, wrap(httphandlers.AdminCancelSubscription))
}
