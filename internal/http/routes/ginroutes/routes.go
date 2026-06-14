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
func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, oatMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(oatMW)
	// Pin a tenant-scoped DB connection AFTER the service token resolves the tenant, so RLS
	// constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.TenantDBConn(rt.DB))
	}

	// #480/#481: federated delegated-token issuers are no longer an OpenRails-owned
	// registry (the tenant_delegated_issuers table was dropped). Standalone JWKS/
	// issuer trust is AuthKit's remote_application registry (#74); register issuers
	// via AuthKit, not an OpenRails route.

	// Always batch (#354): one issuer, many subjects, one query; a single
	// lookup is an array of one.
	group.POST("/tenant-subjects/by-external-subject/entitlements",
		ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead),
		wrap(httphandlers.ServiceGetExternalSubjectEntitlements),
	)

	tenantSubjects := group.Group("/tenant-subjects/:tenant_subject_id")
	tenantSubjects.GET("/entitlements", ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetMerchantSubjectEntitlements))

	users := group.Group("/users/:user_id")
	users.GET("/product-access", ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserProductAccess))

	actors := group.Group("/actors/:actor")
	actors.GET("/credits", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetActorCredits))

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
	// Cross-payer BATCH admission (#335): N admit items (mixed payers) in one
	// hop, per-item verdicts with single-admit semantics. Same spend gates.
	group.POST("/admit/batch", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAdmitBatch))
	// Budget introspection (#304): fixed money-budget windows for a host /status.
	group.GET("/budget", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetBudget))
	// #410: budget-window status against caller-supplied windows (host owns the
	// policy, OpenRails owns the actuals) — powers the tensorhub delegated display.
	group.POST("/budget/check", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceBudgetCheck))
	// Tier policy admin (#298): configure a tier's throughput + entitled endpoints + money budgets.
	group.PUT("/tier-policies", creditsWrite, wrap(httphandlers.ServiceSetTierPolicy))
	// Tier SCHEDULE admin (#476): declare the cumulative-spend ladder ONCE; OpenRails
	// then auto-maintains each payer's tier (no host cranking of GraduateTier).
	group.PUT("/tier-schedules", creditsWrite, wrap(httphandlers.ServiceSetTierSchedule))
	// Graduated-tier READ (#477): the payer's current auto-maintained tier, for a
	// host that drives its OWN per-tier capacity (e.g. tensorhub's scheduler cap).
	group.GET("/tier", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetTier))
	// Hierarchical budget-scope policies (#473). Subject-owned caps (self +
	// (subject, role) pools) are written/read with the same operator credits
	// gates as tier policies. The PLATFORM-owned payer cap is operator-admin
	// gated (openrails:admin) — a subject's own surface can never reach it.
	budgetPolicies := group.Group("/budget-policies")
	budgetPolicies.PUT("/subject", creditsWrite, wrap(httphandlers.ServiceSetSubjectBudgetPolicy))
	budgetPolicies.GET("/subject", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetSubjectBudgetPolicies))
	budgetPolicies.PUT("/platform", ginmw.RequireServiceTokenPermission(controlplane.PermAdmin), wrap(httphandlers.ServiceSetPlatformBudgetPolicy))

	// Prepaid credit windows (#335): open reserves a bulk hold the host admits
	// against locally; settle flushes cross-payer batches of actuals (idempotent
	// per request_id); refill extends funds+TTL; close releases the remainder.
	// Open/settle/refill draw down payer funds -> write+spend (the /admit gates);
	// close releases a remainder (un-bills) -> write only, like hold release.
	credits.POST("/windows", creditsWrite, creditsSpend, wrap(httphandlers.ServiceOpenCreditWindow))
	credits.POST("/settle", creditsWrite, creditsSpend, wrap(httphandlers.ServiceSettleCreditWindows))
	credits.POST("/windows/:id/refill", creditsWrite, creditsSpend, wrap(httphandlers.ServiceRefillCreditWindow))
	credits.POST("/windows/:id/close", creditsWrite, wrap(httphandlers.ServiceCloseCreditWindow))

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
	// #410: per-resource daily revenue (by the usage_event resource column, cross-payer).
	credits.POST("/usage/resource-revenue", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceResourceRevenue))
	credits.GET("/transactions/lookup", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceLookupCreditTransaction))
	credits.GET("/actors/:actor", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetActorCredits))

	// Tenant billing-account admin surface (issue #242): configure prepaid|arrears
	// mode + spend caps + auto-top-up, read settings, and list usage. Tensorhub's
	// billing-admin proxies to these with its service service token; OpenRails owns the model.
	creditsRead := ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead)
	credits.PUT("/account-settings", creditsWrite, wrap(httphandlers.ServiceSetCreditAccountSettings))
	credits.GET("/account-settings", creditsRead, wrap(httphandlers.ServiceGetCreditAccountSettings))
	credits.GET("/transactions", creditsRead, wrap(httphandlers.ServiceListMerchantSubjectCreditTransactions))
	// #472: credit-type definition CRUD removed — money has no credit_type dimension.
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

	// Credit ACCOUNT surface (issue #339 gap-fill): the caller's OWN account
	// settings + balance/outstanding, settings writes, and transaction history.
	// The subject is ALWAYS the delegated principal's subject — no
	// tenant_subject_id parameter exists on this surface. Settings writes are
	// gated by the dedicated self billing:write permission so a read-only token
	// can inspect but never reconfigure.
	billingWrite := ginmw.RequireDelegatedPermission(controlplane.PermSelfBillingWrite)
	group.GET("/account", read, wrap(httphandlers.GetMyCreditAccount))
	group.PUT("/account/settings", billingWrite, wrap(httphandlers.SetMyCreditAccountSettings))
	group.GET("/account/transactions", read, wrap(httphandlers.GetMyAccountTransactions))

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

	walletManage := ginmw.RequireDelegatedPermission(controlplane.PermSelfWallets)
	wallets := group.Group("/wallets")
	wallets.GET("/solana", read, wrap(httphandlers.GetMySolanaWallet))
	wallets.PUT("/solana", walletManage, wrap(httphandlers.UpsertMySolanaWallet))
	wallets.DELETE("/solana", walletManage, wrap(httphandlers.DeleteMySolanaWallet))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkoutCreate := ginmw.RequireDelegatedPermission(controlplane.PermSelfCheckoutCreate)
	checkout := group.Group("/checkout")
	checkout.POST("", checkoutCreate, wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", read, wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", checkoutCreate, wrap(httphandlers.ConfirmCheckoutSession))

	// USDC funding handoff: external providers fund the user's live wallet, then
	// the existing checkout path collects from that wallet after balance checks.
	group.GET("/usdc-funding-options", read, wrap(httphandlers.GetUSDCFundingOptions))
	funding := group.Group("/usdc-funding-sessions")
	funding.POST("", checkoutCreate, wrap(httphandlers.CreateUSDCFundingSession))
	funding.GET("/:id", read, wrap(httphandlers.GetUSDCFundingSession))
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
//     fields like EntitlementGrant.GrantedBy record the acting admin),
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
	secretsList := ginmw.RequireDelegatedPermission(controlplane.PermTenantSecretsList)
	secretsWrite := ginmw.RequireDelegatedPermission(controlplane.PermTenantSecretsWrite)
	secretsDelete := ginmw.RequireDelegatedPermission(controlplane.PermTenantSecretsDelete)
	secretsTest := ginmw.RequireDelegatedPermission(controlplane.PermTenantSecretsTest)

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

	secretGroup := group.Group("/secrets")
	secretGroup.GET("", secretsList, tenantSecretListHandler(rt))
	secretGroup.GET("/registry", secretsList, tenantSecretRegistryHandler())
	secretGroup.PUT("/*name", secretsWrite, tenantSecretPutHandler(rt))
	secretGroup.DELETE("/*name", secretsDelete, tenantSecretDeleteHandler(rt))
	secretGroup.POST("/validate/*name", secretsTest, tenantSecretValidateHandler(rt))

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
	group.POST("/payments/:id/refund", payWrite, wrap(httphandlers.AdminRefundPayment))

	// Subscriptions: a tenant admin may cancel a subscription owned by a user in
	// its tenant. Subscriptions are addressed by id; the handler operates within
	// the pinned tenant, so a sub outside the tenant is unreachable (fail closed).
	subs := group.Group("/subscriptions")
	subs.GET("", subRead, wrap(httphandlers.GetAdminSubscriptions))
	subs.GET("/:id", subRead, wrap(httphandlers.GetAdminSubscription))
	subs.POST("/:id/cancel", subWrite, wrap(httphandlers.AdminCancelSubscription))
}
