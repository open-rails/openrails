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

// ServiceRoutePrefix is the path under the tenant-scoped public API where
// service token-authenticated server-to-server billing operations live (issue #222). It
// REPLACES the retired private/mTLS service listener: machine callers present an
// OpenRails-issued tenant service token as a Bearer token against this public surface. The
// acting tenant is bound by the service token's owning tenant, not the URL.
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

func wrapHandler(rt *app.Runtime, fn func(r *httprequest.Request)) gin.HandlerFunc {
	return func(c *gin.Context) {
		fn(ginreq.New(c, rt))
	}
}

// RegisterServiceRoutes mounts the server-to-server billing surface on a
// tenant-scoped PUBLIC route group authenticated by OpenRails-issued tenant service tokens
// (issue #222). This replaces the retired private/mTLS listener and its
// certificate-scope model: every operation is gated by a service token permission from the
// canonical colon-form OpenRails permission catalog, and the acting tenant is the
// service token's owning tenant (pinned by ServiceTokenRequired before any tenant-owned DB access).
//
// oatMW must authenticate the service token (typically ginmw.ServiceTokenRequired); it pins the
// resolved tenant + permissions onto the context that the per-route
// RequireServiceTokenPermission gates and the handlers read.
func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, oatMW gin.HandlerFunc, minter DelegatedMinter, issuerAdmin DelegatedIssuerAdmin) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(oatMW)
	// Pin a tenant-scoped DB connection AFTER the service token resolves the tenant, so RLS
	// constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.TenantDBConn(rt.DB))
	}

	// Browser-tier delegated-token MINT (issue #222). A host backend authenticated
	// by a service token holding PermSelfMint asks OpenRails to mint a short-lived,
	// user-scoped delegated access token (for its OWN tenant) to hand to a browser
	// for the /v1/self/* surface. Mounted only when a minter is wired (control
	// plane present); the service token gate + per-route permission keep it server-to-server.
	//
	// DEPRECATED (issue #259): the federated/tenant-signed tier lets hosts mint
	// their OWN aud=openrails tokens (signed with their key, verified via JWKS),
	// removing this round-trip. Kept during the dual-trust migration window; it is
	// retired once all tenants self-sign (see the issuer-management routes below).
	if minter != nil {
		group.POST("/delegated-tokens",
			ginmw.RequireServiceTokenPermission(controlplane.PermSelfMint),
			mintDelegatedTokenHandler(minter),
		)
	}

	// FEDERATED issuer management (issue #259). The service token remains the ROOT
	// tenant-management credential: a host backend authenticated by a service token holding
	// PermAdmin for its tenant registers/rotates/disables the issuer + JWKS URL it
	// self-signs aud=openrails browser tokens with. The tenant is bound from the
	// service token (never the body), so a caller can only manage its OWN tenant's issuers.
	if issuerAdmin != nil {
		issuers := group.Group("/tenant/issuers")
		issuers.POST("", ginmw.RequireServiceTokenPermission(controlplane.PermAdmin), registerDelegatedIssuerHandler(issuerAdmin))
		issuers.GET("", ginmw.RequireServiceTokenPermission(controlplane.PermAdmin), listDelegatedIssuersHandler(issuerAdmin))
		issuers.POST("/disable", ginmw.RequireServiceTokenPermission(controlplane.PermAdmin), disableDelegatedIssuerHandler(issuerAdmin))
	}

	group.GET("/tenant-subjects/by-external-subject/entitlements",
		ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead),
		wrap(httphandlers.ServiceGetExternalSubjectEntitlements),
	)

	tenantSubjects := group.Group("/tenant-subjects/:tenant_subject_id")
	tenantSubjects.GET("/entitlements", ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetTenantSubjectEntitlements))

	users := group.Group("/users/:user_id")
	users.GET("/product-access", ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserProductAccess))

	invokers := group.Group("/invokers/:invoker_id")
	invokers.GET("/credits", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetInvokerCredits))

	credits := group.Group("/credits")
	// SPEND (hot-path billing) operations — authorize/hold/capture draw down a
	// payer's balance, so they require BOTH the coarse credits:write operator
	// capability AND the explicit billing:spend (credits:spend) "may you bill this
	// payer" gate (issue #246). The two are checked in sequence; PermAdmin
	// satisfies either. Release returns a remainder (un-bills), so it needs write
	// but not spend.
	creditsSpend := ginmw.RequireServiceTokenPermission(controlplane.PermCreditsSpend)
	creditsWrite := ginmw.RequireServiceTokenPermission(controlplane.PermCreditsWrite)

	// Unified ADMISSION (issue #298): throughput (rate-limit) + money (hold) +
	// suspension + blocklist + endpoint gating in one call; emits x-ratelimit-*
	// + 429/Retry-After. Hot-path gate hosts call before doing work.
	group.POST("/admit", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAdmit))
	// Budget introspection (#304): rolling money-budget windows for a host /status.
	group.GET("/budget", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetBudget))
	// #410: budget-window status against caller-supplied windows (host owns the
	// policy, OpenRails owns the actuals) — powers the tensorhub delegated display.
	group.POST("/budget/check", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceBudgetCheck))
	// Tier policy admin (#298): configure a tier's throughput + entitled endpoints + money budgets.
	group.PUT("/tier-policies", creditsWrite, wrap(httphandlers.ServiceSetTierPolicy))

	// Unified authorize: policy decision + ATOMIC hold placement (issue #235/#247).
	credits.POST("/authorize", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAuthorizeCredits))
	// Payer balance snapshot (issue #235/#247): available = balance - held.
	credits.GET("/balance", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetCreditsBalance))

	credits.POST("/deposit", creditsWrite, wrap(httphandlers.ServiceDepositCredits))
	credits.POST("/withdraw", creditsWrite, wrap(httphandlers.ServiceWithdrawCredits))
	credits.POST("/hold", creditsWrite, creditsSpend, wrap(httphandlers.ServiceHoldCredits))
	credits.POST("/holds/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/holds/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	credits.POST("/hold/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/hold/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	// #311: per-dimension spend rollup for the platform usage/revenue surfaces.
	credits.POST("/usage/rollup", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceUsageRollup))
	// #410: per-endpoint daily revenue (by usage_event endpoint_name, cross-payer).
	credits.POST("/usage/endpoint-revenue", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceEndpointRevenue))
	credits.GET("/transactions/lookup", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceLookupCreditTransaction))
	credits.GET("/invokers/:invoker_id", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetInvokerCredits))

	// Tenant billing-account admin surface (issue #242): configure prepaid|arrears
	// mode + spend caps + auto-top-up, read settings, and list usage. Tensorhub's
	// billing-admin proxies to these with its service service token; OpenRails owns the model.
	creditsRead := ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead)
	credits.PUT("/account-settings", creditsWrite, wrap(httphandlers.ServiceSetCreditAccountSettings))
	credits.GET("/account-settings", creditsRead, wrap(httphandlers.ServiceGetCreditAccountSettings))
	credits.GET("/transactions", creditsRead, wrap(httphandlers.ServiceListTenantSubjectCreditTransactions))

	// Credit-type definition writes are catalog-definition operations: gate them
	// behind the explicit catalog-write permission (issue #222 — catalog/definition
	// writes available to service tokens only under an explicit permission). Reads use the
	// coarse credits:read capability.
	creditTypes := group.Group("/credit-types")
	creditTypes.POST("", ginmw.RequireServiceTokenPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceCreateCreditType))
	creditTypes.GET("", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceListCreditTypes))
	creditTypes.PATCH("/:name", ginmw.RequireServiceTokenPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceUpdateCreditType))
	creditTypes.POST("/:name/deactivate", ginmw.RequireServiceTokenPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceDeactivateCreditType))
	creditTypes.POST("/:name/activate", ginmw.RequireServiceTokenPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceActivateCreditType))
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
// ginmw.DelegatedSelfRequired); it pins the resolved tenant + acting user +
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
		group.Use(ginmw.TenantDBConn(rt.DB))
	}

	read := ginmw.RequireDelegatedPermission(controlplane.PermSelfBillingRead)

	// Account + balance/credits read.
	group.GET("/status", read, wrap(httphandlers.GetMyBillingStatus))
	group.GET("/credits", read, wrap(httphandlers.GetMyCredits))
	group.GET("/credits/:type", read, wrap(httphandlers.GetMyCreditsType))
	group.GET("/credits/:type/transactions", read, wrap(httphandlers.GetMyCreditTransactions))

	// Usage breakdown (issue #289): the acting user's metered usage rolled up by
	// event_type (endpoint/model) over a [from, to) window, with summed dimensions.
	// Scoped to the token's subject + tenant like every other self route.
	group.GET("/usage", read, wrap(httphandlers.GetMyUsage))

	// Invoices (issue #303): the acting user's finalized monthly itemized
	// statements (newest first, paginated) and a single invoice with its line
	// items. Scoped to the token's subject + tenant like every other self route.
	group.GET("/invoices", read, wrap(httphandlers.GetMyInvoices))
	group.GET("/invoices/:id", read, wrap(httphandlers.GetMyInvoice))

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
	subscriptionManage := ginmw.RequireDelegatedPermission(controlplane.PermSelfSubscriptionCancel)
	manage := ginmw.RequireDelegatedPermission(controlplane.PermSelfPaymentMethods)
	subs := group.Group("/subscriptions")
	subs.GET("", read, wrap(httphandlers.GetMySubscriptions))
	subs.GET("/:id", read, wrap(httphandlers.GetSubscription))
	subs.POST("/:id/cancel", subscriptionManage, wrap(httphandlers.CancelSubscription))
	subs.POST("/:id/resume", subscriptionManage, wrap(httphandlers.ResumeSubscription))
	subs.POST("/:id/change-tier", subscriptionManage, wrap(httphandlers.ChangeTier))
	subs.POST("/:id/change-tier/preview", subscriptionManage, wrap(httphandlers.ChangeTierPreview))
	subs.PUT("/:id/payment-method", manage, wrap(httphandlers.UpdateSubscriptionPaymentMethod))
	// App-driven on-chain cancel/revoke (#266/#271): the full prepare -> sign ->
	// confirm -> mirror loop. solana-cancel-tx builds the unsigned cancel tx the
	// wallet signs+sends; solana-cancel confirms the signature landed on-chain and
	// then mirrors the cancel into the DB (stops the cranker). Solana is the source
	// of truth — there is no DB-only "soft cancel". Both gated by the cancel scope.
	subs.POST("/:id/solana-cancel-tx", subscriptionManage, wrap(httphandlers.PrepareSolanaCancelTx))
	subs.POST("/:id/solana-cancel", subscriptionManage, wrap(httphandlers.ConfirmSolanaCancel))
	// App-driven on-chain tier change (#272): the prepare -> sign -> confirm ->
	// mirror loop for changing tier on an existing Solana subscription. prepare
	// returns the SINGLE ATOMIC cancel-old+subscribe-new tx (co-signed for an
	// upgrade's prorated transfer); confirm verifies it landed on-chain and mirrors
	// the switch into the DB (old cancelled, new active, next_pull_at per kind).
	// Both ride the subscription-management (cancel) scope.
	subs.POST("/:id/solana-tier-change", subscriptionManage, wrap(httphandlers.PrepareSolanaTierChange))
	subs.POST("/:id/solana-tier-change/confirm", subscriptionManage, wrap(httphandlers.ConfirmSolanaTierChange))

	// Payment methods: list is a read; mutations require the manage scope.
	pm := group.Group("/payment-methods")
	pm.GET("", read, wrap(httphandlers.ListPaymentMethods))
	pm.POST("", manage, wrap(httphandlers.CreatePaymentMethod))
	pm.PUT("/:id", manage, wrap(httphandlers.UpdatePaymentMethod))
	pm.DELETE("/:id", manage, wrap(httphandlers.DeletePaymentMethod))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkoutCreate := ginmw.RequireDelegatedPermission(controlplane.PermSelfCheckoutCreate)
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
// delegatedMW must authenticate the delegated token (ginmw.DelegatedSelfRequired);
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
		group.Use(ginmw.TenantDBConn(rt.DB))
	}

	read := ginmw.RequireDelegatedPermission(controlplane.PermTenantBillingRead)
	subRead := ginmw.RequireDelegatedPermission(controlplane.PermTenantBillingRead)
	entWrite := ginmw.RequireDelegatedPermission(controlplane.PermTenantEntitlementsWrite)
	payWrite := ginmw.RequireDelegatedPermission(controlplane.PermTenantPaymentsWrite)
	subWrite := ginmw.RequireDelegatedPermission(controlplane.PermTenantSubscriptionsWrite)
	metricsRead := ginmw.RequireDelegatedPermission(controlplane.PermTenantMetricsRead)

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

	// Tenant-wide operational lists. They reuse admin handlers, but the delegated
	// middleware pins the tenant before RLS-aware queries run.
	group.GET("/repair-alerts", read, wrap(httphandlers.GetAdminRepairAlerts))
	group.GET("/manual-rebill-attempts", read, wrap(httphandlers.GetAdminManualRebillAttempts))

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
	subs.GET("", subRead, wrap(httphandlers.GetAdminSubscriptions))
	subs.GET("/:id", subRead, wrap(httphandlers.GetAdminSubscription))
	subs.POST("/:id/cancel", subWrite, wrap(httphandlers.AdminCancelSubscription))
}
